<script lang="ts">
  import type { UIState } from "./types";
  import ActionForm from "./ActionForm.svelte";
  import RouteEditor from "./RouteEditor.svelte";
  import { act } from "./api";
  let { data }: { data: UIState } = $props();
  let cloneDialog: HTMLDialogElement;
  let cloneSource = $state("");
  let cloneProfile = $state("");
  let cloneName = $state("");
  let cloneBusy = $state(false);
  let cloneError = $state("");
  function openClone(source: string) {
    cloneSource = source;
    cloneProfile = data.activeProfile;
    cloneError = "";
    const base = source + " — копия";
    cloneName = base;
    for (let n = 2; data.pools.some((p) => p.name === cloneName); n++)
      cloneName = base + " " + n;
    cloneDialog.showModal();
  }
  async function clonePool(event: SubmitEvent) {
    event.preventDefault();
    cloneBusy = true;
    cloneError = "";
    try {
      await act("pool.edit", {
        profile: cloneProfile, op: "clone", source: cloneSource, name: cloneName,
      });
      cloneDialog.close();
    } catch (error) {
      cloneError = error instanceof Error ? error.message : "Не удалось создать копию";
    } finally {
      cloneBusy = false;
    }
  }
</script>

<div class="page-heading">
  <div>
    <h1>Маршруты<span class="title-dot">.</span></h1>
    <p class="subtitle">
      Какая модель ответит на запрос. Профиль: <strong
        >{data.activeProfile}</strong
      >.
    </p>
  </div>
</div>
<section class="panel">
  <h2>Пул по умолчанию</h2>
  <p class="help">Для моделей, у которых нет правил версии или семейства.
    Явно отключённые маршруты отклоняются. Effort определяется настройками участников пула.</p>
  {#key data.activeProfile}<ActionForm action="pool.edit" fields={{profile:data.activeProfile, op:"default"}} class="inline">
    <label>Пул для неизвестных моделей<select name="name" value={data.defaultPool}>
      <option value="">Отклонять запросы без маршрута</option>
      {#each data.pools as pool}<option value={pool.name}>{pool.name}{pool.members.length ? "" : " · пустой"}</option>{/each}
    </select></label>
    <button class="primary" type="submit">Сохранить пул по умолчанию</button>
  </ActionForm>{/key}
  {#if data.defaultPool && !data.pools.find(p => p.name === data.defaultPool)?.members.length}
    <p class="danger">Пул по умолчанию пуст. Добавьте модели, чтобы запросы могли выполняться.</p>
  {/if}
</section>
<section class="panel">
  <div class="section-head">
    <h2>Модель × усилие</h2>
    <span class="muted">Изменения действуют после сохранения</span>
  </div>
  <p class="help">
    Выбор модели или пула в «Без effort» заполняет свободные уровни.
    «Применить ко всем» перезаписывает назначения; затем сохраните строку.
    Чтобы пул сохранял усилие запроса, выберите этот режим у его моделей.
  </p>
  {#key data.activeProfile}{#each data.families as route (route.model)}<RouteEditor
        {data}
        {route}
        family
      />{/each}
    <details class="extra">
      <summary>Переопределения отдельных версий · {data.routes.length}</summary
      >{#each data.routes as route (route.model)}<RouteEditor
          {data}
          {route}
        />{/each}<ActionForm
        action="route.save"
        fields={{
          profile: data.activeProfile,
          scope: "model",
          default: "inherit",
          low: "inherit",
          medium: "inherit",
          high: "inherit",
          xhigh: "inherit",
          max: "inherit",
        }}
        class="inline"
        ><label
          >ID модели Anthropic<input
            name="model"
            placeholder="claude-sonnet-…"
            required
          /></label
        ><button type="submit">Добавить версию</button></ActionForm
      >
    </details>{/key}
</section>
<section>
  <div class="section-head">
    <div>
      <h2>Пулы моделей</h2>
      <p class="muted">Порядок, резервные модели и поведение сессий.</p>
    </div>
  </div>
  <div class="cards pools">
    {#each data.pools as pool (data.activeProfile + ":" + pool.name)}<article class="card">
        <div class="spread">
          <h3>{pool.name}</h3>
          {#if data.defaultPool === pool.name}<span class="badge">По умолчанию</span>{/if}
          <button type="button" aria-label={"Клонировать пул " + pool.name}
            onclick={() => openClone(pool.name)}>Клонировать</button>
          <span class="badge"
            >{pool.type === "balance" ? "Балансировка" : "По порядку"}</span
          >
        </div>
        <div class="pool-members">
          {#each pool.members as member, index (member.model)}<div class="pool-member">
              <div class="spread">
                <strong>{member.model}</strong><span
                  class="badge"
                  class:danger={member.cooling}
                  title="Пауза после ошибки. Авторизация и лимиты показаны в разделе «Подключения»."
                  >{member.cooling ? "Пауза после ошибки" : "Без паузы"}</span
                >
              </div>
              <ActionForm
                action="pool.edit"
                fields={{
                  profile: data.activeProfile,
                  name: pool.name,
                  key: member.model,
                }}
                class="inline"
                ><label
                  >Усилие<select name="effort" value={member.effort}
                    ><option value="">По умолчанию модели</option>
                    <option value="request">Сохранять effort запроса</option
                    >{#if member.effort && member.effort !== "request" && !member.efforts.includes(member.effort)}<option
                        value={member.effort}
                        >{member.effort} · не подтверждён</option
                      >{/if}{#each member.efforts as effort}<option
                        value={effort}>{effort}</option
                      >{/each}</select
                  ></label
                ><button type="submit" name="op" value="effort"
                  >Сохранить</button
                ><button
                  type="submit"
                  name="op"
                  value="up"
                  disabled={index === 0}
                  aria-label={"Поднять " + member.model}>↑</button
                ><button
                  type="submit"
                  name="op"
                  value="down"
                  disabled={index === pool.members.length - 1}
                  aria-label={"Опустить " + member.model}>↓</button
                ><button type="submit" name="op" value="remove">Убрать</button
                ></ActionForm
              >
              <details class="extra">
                <summary>Сопоставление effort{Object.keys(member.effortMap || {}).length ? " · настроено" : ""}</summary>
                <p class="help">Слева — effort запроса, поступившего в пул; справа — effort этой модели.
                  «Основное правило» использует выбор выше.</p>
                <ActionForm action="pool.edit" fields={{ profile: data.activeProfile, name: pool.name, key: member.model, op: "mapping" }} class="form-grid">
                  {#each data.efforts as source}
                    <label>{source === "default" ? "Без effort" : source} пула →
                      <select name={source} aria-label={"Effort " + source + " для " + member.model}
                        value={member.effortMap?.[source] ?? "inherit"}>
                        <option value="inherit">Основное правило</option>
                        <option value="request">Как в запросе</option>
                        <option value="">По умолчанию модели</option>
                        {#if member.effortMap?.[source] && member.effortMap[source] !== "request" && !member.efforts.includes(member.effortMap[source])}
                          <option value={member.effortMap[source]}>{member.effortMap[source]} · не подтверждён</option>
                        {/if}
                        {#each member.efforts as effort}<option value={effort}>{effort}</option>{/each}
                      </select>
                    </label>
                  {/each}
                  <button class="primary" type="submit">Сохранить сопоставление</button>
                </ActionForm>
              </details>
              {#if Object.keys(member.effortMap).length}<p class="help">
                {Object.entries(member.effortMap).map(([source, target]) =>
                  (source === "default" ? "без effort" : source) + " → " +
                  (target === "request" ? source === "default" ? "по умолчанию" : source : target || "по умолчанию")
                ).join(" · ")}
              </p>{/if}
            </div>{/each}
        </div>
        <p class="help">«Сохранять effort запроса»: high → high, medium → medium.
          Без effort — по умолчанию модели. Модель должна поддерживать переданный уровень.</p>
        <ActionForm
          action="pool.edit"
          fields={{ profile: data.activeProfile, name: pool.name, op: "add" }}
          class="inline"
          ><label
            >Добавить модель<select name="key" required
              >{#each data.models.filter((m) => !pool.members.some((x) => x.model === m.key)) as model}<option
                  value={model.key}>{model.key}</option
                >{/each}</select
            ></label
          ><button
            type="submit"
            disabled={!data.models.some(
              (m) => !pool.members.some((x) => x.model === m.key),
            )}>Добавить</button
          ></ActionForm
        >
        <details class="extra">
          <summary>Поведение пула</summary><ActionForm
            action="pool.settings"
            fields={{
              profile: data.activeProfile,
              name: pool.name,
              failover: "0",
            }}
            class="form-grid"
            ><label
              >Выбор модели<select name="type" value={pool.type}
                ><option value="failover">Первая доступная по порядку</option
                ><option value="balance">Распределять новые сессии</option
                ></select
              ></label
            ><label class="check"
              ><input
                type="checkbox"
                name="failover"
                value="1"
                checked={pool.failover}
              />Переключать модель при сбое</label
            ><label
              >Ожидание первого ответа, сек.<input
                type="number"
                min="0"
                name="first_byte"
                value={pool.firstByte}
              /></label
            ><label
              >Проверка простаивающих, сек.<input
                type="number"
                min="0"
                name="probe_every"
                value={pool.probeEvery}
              /></label
            ><label
              >Лимит символов контекста<input
                type="number"
                min="0"
                name="max_input_chars"
                value={pool.maxInputChars}
              /></label
            >
            <p class="help">
              0 отключает ограничение. Новые правила выбора действуют на новые
              сессии.
            </p>
            <button class="primary" type="submit">Сохранить настройки</button
            ></ActionForm
          >
        </details>
        <ActionForm
          action="pool.edit"
          fields={{
            profile: data.activeProfile,
            name: pool.name,
            op: "delete",
          }}
          confirm={"Удалить пул «" +
            pool.name +
            "»? Используемый маршрутами пул удалить нельзя."}
          ><button class="text-button danger" type="submit">Удалить пул</button
          ></ActionForm
        >
      </article>{/each}
  </div>
  <ActionForm
    action="pool.edit"
    fields={{ profile: data.activeProfile, op: "create" }}
    class="inline new-item"
    ><label
      >Новый пул<input
        name="name"
        placeholder="Например, быстрые ответы"
        required
      /></label
    ><button type="submit" class="primary">Создать пул</button></ActionForm
  >
</section>
<dialog bind:this={cloneDialog} aria-label="Клонирование пула" oncancel={(event) => { if (cloneBusy) event.preventDefault(); }}>
  <h2>Клонировать «{cloneSource}»</h2>
  <p class="subtitle">Копия сохранит модели, их порядок, effort и настройки поведения.</p>
  <form onsubmit={clonePool} aria-busy={cloneBusy}>
    <fieldset disabled={cloneBusy}>
      <label>Название копии<input name="name" bind:value={cloneName} required /></label>
      {#if cloneError}<p class="danger" role="alert">{cloneError}</p>{/if}
      <div class="actions">
        <button type="button" onclick={() => cloneDialog.close()}>Отмена</button>
        <button type="submit" class="primary">{cloneBusy ? "Создаём…" : "Создать копию"}</button>
      </div>
    </fieldset>
  </form>
</dialog>
<section class="panel">
  <h2>Управление профилями</h2>
  <ActionForm action="profile.create" class="inline"
    ><label
      >Название<input
        name="name"
        placeholder="Название нового профиля"
        required
      /></label
    ><button type="submit" name="mode" value="clone"
      >Клонировать активный</button
    ><button type="submit" name="mode" value="empty">Создать пустой</button
    ></ActionForm
  >{#if data.profiles.length > 1}<ActionForm
      action="profile.delete"
      confirm="Удалить профиль и его сохранённые маршруты? Это действие нельзя отменить."
      class="inline"
      ><label
        >Неактивный профиль<select name="name"
          >{#each data.profiles.filter((p) => p.name !== data.activeProfile) as profile}<option
              >{profile.name}</option
            >{/each}</select
        ></label
      ><button class="danger" type="submit">Удалить профиль</button></ActionForm
    >{/if}
</section>
