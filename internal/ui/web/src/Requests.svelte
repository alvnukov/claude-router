<script lang="ts">
  import { onMount, untrack } from "svelte";
  import type { UIState, RequestList, RequestDetail } from "./types";
  import { get, fail, time, revision } from "./api";
  import ActionForm from "./ActionForm.svelte";
  import Transcript from "./Transcript.svelte";
  import SessionIdentity from "./SessionIdentity.svelte";
  import { sessionOption } from "./session";
  import ConnectionUsage from "./ConnectionUsage.svelte";
  let { data }: { data: UIState } = $props();
  const sessionMap = $derived(new Map(data.sessions.map(s => [s.id, s])));
  let params = new URLSearchParams(location.hash.split("?")[1] || "");
  let q = $state(params.get("q") || "");
  let model = $state(params.get("model") || "");
  let connection = $state(params.get("connection") || "");
  let errors = $state(params.get("errors") === "1");
  let unrecognized = $state(params.get("unrecognized") === "1");
  let session = $state(params.get("session") || "");
  let offset = $state(0);
  let list = $state<RequestList>({ items: [], total: 0, offset: 0, limit: 50 });
  let loading = $state(false);
  let detail = $state<RequestDetail | null>(null);
  let detailLoading = $state(false);
  let tab = $state("response");
  let dialog: HTMLDialogElement;
  let generation = 0;
  let activeQuery = "";
  async function refresh(invalidate = false) {
    const p = new URLSearchParams({
      q,
      model,
      connection,
      session,
      offset: String(offset),
      limit: "50",
    });
    if (errors) p.set("errors", "1");
    if (unrecognized) p.set("unrecognized", "1");
    const query = p.toString();
    // State updates must not invalidate an identical request still in flight.
    if (!invalidate && loading && activeQuery === query) return;
    const gen = ++generation;
    activeQuery = query;
    loading = true;
    try {
      const result = await get<RequestList>("/api/ui/requests?" + query);
      if (gen === generation) list = result;
    } catch (e) {
      if (gen === generation) fail(e);
    } finally {
      if (gen === generation) loading = false;
    }
  }
  function filter(event?: SubmitEvent) {
    event?.preventDefault();
    offset = 0;
    list = { ...list, sessionUsage: undefined };
    const p = new URLSearchParams();
    if (q) p.set("q", q);
    if (model) p.set("model", model);
    if (connection) p.set("connection", connection);
    if (session) p.set("session", session);
    if (errors) p.set("errors", "1");
    if (unrecognized) p.set("unrecognized", "1");
    history.replaceState(null, "", "#/requests" + (p.size ? "?" + p : ""));
    void refresh();
  }
  async function select(id: string) {
    detail = null;
    detailLoading = true;
    dialog.showModal();
    try {
      detail = await get<RequestDetail>(
        "/api/ui/requests/" + encodeURIComponent(id),
      );
      tab = detail.unrecognized ? "request" : "response";
    } catch (e) {
      fail(e);
      dialog.close();
    } finally {
      detailLoading = false;
    }
  }
  onMount(() => {
    return revision.subscribe(() => void refresh(true));
  });
  $effect(() => {
    data.now;
    untrack(() => void refresh());
  });
</script>

<div class="page-heading">
  <div>
    <h1>Запросы<span class="title-dot">.</span></h1>
    <p class="subtitle">От входного сообщения до ответа модели.</p>
  </div>
  <ActionForm
    action="requests.clear"
    confirm="Очистить историю запросов? Сохранённые запросы и ответы будут удалены без возможности восстановления."
    ondone={() => void refresh()}
    ><button type="submit">Очистить историю</button></ActionForm
  >
</div>
<form class="filters panel" onsubmit={filter}>
  <label class="search-label"
    >Поиск<input
      type="search"
      placeholder="Текст запроса или ответа"
      bind:value={q}
    /></label
  ><label
    >Модель<select bind:value={model} onchange={() => filter()}
      ><option value="">Все модели</option
      >{#each [...new Set([...data.models.map((m) => m.key), ...data.sessions
            .map((s) => s.model)
            .filter(Boolean)])] as key}<option>{key}</option>{/each}</select
    ></label
  ><label
    >Подключение<select bind:value={connection} onchange={() => filter()}
      ><option value="">Все подключения</option
      >{#each data.connections as c}<option value={c.name}
          >{c.displayName || c.name}</option
        >{/each}</select
    ></label
  ><label
    >Сессия<select bind:value={session} onchange={() => filter()}
      ><option value="">Все сессии</option
      >{#each data.sessions.filter((s) => s.id) as s}<option value={s.id}
          >{sessionOption(s)}</option
        >{/each}</select
    ></label
  ><label class="check"
    ><input
      type="checkbox"
      bind:checked={errors}
      onchange={() => filter()}
    />Только ошибки</label>
  <label class="check"><input type="checkbox" bind:checked={unrecognized} onchange={() => filter()} />Не распознано</label
  ><button type="submit" class="primary">Найти</button>
</form>
{#if session && list.sessionUsage}<div class="panel session-usage-panel">
    <div class="spread"><h2><SessionIdentity session={sessionMap.get(session)} id={session} /></h2><button class="text-button" onclick={() => { session = ""; filter(); }}>Все сессии</button></div>
    <ConnectionUsage usage={list.sessionUsage} scope="session" />
  </div>{/if}
<div class="section-head">
  <h2>{list.total} запросов</h2>
  <span class="muted" aria-live="polite"
    >{loading ? "Обновляем…" : "Данные обновляются автоматически"}</span
  >
</div>
<div class="request-list" aria-busy={loading}>
  {#each list.items as item, index}{#if index === 0 || list.items[index - 1].session !== item.session}<div
        class="request-session"
      >
        {#if item.session}<button class="text-button" onclick={() => { session = item.session; filter(); }}><SessionIdentity session={sessionMap.get(item.session)} id={item.session} /> <span aria-hidden="true">↗</span></button>{:else}Сессия · Без сессии{/if}
      </div>{/if}<button
      class="request-row"
      onclick={() => void select(item.id)}
      ><span class="request-signal" class:danger={item.failed}
        >{item.pending ? "◌" : item.failed ? "!" : "✓"}</span
      ><time>{time(item.start)}</time>
      <div class="request-copy">
        <strong>{item.unrecognized && item.model && item.served ? item.model + " → " + item.served : item.served || item.model || item.method + " " + item.path}</strong>
        {#if item.unrecognized}<small>{item.fallbackPool ? "Пул по умолчанию · " + item.fallbackPool : "Не распознано"} · {item.method} {item.path} · HTTP {item.status || "…"}</small>{/if}
        <p>{item.preview || "Запрос без текстового сообщения"}</p>
        {#if item.error}<small class="danger">{item.error}</small>{/if}
      </div>
      <span class="request-route">{item.connection || item.route || "—"}</span
      ><span class="duration"
        >{item.pending
          ? "В полёте"
          : item.failed
            ? "Ошибка " + item.status
            : (item.durationMs / 1000).toFixed(1) + " с"}</span
      ><span aria-hidden="true">↗</span></button
    >{/each}{#if !list.items.length}<div class="empty">
      <h3>
        {loading ? "Загружаем запросы…" : q || model || connection || errors || unrecognized || session
          ? "Ничего не найдено"
          : "Пока нет запросов"}
      </h3>
      <p>
        {loading ? "Получаем историю сессии." : q || model || connection || errors || unrecognized || session
          ? "Попробуйте изменить фильтры."
          : "Отправьте первый запрос через роутер — он появится здесь."}
      </p>
    </div>{/if}
</div>
<div class="pagination">
  <button
    disabled={offset === 0 || loading}
    onclick={() => {
      offset = Math.max(0, offset - 50);
      void refresh();
    }}>← Новее</button
  ><span
    >{list.total ? offset + 1 : 0}–{Math.min(
      offset + list.items.length,
      list.total,
    )} из {list.total}</span
  ><button
    disabled={offset + 50 >= list.total || loading}
    onclick={() => {
      offset += 50;
      void refresh();
    }}>Старее →</button
  >
</div>
<dialog class="request-dialog" bind:this={dialog} aria-label="Детали запроса">
  <div class="spread">
    <h2>Детали запроса</h2>
    <button onclick={() => dialog.close()} aria-label="Закрыть детали">✕</button
    >
  </div>
  {#if detailLoading}<p class="empty">Загружаем запрос…</p>{:else if detail}<p
      class="subtitle"
    >
      {detail.method} {detail.path} · HTTP {detail.status || "…"} · {time(detail.start)} · {detail.pending
        ? "В полёте"
        : (detail.durationMs / 1000).toFixed(1) + " с"}
    </p>
    {#if detail.unrecognized}<div class="alert"><strong>{detail.fallbackPool ? "Применён пул по умолчанию" : "Не распознано"}</strong><p>{detail.unrecognized}</p></div>{/if}
    <div class="request-routing" aria-label="Фактическое направление">
      <div><small>Запрошено</small><strong>{detail.model}</strong>
        <span>effort: {detail.requestedEffort === null ? "не зафиксирован" : detail.requestedEffort || "без effort"}</span></div>
      <span aria-hidden="true">→</span>
      <div><small>Последняя отправка</small><strong>{detail.served || (detail.route === "cloud" ? "Anthropic" : "не зафиксирована")}</strong>
        <span>effort: {detail.sentEffort === null ? "не зафиксирован" : detail.sentEffort || "по умолчанию модели"}</span></div>
    </div>
    {#if detail.error}<p class="alert danger">{detail.error}</p>{/if}
    <div class="detail-tabs" role="tablist" aria-label="Содержимое запроса">
      {#each [["response", "Ответ"], ["request", "Запрос"], ["sent", "В облако"], ["attempts", "Попытки"], ["advanced", "Расширенное"]] as [key, label]}<button
          role="tab"
          aria-selected={tab === key}
          onclick={() => (tab = key)}>{label}</button
        >{/each}
    </div>
    <div
      role="tabpanel"
      aria-label={tab === "response"
        ? "Ответ"
        : tab === "request"
          ? "Запрос"
          : tab === "sent"
            ? "В облако"
            : tab === "attempts"
              ? "Попытки"
              : "Расширенное"}
    >
      {#if tab === "attempts"}{#each detail.attempts as attempt, i}<div
            class="attempt"
          >
            <span class="badge">{i + 1}</span><strong>{attempt.model}</strong
            ><span>{(attempt.durationMs / 1000).toFixed(1)} с</span>
            <p class:danger={!!attempt.error}>{attempt.error || "Завершено"}</p>
          </div>{/each}{#if !detail.attempts.length}<p class="empty">
            Сведения о попытках не записаны
          </p>{/if}{:else if tab === "advanced"}<h3>Использование</h3>
        <dl class="stats">
          {#each Object.entries(detail.usage || {}) as [key, value]}<dt>
              {key}
            </dt>
            <dd>{value}</dd>{/each}
        </dl>
        <h3>Заголовки</h3>
        <pre>{JSON.stringify(detail.headers, null, 2)}</pre>
        <details>
          <summary>Исходный запрос</summary>
          <pre>{detail.request}</pre>
        </details>
        <details>
          <summary>Отправлено в облако</summary>
          <pre>{detail.sent}</pre>
        </details>
        <details>
          <summary>Исходный ответ</summary>
          <pre>{detail.response}</pre>
        </details>
        <div class="actions">
          <a
            class="button"
            href={"/requests/" +
              encodeURIComponent(detail.id) +
              "/request.json"}
            download>Скачать запрос</a
          ><a
            class="button"
            href={"/requests/" +
              encodeURIComponent(detail.id) +
              "/response.txt"}
            download>Скачать ответ</a
          ><a
            class="button"
            href={"/requests/" + encodeURIComponent(detail.id) + "/sent.json"}
            download>Скачать отправленное</a
          >
        </div>{:else if detail.unrecognized && (tab === "request" || tab === "response")}<pre>{tab === "request" ? detail.request || "Тело запроса пустое или не было прочитано" : detail.response || "Тело ответа пустое"}</pre>
        {#if tab === "request"}<h3>Заголовки запроса</h3><pre>{JSON.stringify(detail.headers, null, 2)}</pre>{/if}
        {:else}<Transcript
          content={tab === "request"
            ? detail.request
            : tab === "sent"
              ? detail.sent
              : detail.response}
        />{#if tab === "sent" && !detail.sent}<p class="muted">
            Отправленное содержимое не записано
          </p>{/if}{/if}
    </div>
    {#if detail.captureTruncated}<p class="help">{detail.requestNote || "Содержимое записано частично: достигнут лимит захвата."} В скачиваемом файле тоже только записанная часть.</p>
    {:else if detail.truncated}<p class="help">
        Предпросмотр сокращён. Полное содержимое доступно в скачиваемом файле.
      </p>{/if}{/if}
</dialog>
