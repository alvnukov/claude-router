<script lang="ts">
  import type { ConnectionUsage } from "./types";
  import { tokens, exactTokens, cachePercent } from "./usage";
  let { usage, scope = "connection" }: { usage?: ConnectionUsage; scope?: "connection" | "session" } = $props();
  const rate = $derived(cachePercent(usage));
  const attention = $derived(!!usage && (usage.lowCache || usage.invalidRequests > 0));
</script>

<section class="token-usage" class:attention aria-label={scope === "session" ? "Токены сессии за 24 часа" : "Токены подключения за 24 часа"}>
  <div class="usage-heading">
    <h3>{scope === "session" ? "Токены сессии" : "Токены"} <span>· 24 часа</span></h3>
    {#if attention}<span class="usage-notice"><span aria-hidden="true">●</span> {usage?.invalidRequests ? "Счётчики расходятся" : "Мало кеша"}</span>{/if}
  </div>
  {#if usage && usage.measuredRequests > 0}
    <div class="usage-input">
      <span>Входные <strong title={exactTokens(usage.inputTokens)}>{tokens(usage.inputTokens)}</strong></span>
      <span class="cache-rate">Кеш <strong>{rate === null ? "—" : rate + "%"}</strong></span>
    </div>
    {#if rate !== null}<progress class="cache-meter" aria-label="Доля входных токенов из кеша" max="100" value={rate}></progress>{/if}
    <dl class="usage-numbers">
      <div><dt>Без кеша</dt><dd title={usage.cacheMeasuredRequests ? exactTokens(usage.uncachedInputTokens) : "Нет данных о кеше"}>{usage.cacheMeasuredRequests ? tokens(usage.uncachedInputTokens) : "—"}</dd></div>
      <div><dt>Из кеша</dt><dd title={usage.cacheMeasuredRequests ? exactTokens(usage.cachedInputTokens) : "Нет данных о кеше"}>{usage.cacheMeasuredRequests ? tokens(usage.cachedInputTokens) : "—"}</dd></div>
      <div><dt>Выходные</dt><dd title={exactTokens(usage.outputTokens)}>{tokens(usage.outputTokens)}</dd></div>
    </dl>
    <p class="usage-coverage">Кеш измерен: {usage.cacheMeasuredRequests} из {usage.requests} запросов{usage.cacheMeasuredRequests < usage.requests ? " · суммы неполные" : ""}</p>
  {:else}
    <p class="usage-empty">{usage?.requests ? "В истории пока нет измеренных токенов." : "За последние 24 часа запросов нет."}</p>
  {/if}
  {#if usage?.lowCache}<p class="usage-warning">Крупные запросы повторно обрабатываются почти без кеша.</p>{/if}
  {#if usage?.invalidRequests}<p class="usage-warning">Несогласованные данные в {usage.invalidRequests} запросах исключены из сумм.</p>{/if}
  <details class="usage-details">
    <summary>Как читать расход</summary>
    <div>
      <p>Входные включают кеш. «Без кеша» — входные минус чтение из кеша, включая запись нового кеша. Выходные включают рассуждения модели.</p>
      <p>Это токены, сообщённые провайдером, а не сумма списания по подписке. Её нельзя точно вычислить по этим счётчикам.</p>
      <p>{scope === "session" ? "Сохранённая история этой сессии за 24 часа, по всем подключениям. Другие фильтры списка не меняют эти суммы." : "Сохранённая история этого подключения за 24 часа. После смены аккаунта предыдущие запросы остаются в этом окне."} Запросы вне роутера сюда не входят.</p>
      {#if usage}
        <p>Токены измерены в {usage.measuredRequests} из {usage.requests} завершённых запросов. Данные о кеше известны для {usage.cacheMeasuredRequests}. Отсутствующие данные не считаются нулевым кешем; процент рассчитан только по измеренным входным.</p>
        {#if usage.reasoningMeasuredRequests > 0}<p>Из выходных токенов {exactTokens(usage.reasoningTokens)} — рассуждения ({usage.reasoningMeasuredRequests} запросов с этой детализацией).</p>{/if}
        {#if usage.cacheWriteTokens > 0}<p>Запись нового кеша: {exactTokens(usage.cacheWriteTokens)} токенов.</p>{/if}
        {#if usage.continuationRequests > 0}<p>Внутренние продолжения: {usage.continuationRequests} запросов. Учтены все {usage.upstreamCalls} измеренных обращений к модели.</p>{/if}
        {#if usage.lowCache}<p>Сигнал появляется, когда в последних 5 крупных запросах одной сессии к одной модели кеш меньше 20% при сумме входных от 100 тысяч. Первый запрос не учитывается. Это повод проверить расход: смена контекста или его обрезка также могут снижать кеш.</p>{/if}
      {/if}
    </div>
  </details>
</section>

<style>
  .token-usage { margin-top: 18px; padding-top: 16px; border-top: 1px solid var(--line); }
  .usage-heading, .usage-input { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
  h3 { font-size: 13px; }
  h3 span { color: var(--muted); font-weight: 400; }
  .usage-notice { color: var(--orange); font-size: 11px; font-weight: 550; }
  .usage-notice span { font-size: 8px; vertical-align: 1px; margin-right: 3px; }
  .usage-input { margin-top: 12px; font-size: 12px; color: var(--muted); }
  .usage-input strong { font-variant-numeric: tabular-nums; color: var(--ink); margin-left: 5px; }
  .cache-rate strong { color: var(--teal); }
  .attention .cache-rate strong { color: var(--orange); }
  .cache-meter { display: block; appearance: none; width: 100%; height: 4px; margin: 8px 0 14px; border: 0; border-radius: 2px; overflow: hidden; background: var(--sunk); color: var(--teal); }
  .cache-meter::-webkit-progress-bar { background: var(--sunk); }
  .cache-meter::-webkit-progress-value { background: var(--teal); border-radius: 2px; }
  .cache-meter::-moz-progress-bar { background: var(--teal); border-radius: 2px; }
  .usage-numbers { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 12px; margin: 12px 0 9px; }
  dt { font-size: 11px; color: var(--muted); }
  dd { margin: 3px 0 0; font-size: clamp(15px, 2vw, 20px); font-weight: 600; letter-spacing: -0.4px; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }
  .usage-coverage, .usage-empty, .usage-warning { font-size: 11px; color: var(--muted); }
  .usage-empty { margin: 12px 0; }
  .usage-warning { color: var(--orange); margin-top: 8px; padding-left: 9px; border-left: 2px solid var(--orange); }
  .usage-details { color: var(--muted); font-size: 11px; margin-top: 10px; }
  .usage-details summary { cursor: pointer; width: fit-content; }
  .usage-details div { margin-top: 8px; line-height: 1.6; }
  .usage-details p + p { margin-top: 7px; }
</style>
