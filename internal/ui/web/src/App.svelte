<script lang="ts">
  import { onMount } from "svelte";
  import type { UIState, Profile } from "./types";
  import { get, fail, notice, revision, act, targetLabel } from "./api";
  import Overview from "./Overview.svelte";
  import Requests from "./Requests.svelte";
  import Routes from "./Routes.svelte";
  import Connections from "./Connections.svelte";
  let data = $state<UIState | null>(null);
  let screen = $state("overview");
  let offline = $state(false);
  let loading = $state(true);
  let theme = $state("system");
  let profile = $state<Profile | null>(null);
  let profileDialog: HTMLDialogElement;
  let switching = $state(false);
  let refreshing = false;
  const tabs = [
    ["overview", "Обзор"],
    ["requests", "Запросы"],
    ["routes", "Маршруты"],
    ["connections", "Подключения"],
  ];
  function route() {
    const value = location.hash.replace(/^#\//, "").split("?")[0];
    screen = tabs.some((t) => t[0] === value)
      ? value
      : tabs.some((t) => "/" + t[0] === location.pathname)
        ? location.pathname.slice(1)
        : location.pathname === "/settings"
          ? "connections"
          : "overview";
  }
  function setTheme(value: string) {
    theme = value;
    document.documentElement.dataset.theme = value;
    try {
      localStorage.setItem("router-theme", value);
    } catch {
      /* Storage may be disabled by the browser. */
    }
  }
  async function refresh() {
    if (refreshing) return;
    refreshing = true;
    try {
      data = await get<UIState>("/api/ui/state");
      offline = false;
    } catch (e) {
      offline = true;
      if (!data) fail(e);
    } finally {
      loading = false;
      refreshing = false;
    }
  }
  function preview(name: string) {
    profile = data?.profiles.find((p) => p.name === name) || null;
    if (profile) profileDialog.showModal();
  }
  function profileRoutes(p: Profile): [string, string][] {
    return Object.entries({ ...p.familyRoutes, ...p.routes }).flatMap(
      ([model, efforts]) =>
        Object.entries(efforts || {}).map(
          ([effort, value]): [string, string] => [
            model + " / " + effort,
            value.mode === "model"
              ? (value.model || "") + (value.effort ? " · " + value.effort : "")
              : value.mode === "pool"
                ? "Пул · " + value.pool
                : value.mode === "anthropic"
                  ? "Anthropic"
                  : value.mode,
          ],
        ),
    );
  }
  async function activate() {
    if (!profile) return;
    switching = true;
    try {
      await act("profile.activate", { name: profile.name });
      profileDialog.close();
    } catch (e) {
      fail(e);
    } finally {
      switching = false;
    }
  }
  onMount(() => {
    route();
    try {
      const saved = localStorage.getItem("router-theme");
      if (saved && ["light", "dark", "system"].includes(saved)) setTheme(saved);
    } catch {
      /* Default to system theme. */
    }
    void refresh();
    window.addEventListener("hashchange", route);
    const events = new EventSource("/api/ui/events");
    events.addEventListener("refresh", () => void refresh());
    events.onerror = () => {
      offline = true;
    };
    events.onopen = () => {
      offline = false;
    };
    const unsubscribe = revision.subscribe(() => void refresh());
    const poll = window.setInterval(() => {
      if (document.visibilityState === "visible") void refresh();
    }, 15000);
    return () => {
      window.removeEventListener("hashchange", route);
      events.close();
      unsubscribe();
      clearInterval(poll);
    };
  });
</script>

<a class="skip-link" href="#main">К содержимому</a>
<header id="bar">
  <div class="bar-in">
    <a class="brand" href="#/overview" aria-label="Роутер — обзор"
      ><svg width="30" height="30" viewBox="0 0 32 32" aria-hidden="true"
        ><path
          d="M8 8h9v16h7"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
        /><circle cx="8" cy="8" r="4" /><circle cx="24" cy="24" r="4" /></svg
      >Роутер</a
    >
    <nav class="tabs" aria-label="Основная навигация">
      {#each tabs as [key, label]}<a
          class="tab"
          href={"#/" + key}
          aria-current={screen === key ? "page" : undefined}>{label}</a
        >{/each}
    </nav>
    <div class="header-tools">
      <label class="theme-control"
        ><span class="sr">Цветовая тема</span><select
          aria-label="Цветовая тема"
          value={theme}
          onchange={(e) => setTheme(e.currentTarget.value)}
          ><option value="system">◐ Системная</option><option value="light"
            >☀ Светлая</option
          ><option value="dark">☾ Тёмная</option></select
        ></label
      >
    </div>
  </div>
</header>
<main id="main" tabindex="-1">
  {#if data}<div class="workspace-strip">
      <div class="profile-chips">
        <span class="muted">Профиль</span>{#each data.profiles as p}<button
            class:active={p.name === data.activeProfile}
            disabled={p.name === data.activeProfile}
            onclick={() => preview(p.name)}
            >{p.name}{p.name === data.activeProfile ? " ✓" : ""}</button
          >{/each}
      </div>
      <span class="live-state" class:danger={offline}
        ><i class="status-dot"></i>{offline
          ? "Связь прервана · повторяем"
          : "На связи"}</span
      >
    </div>{/if}{#if offline}<div class="alert danger" role="alert">
      Не удаётся обновить данные. Показано последнее полученное состояние. <button
        onclick={() => void refresh()}>Повторить</button
      >
    </div>{/if}{#if $notice}<div
      class="notice"
      class:danger={$notice.error}
      role={$notice.error ? "alert" : "status"}
    >
      <span>{$notice.message}</span><button
        class="text-button"
        aria-label="Закрыть уведомление"
        onclick={() => notice.set(null)}>✕</button
      >
    </div>{/if}{#if loading}<div class="empty">
      <h1>Подключаемся к роутеру…</h1>
      <p>Получаем состояние и маршруты.</p>
    </div>{:else if data}{#each data.reloadErrors as error}<p
        class="alert danger"
      >
        {error}
      </p>{/each}{#if screen === "requests"}<Requests
        {data}
      />{:else if screen === "routes"}<Routes
        {data}
      />{:else if screen === "connections"}<Connections
        {data}
      />{:else}<Overview {data} />{/if}{:else}<div class="empty">
      <h1>Роутер недоступен</h1>
      <p>Проверьте, что процесс запущен, и повторите подключение.</p>
      <button class="primary" onclick={() => void refresh()}>Повторить</button>
    </div>{/if}
  <footer>Роутер <span>Локальный пульт управления</span></footer>
</main>
<dialog bind:this={profileDialog} aria-label="Переключение профиля">
  <div class="spread">
    <h2>Переключить профиль</h2>
    <button
      onclick={() => profileDialog.close()}
      aria-label="Закрыть предпросмотр">✕</button
    >
  </div>
  {#if profile}<p class="subtitle">
      {data?.activeProfile} → <strong>{profile.name}</strong>
    </p>
    <p>Новые запросы будут использовать следующие маршруты:</p>
    <div class="profile-preview">
      {#each profileRoutes(profile) as [key, value]}<div class="spread">
          <span>{key}</span><strong>{targetLabel(value)}</strong>
        </div>{/each}{#if !profileRoutes(profile).length}<p>
          В этом профиле маршруты не настроены.
        </p>{/if}
    </div>
    <p class="help">
      Пулы профиля: {Object.keys(profile.modelPools || {}).join(", ") || "нет"}.
      Уже выполняющиеся запросы завершатся по прежним маршрутам.
    </p>
    <div class="actions">
      <button onclick={() => profileDialog.close()}>Отмена</button><button
        class="primary"
        disabled={switching}
        onclick={activate}
        >{switching ? "Переключаем…" : "Активировать " + profile.name}</button
      >
    </div>{/if}
</dialog>
