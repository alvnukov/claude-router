<script lang="ts">
  import type { Session } from "./types";
  import { tokens, exactTokens, cachePercent } from "./usage";
  let { session }: { session: Session } = $props();
</script>

<small class="session-token-summary session-context" title={session.context ? `Вход последнего успешного запроса: ${exactTokens(session.context.inputTokens)} токенов. Во время нового запроса показано последнее измерение.` : "Последний успешный запрос не сообщил достоверные счётчики токенов."}>
  Контекст: {session.context ? tokens(session.context.inputTokens) + " токенов" : "—"} · кеш {session.context?.cacheKnown && session.context.inputTokens > 0 ? Math.round(100 * session.context.cachedInputTokens / session.context.inputTokens) + "%" : "—"}
</small>
{#if session.totalUsage}<small class="session-token-summary session-total" class:usage-attention={session.usage?.lowCache || session.totalUsage.invalidRequests > 0} title="Накопленный расход сессии по сохранённой истории роутера. Включает внутренние продолжения; удалённые запросы не учитываются.">
    {#if session.totalUsage.invalidRequests > 0}Счётчики расходятся · {:else if session.usage?.lowCache}Мало кеша · {/if}
    За сессию: вход {session.totalUsage.measuredRequests ? tokens(session.totalUsage.inputTokens) : "—"} · выход {session.totalUsage.measuredRequests ? tokens(session.totalUsage.outputTokens) : "—"} · кеш {cachePercent(session.totalUsage) === null ? "—" : cachePercent(session.totalUsage) + "%"} · по истории{session.totalUsage.measuredRequests < session.totalUsage.requests || session.totalUsage.cacheMeasuredRequests < session.totalUsage.measuredRequests ? " · неполные данные" : ""}
  </small>{/if}
