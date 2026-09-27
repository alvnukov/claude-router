<script lang="ts">
  import { tokens, cachePercent } from "./usage";
  import type { UIState } from "./types";
  import { time } from "./api";
  import PatchBay from "./PatchBay.svelte";
  let { data }: { data: UIState } = $props();
</script>

<div class="page-heading">
  <div>
    <h1>Обзор<span class="title-dot">.</span></h1>
    <p class="subtitle">
      {!data.interception.enabled
        ? "Перехват отключён — запросы Claude Code могут обходить роутер."
        : data.summary.errors5m
          ? "Есть ошибки за последние пять минут — проверьте запросы."
          : "Сессии, маршруты и подключения — в одном поле зрения."}
    </p>
  </div>
  <a class="button" href="#/routes">Настроить маршруты ↗</a>
</div>
<section class="status-strip" aria-label="Состояние роутера">
  <span
    ><i class="status-dot"></i>Режим:
    <strong
      >{data.lifecycle === "active"
        ? "Работает"
        : data.lifecycle === "draining"
          ? "Завершает запросы"
          : data.lifecycle === "standby"
            ? "Ожидает запуска"
            : data.lifecycle === "stopped"
              ? "Остановлен"
              : "Неизвестен"}</strong
    ></span
  ><span class:danger={!data.interception.enabled}
    >Перехват <strong
      >{data.interception.enabled ? "включён" : "выключен"}</strong
    ></span
  ><span class:danger={data.summary.errors5m > 0}
    >Ошибок за 5 мин <strong>{data.summary.errors5m}</strong></span
  ><span>В полёте <strong>{data.summary.pending}</strong></span>
</section>
{#if data.interception.error}<p class="alert danger">
    {data.interception.error}
  </p>{/if}
<PatchBay {data} />
<section class="panel"><div class="spread"><div><h2>Конфиденциальность</h2><p class="muted">Настройте профили защиты, проверьте преобразования в лаборатории и наблюдайте за применением к трафику.</p></div><a class="button" href="#/privacy">Проверить фильтр ↗</a></div></section>
<section>
  <div class="section-head">
    <h2>Сессии</h2>
    <a class="text-link" href="#/requests">История запросов ↗</a>
  </div>
  <div class="session-list">
    {#each data.sessions.slice(0, 12) as session}<a
        class="session-row"
        href={"#/requests?session=" + encodeURIComponent(session.id)}
        ><span class="session-icon" aria-hidden="true">⌘</span>
        <div>
          <strong>{session.id ? session.id.slice(0, 12) : "Без сессии"}</strong
          ><small>{session.model || "Модель не определена"}</small>
          {#if session.usage}<small class="session-token-summary" class:usage-attention={session.usage.lowCache || session.usage.invalidRequests > 0}>
              {#if session.usage.invalidRequests > 0}Счётчики расходятся · {:else if session.usage.lowCache}Мало кеша · {/if}
              Кеш {cachePercent(session.usage) === null ? "—" : cachePercent(session.usage) + "%"} · без кеша {session.usage.cacheMeasuredRequests ? tokens(session.usage.uncachedInputTokens) : "—"} · выход {session.usage.measuredRequests ? tokens(session.usage.outputTokens) : "—"} · 24 ч
            </small>{/if}
        </div>
        <div>
          <span
            >{session.route === "cloud"
              ? "Через Anthropic"
              : session.route === "local"
                ? "Через подключение"
                : session.route || "Маршрут не определён"}</span
          ><small class:danger={!!session.error}
            >{session.error ||
              (session.pending
                ? "В полёте: " + session.pending
                : "Нет активных запросов")}</small
          >
        </div>
        <time>{time(session.lastAt)}</time><span class="muted"
          >{session.requests} запр.</span
        ></a
      >{/each}{#if !data.sessions.length}<div class="empty">
        <h3>Здесь появятся ваши сессии</h3>
        <p>Отправьте запрос из Claude Code через роутер.</p>
      </div>{/if}
  </div>
</section>
