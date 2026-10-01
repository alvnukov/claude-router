<script lang="ts">
  import type { UIState } from "./types";
  import ActionForm from "./ActionForm.svelte";
  import CodexLogin from "./CodexLogin.svelte";
  import LimitMeter from "./LimitMeter.svelte";
  import ConnectionUsage from "./ConnectionUsage.svelte";
  import { time } from "./api";
  let { data }: { data: UIState } = $props();
  let adding = $state(false);
  let addType = $state("codex");
</script>

<div class="page-heading">
  <div>
    <h1>Подключения<span class="title-dot">.</span></h1>
    <p class="subtitle">
      Аккаунты, доступные модели и запас на следующую задачу.
    </p>
  </div>
  <button
    class="primary"
    onclick={() => (adding = !adding)}
    aria-expanded={adding}
    >{adding ? "Закрыть форму" : "+ Добавить подключение"}</button
  >
</div>
<section class="panel interception">
  <div>
    <h2>Перехват Claude Code</h2>
    <p class="muted">
      {data.interception.enabled
        ? "Claude Code направляет запросы через роутер."
        : "Подключите Claude Code, чтобы применять маршруты."}
    </p>
    {#if data.interception.error}<p class="danger">
        {data.interception.error}
      </p>{/if}
  </div>
  <ActionForm
    action="interception.set"
    fields={{ op: data.interception.enabled ? "restore" : "connect" }}
    confirm={data.interception.enabled
      ? "Отключить перехват? Следующие запросы Claude Code пойдут без маршрутизации роутера."
      : "Подключить Claude Code к роутеру? Его настройка адреса API будет изменена."}
    ><button type="submit" class={data.interception.enabled ? "" : "primary"}
      >{data.interception.enabled
        ? "Отключить перехват"
        : "Включить перехват"}</button
    ></ActionForm
  >
</section>
{#if adding}<section class="panel add-connection">
    <h2>Новое подключение</h2>
    <p class="help">
      Укажите имя и тип. Для подписки Codex после добавления нажмите «Войти».
    </p>
    <ActionForm
      action="connection.edit"
      fields={{ op: "add" }}
      class="form-grid"
      ondone={() => (adding = false)}
      ><label
        >Название для отображения<input
          name="display_name"
          placeholder="Мой основной аккаунт"
        /></label
      ><label
        >Короткий идентификатор<input
          name="name"
          placeholder="codex-main"
          pattern="[^\s/]+"
          required
        /><small>Без пробелов и /, используется в названиях моделей</small
        ></label
      ><label
        >Тип<select name="type" bind:value={addType}
          ><option value="codex">Codex · подписка ChatGPT</option><option
            value="">OpenAI-совместимый API</option
          ></select
        ></label
      >{#if addType !== "codex"}<label
          >Адрес API<input
            type="url"
            name="base_url"
            placeholder="https://example.com/v1"
            required
          /></label
        ><label
          >API-ключ<input
            type="password"
            name="api_key"
            autocomplete="new-password"
            placeholder="Если требуется"
          /></label
        >{/if}<button type="submit" class="primary">Добавить подключение</button
      ></ActionForm
    >
  </section>{/if}
<div class="section-head">
  <h2>Ваши подключения <span class="muted">{data.connections.length}</span></h2>
  <ActionForm action="catalog.refresh"
    ><button type="submit">Обновить каталог</button></ActionForm
  >
</div>
<div class="cards connections">
  {#each data.connections as connection, i (connection.name)}<article
      class="card connection-card"
      data-color={i % 5}
    >
      <div class="spread">
        <div>
          <p class="eyebrow">
            {connection.type === "codex"
              ? "CODEX / ПОДПИСКА"
              : connection.type === "anthropic"
                ? "ANTHROPIC"
                : "СОВМЕСТИМЫЙ API"}
          </p>
          <h2>{connection.displayName || connection.name}</h2>
        </div>
        <span class="cable-mark"></span>
      </div>
      <div class="connection-health">
        <span class="badge" class:danger={!!connection.error}
          >{connection.error
            ? "Требует внимания"
            : connection.pending
              ? "Вход не завершён"
              : connection.connected
                ? "Подключено"
                : "Ожидает подключения"}</span
        ><small>Обновлено {time(connection.updated)}</small>
      </div>
		{#if connection.authMode === "chatgpt-plan"}<p class="muted">{connection.subscriptionEnabled ? "Доступ по подписке ChatGPT разрешён" : "Доступ по подписке отключён. Разрешите его при входе с ChatGPT."}</p>
		{:else if connection.type === "codex" && connection.connected}<p class="muted">Вход Codex CLI. Для актуального каталога войдите с ChatGPT.</p>{/if}
		{#if connection.catalogUpdated}<small class="muted">Каталог моделей: {time(connection.catalogUpdated)}</small>{/if}
      {#if connection.error}<p class="alert danger">
          {connection.error}
        </p>{/if}{#each connection.limits as limit}<LimitMeter
          {limit}
          now={data.now}
        />{/each}{#if !connection.limits.length}<p class="muted">
          {connection.authMode === "chatgpt-plan" ? "Лимиты подписки доступны в настройках ChatGPT." : connection.refreshing ? "Обновляем лимиты…" : "Лимиты не получены"}
        </p>{/if}
		{#if connection.usageURL}<a href={connection.usageURL} target="_blank" rel="noopener noreferrer">Настройки ChatGPT → Usage ↗</a>{/if}
      {#if connection.type === "codex" && connection.connected && connection.authMode !== "chatgpt-plan"}<p class="muted">
          Доступно сбросов лимита: <strong
            >{connection.resetsKnown ? connection.resets : "не сообщено"}</strong
          >
        </p>{/if}
      {#if connection.type === "codex"}<div class="actions">
          <CodexLogin {connection} />
          <ActionForm
            action="codex.import"
            fields={{ provider: connection.name }}
            ><button type="submit">Импортировать вход Codex CLI</button
            ></ActionForm
          >{#if connection.authMode !== "chatgpt-plan"}<ActionForm
            action="codex.usage"
            fields={{ provider: connection.name }}
            ><button type="submit">Обновить лимиты</button></ActionForm
          >{/if}
        </div>{/if}
      <ConnectionUsage usage={connection.usage} />
      {#if connection.type !== "anthropic"}<div class="section-head">
          <h3>
            Модели <span class="muted"
              >{data.models.filter((m) => m.provider === connection.name)
                .length}</span
            >
          </h3>
          <ActionForm
            action="connection.probe"
            fields={{ provider: connection.name }}
            ><button type="submit" class="text-button"
              >Проверить подключение</button
            ></ActionForm
          >
        </div>
        <div class="model-list">
          {#each data.models
            .filter((m) => m.provider === connection.name)
            .map((m) => m.key) as key}<div class="spread">
              <code>{key}</code><ActionForm
                action="model.edit"
                fields={{ op: "remove", key }}
                confirm={"Убрать модель «" +
                  key +
                  "» из каталога? Если она используется в маршрутах или пулах, сначала измените их."}
                ><button
                  type="submit"
                  class="text-button"
                  aria-label={"Убрать модель " + key}>Убрать</button
                ></ActionForm
              >
            </div>{/each}
        </div>
        <ActionForm
          action="model.edit"
          fields={{ op: "add", provider: connection.name }}
          class="inline"
          ><label
            >Модель<input
              name="model"
              list={"catalog-" + i}
              placeholder="ID модели"
              required
            /><datalist id={"catalog-" + i}
              >{#each connection.models as model}<option value={model}
                  >{model}</option
                >{/each}</datalist
            ></label
          ><button type="submit">Добавить</button></ActionForm
        >
        <details class="extra">
          <summary>Настройки подключения</summary><ActionForm
            action="connection.edit"
            fields={{
              op: "update",
              orig: connection.name,
              type: connection.type,
            }}
            class="form-grid"
            ><label
              >Название<input
                name="display_name"
                value={connection.displayName}
              /></label
            ><label
              >Идентификатор<input
                name="name"
                value={connection.name}
                readonly={connection.type === "codex"}
                required
              /></label
            >{#if connection.type !== "codex"}<label
                >Адрес API<input
                  type="url"
                  name="base_url"
                  value={connection.baseURL}
                  data-original-url={connection.baseURL}
                  required
                /></label
              ><label
                >API-ключ<input
                  type="password"
                  name="api_key"
                  autocomplete="new-password"
                  placeholder={connection.keySet
                    ? "Задан · пустое поле сохраняет ключ"
                    : "Не задан"}
                /></label
              >{#if connection.keySet}<label class="check"
                  ><input type="checkbox" name="clear_key" value="1" />Удалить
                  сохранённый API-ключ</label
                >{/if}{/if}<button type="submit" class="primary"
              >Сохранить</button
            ></ActionForm
          ><ActionForm
            action="connection.edit"
            fields={{ op: "remove", name: connection.name }}
            confirm={"Удалить подключение «" +
              (connection.displayName || connection.name) +
              "»? Сначала уберите его модели из маршрутов и пулов."}
            ><button type="submit" class="text-button danger"
              >Удалить подключение</button
            ></ActionForm
          >
        </details>{/if}
    </article>{/each}
</div>
{#if !data.connections.length}<div class="empty">
    <h2>Подключите первую модель</h2>
    <p>Добавьте подписку Codex или совместимый API кнопкой выше.</p>
  </div>{/if}
