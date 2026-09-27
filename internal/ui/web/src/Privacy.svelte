<script lang="ts">
  import { onMount } from "svelte";
  import type { UIState } from "./types";
  import { bytes, kindName, privacyAPI } from "./privacy";
  import type { FilterProfile, PrivacyMetrics, ProfileSnapshot } from "./privacy";
  import PrivacyLab from "./PrivacyLab.svelte";
  import PrivacyProfiles from "./PrivacyProfiles.svelte";
  let { data }: {data:UIState}=$props();
  let tab=$state("lab");
  let metrics=$state<PrivacyMetrics|null>(null);
  let snapshot=$state<ProfileSnapshot|null>(null);
  let error=$state("");
  let notice=$state("");
  let initial=$state<{profile:FilterProfile;filter:string}|null>(null);
  let labVersion=$state(0);
  let editorVersion=$state(0);
  let loading=$state(true);
  let alive=true;
  const abort=new AbortController();
  let refreshing=false;
  async function refresh() {
    if(refreshing) return;
    refreshing=true;
    try { const result=await privacyAPI<PrivacyMetrics>("",undefined,abort.signal); if(alive) { metrics=result;error=""; } }
    catch(e) { if(alive) error=e instanceof Error ? e.message : "Метрики недоступны"; }
    finally { refreshing=false; }
  }
  async function load() {
    loading=true;
    try { const s=await privacyAPI<ProfileSnapshot>("/config",undefined,abort.signal); if(alive) {snapshot=s;editorVersion++;error="";} }
    catch(e) { if(alive) error=e instanceof Error ? e.message : "Настройки недоступны"; }
    finally { loading=false; }
  }
  function saved(s:ProfileSnapshot) { snapshot=s;void refresh();notice="Профили сохранены. Новые запросы используют новые настройки; текущие запросы завершаются со своим снимком правил."; }
  function test(profile:FilterProfile,filter:string) { initial={profile,filter};labVersion++;tab="lab";window.scrollTo({top:0,behavior:"smooth"}); }
  onMount(()=>{void load();void refresh();const tick=window.setInterval(()=>{if(document.visibilityState==="visible") void refresh();},10000);return ()=>{alive=false;abort.abort();clearInterval(tick);};});
</script>

<div class="privacy-page">
  <div class="page-heading"><div><p class="eyebrow privacy-eyebrow">PRIVACY / ЛОКАЛЬНЫЙ КОНТРОЛЬ</p><h1>Конфиденциальность<span class="title-dot">.</span></h1><p class="subtitle">Понимайте, что видит модель. Проверяйте, что возвращается обратно.</p></div><span class="privacy-local-badge"><svg width="20" height="22" viewBox="0 0 20 22" aria-hidden="true"><path d="M10 1 18 4v6c0 5-5 9-8 11-3-2-8-6-8-11V4Z" fill="none" stroke="currentColor" stroke-width="1.5"/><path d="m6 10 3 3 5-6" fill="none" stroke="currentColor" stroke-width="1.5"/></svg>Проверка на вашей машине</span></div>
  <section class="privacy-status" aria-label="Состояние защиты"><div class="privacy-status-icon" aria-hidden="true">◐</div><div><strong>{metrics?.traffic?.status==="invalid" || metrics?.traffic?.status==="unavailable" ? "Отправка заблокирована: проверьте настройки" : metrics?.traffic?.enabled ? "Маскирование подключено к трафику" : "Маскирование трафика выключено"}</strong><p>Применяются назначенные включённые профили. Без назначения или при выключенном профиле данные передаются без маскирования. Ответы проверяются целиком, поэтому потоковая выдача задерживается до конца генерации.</p></div><div class="privacy-file-state"><small>privacy-profiles.json</small><strong>{snapshot?.status==="ready" ? "Загружен" : snapshot?.status==="missing" ? "Ещё не создан" : snapshot?.status==="invalid" ? "Ошибка конфигурации" : snapshot?.status==="unavailable" ? "Хранилище недоступно" : "Загрузка…"}</strong></div></section>
  <nav class="privacy-tabs" aria-label="Разделы конфиденциальности">{#each [["lab","Лаборатория"],["profiles","Профили и назначения"],["metrics","Наблюдение"]] as [id,label]}<button class:active={tab===id} aria-current={tab===id ? "page" : undefined} onclick={()=>tab=id}>{label}{#if id==="profiles" && snapshot?.config}<span>{snapshot.config.profiles.length}</span>{/if}</button>{/each}</nav>
  {#if notice}<p class="notice" role="status">{notice}<button class="text-button" onclick={()=>notice=""} aria-label="Закрыть сообщение о профилях">✕</button></p>{/if}
  {#if error}<p class="alert danger" role="alert">{error}<button onclick={()=>{void load();void refresh();}}>Повторить загрузку</button></p>{/if}
  <div hidden={tab!=="lab"}>{#key labVersion}<PrivacyLab profiles={snapshot?.config?.profiles || []} {initial} onresult={()=>void refresh()}/>{/key}</div>
  <div hidden={tab!=="profiles"}>{#if snapshot}{#key editorVersion}<PrivacyProfiles {snapshot} {data} onsaved={saved} ontest={test} onreload={()=>void load()}/>{/key}{:else if loading}<p class="empty">Загружаем профили…</p>{/if}</div>
  <div hidden={tab!=="metrics"}>
    <div class="privacy-section-heading"><div><p class="eyebrow">МЕТАДАННЫЕ БЕЗ СОДЕРЖИМОГО</p><h2>Рабочий трафик и лаборатория</h2><p class="muted">Раздельные счётчики с запуска процесса. Содержимое запросов и ответов сюда не записывается.</p></div><button onclick={()=>void refresh()}>Обновить показатели</button></div>
    {#if metrics}
      {#if metrics.traffic}<h3>Рабочий трафик</h3><div class="privacy-metrics"><article><small>Защищённые отправки</small><strong>{metrics.traffic.protected}</strong><span>каждая попытка, включая смену модели</span></article><article><small>Отправки без фильтра</small><strong>{metrics.traffic.bypassed}</strong><span>при включённом общем режиме</span></article><article class:has-errors={metrics.traffic.rejected>0}><small>Отклонённые запросы</small><strong>{metrics.traffic.rejected}</strong><span>конфигурация, вход или ответ</span></article><article><small>Проверенные ответы</small><strong>{metrics.traffic.restored}</strong><span>активных словарей: {metrics.traffic.active}</span></article></div><p class="help">При общем выключателе «выключено» обычный трафик не входит в эти счётчики. В защищённом режиме тела обмена не сохраняются в истории. Одновременно обрабатываются до 4 запросов; пределы: вход 32 MiB, ответ 16 MiB.</p>{/if}
      <h3>Локальная лаборатория</h3><div class="privacy-metrics"><article><small>Проверки маскирования</small><strong>{metrics.checks}</strong><span>включая отклонённые</span></article><article><small>Проверки восстановления</small><strong>{metrics.restores}</strong><span>ручные запуски</span></article><article class:has-errors={metrics.rejected>0}><small>Отклонённые проверки</small><strong>{metrics.rejected}</strong><span>ошибки входа, правил или ответа</span></article><article><small>Временные словари</small><strong>{metrics.active}<small> / 4</small></strong><span>до {metrics.ttlSeconds/60} минут в памяти</span></article></div>
      <div class="privacy-observability"><section class="panel"><h3>Какие значения заменялись</h3><p class="help">Количество вхождений, а не уникальных сущностей. Общий итог всех профилей и отдельных фильтров.</p>{#each Object.entries(metrics.masked).sort((a,b)=>b[1]-a[1]) as [kind,count]}<div class="privacy-meter"><span>{kindName(kind)}</span><meter min="0" max={Math.max(1,...Object.values(metrics.masked))} value={count} aria-label={kindName(kind)}></meter><strong>{count}</strong></div>{/each}{#if !Object.keys(metrics.masked).length}<div class="privacy-empty-chart"><span aria-hidden="true">▥</span><p>Пока нет замен</p><button class="text-button" onclick={()=>tab="lab"}>Проверить первый пример ↗</button></div>{/if}</section><section class="panel"><h3>Что здесь хранится</h3><ul class="privacy-facts"><li>Время, размер, число замен и результат проверки.</li><li>Последние 20 событий, только в памяти процесса.</li><li>Без текста, паролей, псевдонимов и словарей соответствий.</li></ul><div class="privacy-hint"><strong>Нулевое число замен ≠ отсутствие секретов</strong><p>Наблюдение показывает срабатывания настроенных правил. Полнота защиты зависит от словаря, контекста и поддержанных форматов.</p></div><p class="help">С начала: {new Date(metrics.started).toLocaleString("ru-RU")}. Обновление каждые 10 секунд.</p></section></div>
      <section class="panel privacy-events"><div class="spread"><h3>Последние проверки</h3><span class="badge">Без содержимого</span></div>{#if metrics.events.length}<div class="privacy-table-wrap"><table><thead><tr><th>Время</th><th>Операция</th><th>Результат</th><th>Размер входа</th><th>Замен</th><th>Время обработки</th></tr></thead><tbody>{#each metrics.events as e}<tr><td>{new Date(e.at).toLocaleTimeString("ru-RU")}</td><td>{e.operation==="mask" ? "Маскирование" : "Восстановление"}</td><td><span class="badge" class:danger={e.outcome==="rejected" || e.outcome==="needs_correction"}>{({ok:"Выполнено",rejected:"Отклонено",bypass:"Обход фильтра",needs_correction:"Нужно исправление"})[e.outcome] || e.outcome}</span></td><td>{bytes(e.inputBytes)}</td><td>{e.replacements}</td><td>{e.durationMs.toFixed(1)} мс</td></tr>{/each}</tbody></table></div>{:else}<p class="muted">Запустите проверку в лаборатории — её результат появится здесь.</p>{/if}</section>
    {/if}
  </div>
</div>
