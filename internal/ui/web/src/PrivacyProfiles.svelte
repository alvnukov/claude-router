<script lang="ts">
  import { onMount } from "svelte";
  import type { UIState } from "./types";
  import { demoRules, detectDescriptions, detectNotice, editableRules, filters, privacyAPI } from "./privacy";
  import type { FilterProfile, FilterRules, PrivacyConfig, PrivacyOperation, ProfileSnapshot } from "./privacy";
  let { snapshot, data, onsaved, ontest, onreload }: { snapshot:ProfileSnapshot; data:UIState; onsaved:(s:ProfileSnapshot)=>void; ontest:(p:FilterProfile,f:string)=>void; onreload:()=>void } = $props();
  let config = $state<PrivacyConfig>({version:1,enabled:false,default:"",profiles:[],bindings:[]});
  let selected = $state("");
  let rulesText = $state("{}");
  let dirty = $state(false);
  let error = $state("");
  let message = $state("");
  let saving = $state(false);
  let validating = $state(false);
  let inspectModel = $state("");
  let inspectPool = $state("");
  let inspectProvider = $state("");
  let deleted = $state<FilterProfile | null>(null);
  const current = $derived(config.profiles.find(p=>p.id===selected));
  const previewResolution = $derived.by(()=>{
    for(const [kind,value] of [["model",inspectModel],["pool",inspectPool],["provider",inspectProvider]]) {
      const b=config.bindings.find(b=>b.kind===kind && b.target===value);
      if(b) return {profile:config.profiles.find(p=>p.id===b.profile),via:{model:"модель",pool:"пул",provider:"провайдер"}[kind]};
    }
    return {profile:config.profiles.find(p=>p.id===config.default),via:"по умолчанию"};
  });
  onMount(()=>{ if(snapshot.config) config=structuredClone($state.snapshot(snapshot.config)); selected=config.profiles[0]?.id || ""; syncText(); });
  function syncText() { rulesText=JSON.stringify(config.profiles.find(p=>p.id===selected)?.rules || {},null,2); }
  function mark() { dirty=true; message=""; }
  async function commitRules() {
    if(!current) return true;
    const profile=current;
    if(rulesText===JSON.stringify(profile.rules,null,2)) return true;
    validating=true;
    try {
      const rules=await privacyAPI<FilterRules>("/validate",{rules:rulesText});
      if(!editableRules(rules)) throw new Error("Некорректная форма правил");
      profile.rules=rules; error=""; return true;
    } catch(e) { error=e instanceof Error ? e.message : "Правила не приняты. Исправьте JSON в редакторе."; return false; }
    finally { validating=false; }
  }
  async function choose(id:string) { if(!await commitRules()) return; selected=id; syncText(); }
  async function create(copy=false) {
    if(!await commitRules()) return;
    const id="profile-"+crypto.randomUUID().slice(0,8);
    const p:FilterProfile={id,name:copy && current ? current.name+" · копия" : "Новый профиль",enabled:true,mode:copy && current ? current.mode || "mask" : "mask",rules:copy && current ? structuredClone($state.snapshot(current.rules)) : {}};
    config.profiles.push(p); selected=id; syncText(); mark();
  }
  function remove() {
    if(!current) return;
    if(config.default===current.id || config.bindings.some(b=>b.profile===current?.id)) { error="Сначала снимите назначения и выбор по умолчанию. Используемый профиль удалить нельзя."; return; }
    deleted=structuredClone($state.snapshot(current)); config.profiles=config.profiles.filter(p=>p.id!==selected); selected=config.profiles[0]?.id || ""; syncText(); mark(); error="";
  }
  function undoRemove() { if(saving || validating) return; if(deleted) { config.profiles.push(deleted); selected=deleted.id; deleted=null; syncText(); mark(); } }
  async function toggle(id:string,control:HTMLInputElement) {
    const value=control.checked;
    if(!await commitRules() || !current) { control.checked=current?.rules.filters?.[id]!==false; return; }
    current.rules.filters={...current.rules.filters,[id]:value}; syncText(); mark();
  }
  async function listField(field:"domains"|"networks",control:HTMLInputElement) {
    const value=control.value;
    if(!await commitRules() || !current) { control.value=(current?.rules[field] || []).join(", "); return; }
    current.rules[field]=value.split(/[,\n]/).map(s=>s.trim()).filter(Boolean); syncText(); mark();
  }
  async function test(filter="") { if(await commitRules() && current) ontest(structuredClone($state.snapshot(current)),filter); }
  async function importRules() {
    validating=true;
    try { const rules=await privacyAPI<FilterRules>("/rules"); if(current) { current.rules=rules; syncText(); mark(); message="Правила privacy.json загружены в черновик. Сохранение — отдельным действием."; } }
    catch(e) { error=e instanceof Error ? e.message : "Правила недоступны"; }
    finally { validating=false; }
  }
  async function save() {
    if(!await commitRules()) return;
    error=""; saving=true;
    try { const result=await privacyAPI<ProfileSnapshot>("/config",{revision:snapshot.revision,config}); dirty=false; message=""; onsaved(result); }
    catch(e) { error=e instanceof Error ? e.message : "Не удалось сохранить профили"; }
    finally { saving=false; }
  }
  function targets(kind:string): {value:string;label:string}[] {
    if(kind==="model") return [...new Set([...data.models.map(m=>m.key),...data.routes.map(r=>r.model)])].map(value=>({value,label:value}));
    if(kind==="pool") return data.profiles.flatMap(p=>Object.keys(p.modelPools || {}).map(pool=>({value:p.name+"/"+pool,label:p.name+" / "+pool})));
    return [{value:"anthropic",label:"Anthropic (прямой маршрут)"},...data.connections.map(c=>({value:c.name,label:c.displayName || c.name}))];
  }
</script>

<section aria-label="Профили фильтров" class="privacy-profiles">
  <div class="privacy-section-heading"><div><p class="eyebrow">НАСТРОЙКИ В ФАЙЛЕ</p><h2>Профили фильтров</h2><p class="muted">Независимые наборы правил. Изменения сначала остаются в черновике.</p></div><div class="actions"><button disabled={saving || validating} onclick={onreload}>{dirty ? "Отменить правки и перечитать" : "Перечитать файл"}</button><button class="primary" disabled={saving || validating || !dirty || snapshot.status==="unavailable"} onclick={save}>{saving ? "Сохраняем…" : "Сохранить профили"}</button></div></div>
  {#if error}<p class="alert danger" role="alert">{error}</p>{/if}
  {#if message}<p class="notice" role="status">{message}</p>{/if}
  {#if deleted}<p class="notice" role="status">Профиль удалён из черновика. <button class="text-button" disabled={saving || validating} onclick={undoRemove}>Отменить удаление</button></p>{/if}
  <fieldset disabled={saving || validating} class="privacy-fieldset">
    <div class="privacy-config-bar"><label class="check"><input type="checkbox" bind:checked={config.enabled} onchange={mark}/>Использовать назначения профилей</label><label>Профиль по умолчанию<select aria-label="Профиль по умолчанию" bind:value={config.default} onchange={mark}><option value="">Не назначен · без фильтра</option>{#each config.profiles as p}<option value={p.id}>{p.name}{p.mode==="detect" ? " · только детект" : ""}</option>{/each}</select></label><span class="badge">{dirty ? "Есть несохранённые правки" : "Настройки загружены"}</span></div>
    <p class="help">Общий выключатель включает применение назначений к новым запросам. Выключенный профиль или отсутствие назначения означает передачу без маскирования. В лаборатории профиль выбирается вручную.</p>
    <div class="privacy-profile-layout">
      <aside class="privacy-profile-nav" aria-label="Выбор профиля фильтров"><div class="spread"><strong>Ваши профили</strong><span class="muted">{config.profiles.length}/32</span></div>{#each config.profiles as p}<button class:chosen={selected===p.id} onclick={()=>choose(p.id)} aria-pressed={selected===p.id}><span>{p.name}</span><small>{p.enabled ? "Включён" : "Выключен"} · {p.mode==="detect" ? "только детект" : "маскирование"}{config.default===p.id ? " · основной" : ""}</small></button>{/each}<button disabled={config.profiles.length>=32} onclick={()=>create()}>+ Новый профиль</button></aside>
      <div class="privacy-profile-detail">
        {#if current}
          <div class="privacy-profile-title"><label>Название профиля<input aria-label="Название профиля фильтров" bind:value={current.name} oninput={mark} maxlength="160"/></label><label class="check"><input type="checkbox" bind:checked={current.enabled} onchange={mark}/>Профиль включён</label><button onclick={()=>test()}>Проверить профиль ↗</button></div>
          <div class="privacy-profile-mode"><label>Режим профиля<select aria-label="Режим профиля" aria-describedby="privacy-profile-mode-help" value={current.mode || "mask"} onchange={e=>{if(current) {current.mode=e.currentTarget.value as PrivacyOperation;mark();}}}><option value="mask">Маскирование и восстановление</option><option value="detect">Только детект · без защиты</option></select></label><p id="privacy-profile-mode-help" class="help">{current.mode==="detect" ? "Считает срабатывания выбранных правил. Данные не заменяются, словарь восстановления не создаётся." : "Заменяет найденные значения перед отправкой и восстанавливает ответ локально."}</p></div>
          {#if current.mode==="detect"}<div class="privacy-detect-warning" role="note"><strong>Только детект · данные открыты</strong><p>{detectNotice}</p><small>Срабатывания зависят от настроенных правил. Содержимое вложений не анализируется; отсутствие совпадений не доказывает отсутствие секретов.</small></div>{/if}
          {#if !current.enabled}<p class="privacy-hint">Выключенный профиль означает обход маскирования и демаскирования для назначенных ему запросов. Уже выданные псевдонимы восстанавливаются прежним снимком.</p>{/if}
          <div class="privacy-filter-grid">{#each filters as f}<article class="privacy-filter" class:filter-off={current.rules.filters?.[f.id]===false}><div class="spread"><span class="eyebrow">{f.group}</span><label class="privacy-toggle"><span class="sr">{f.name}</span><input type="checkbox" aria-label={"Фильтр: "+f.name} checked={current.rules.filters?.[f.id]!==false} onchange={e=>toggle(f.id,e.currentTarget)}/><span aria-hidden="true"></span></label></div><h3>{f.name}</h3><p>{current.mode==="detect" ? detectDescriptions[f.id] : f.description}</p><button class="text-button" onclick={()=>test(f.id)}>Проверить отдельно ↗<span class="sr"> {f.name}</span></button></article>{/each}</div>
          <div class="privacy-controls"><label>Внутренние домены<input value={(current.rules.domains || []).join(", ")} onchange={e=>listField("domains",e.currentTarget)} placeholder="example.internal, corp.example"/><small>Общие для фильтров доменов и почты</small></label><label>Публичные сети организации<input value={(current.rules.networks || []).join(", ")} onchange={e=>listField("networks",e.currentTarget)} placeholder="203.0.113.0/24"/><small>Через запятую, IPv4 и IPv6 с длиной префикса</small></label></div>
          <details class="privacy-rules"><summary>Словарь, шаблоны и расширенные правила · JSON</summary><p class="help">Полный объект правил: entries (kind, forms), patterns (name, kind, regex, group), fields (path, kind), allow и allow_paths. Исключения оставляют значения открытыми. Регулярные выражения — RE2. JSON проверяется при сохранении и запуске теста.</p><label for="privacy-rules-json">Правила профиля</label><textarea id="privacy-rules-json" class="privacy-code" bind:value={rulesText} oninput={mark} spellcheck="false" autocomplete="off"></textarea><div class="actions"><button onclick={async()=>{if(await commitRules()) {syncText();message="JSON разобран. Полная проверка — при сохранении или тесте.";}}}>Применить JSON к редактору</button><button onclick={importRules}>Импортировать privacy.json</button><button onclick={()=>{if(current) {current.rules=structuredClone(demoRules);syncText();mark();}}}>Заполнить демонстрационными правилами</button></div><p class="help">Сохранённые словари содержат реальные значения. Файл privacy-profiles.json создаётся рядом с providers.json с правами владельца.</p></details>
          <div class="privacy-profile-actions"><small class="muted">ID: {current.id}</small><div class="actions"><button disabled={config.profiles.length>=32} onclick={()=>create(true)}>Дублировать профиль</button><button class="text-button danger" onclick={remove}>Удалить профиль</button></div></div>
        {:else}<div class="empty"><h3>Первый профиль — под вашу задачу</h3><p>Создайте набор правил, проверьте его на примере и назначьте нужным моделям.</p><button class="primary" onclick={()=>create()}>Создать профиль</button></div>{/if}
      </div>
    </div>
    <section class="privacy-assignments" aria-label="Назначения фильтров"><div class="privacy-section-heading"><div><h2>Где использовать профиль</h2><p class="muted">Приоритет: модель → пул → провайдер → профиль по умолчанию. Правила не смешиваются.</p></div><button disabled={!config.profiles.length || config.bindings.length>=256} onclick={()=>{config.bindings.push({kind:"provider",target:targets("provider")[0]?.value || "",profile:current?.id || config.profiles[0].id});mark();}}>+ Назначение</button></div>
      {#each config.bindings as b,i}<div class="privacy-binding"><label>Область<select aria-label={"Область назначения "+(i+1)} bind:value={b.kind} onchange={()=>{b.target=targets(b.kind)[0]?.value || "";mark();}}><option value="provider">Провайдер</option><option value="pool">Пул</option><option value="model">Модель</option></select></label><label>Получатель<select aria-label={"Получатель назначения "+(i+1)} bind:value={b.target} onchange={mark}><option value="" disabled>Выберите получателя</option>{#if b.target && !targets(b.kind).some(t=>t.value===b.target)}<option value={b.target}>{b.target} · отсутствует в каталоге</option>{/if}{#each targets(b.kind) as t}<option value={t.value}>{t.label}</option>{/each}</select></label><label>Профиль<select aria-label={"Профиль назначения "+(i+1)} bind:value={b.profile} onchange={mark}>{#each config.profiles as p}<option value={p.id}>{p.name}{p.mode==="detect" ? " · только детект" : ""}{p.enabled ? "" : " · выключен"}</option>{/each}</select></label><button class="text-button danger" aria-label={"Убрать назначение "+(i+1)} onclick={()=>{config.bindings.splice(i,1);mark();}}>Убрать</button></div>{/each}
      {#if !config.bindings.length}<p class="privacy-hint">Индивидуальных назначений пока нет. Используется выбор по умолчанию, если общий выключатель включён.</p>{/if}
      <details class="privacy-resolve"><summary>Проверить, какой профиль будет выбран</summary><p class="help">Проверка выбора по текущему черновику. Это не отправляет запрос и не включает защиту трафика. При смене модели из-за ошибки назначение нужно определять заново.</p><div class="privacy-controls"><label>Модель<select aria-label="Модель для выбора профиля" bind:value={inspectModel}><option value="">Не указана</option>{#each targets("model") as t}<option value={t.value}>{t.label}</option>{/each}</select></label><label>Пул<select aria-label="Пул для выбора профиля" bind:value={inspectPool}><option value="">Не указан</option>{#each targets("pool") as t}<option value={t.value}>{t.label}</option>{/each}</select></label><label>Провайдер<select aria-label="Провайдер для выбора профиля" bind:value={inspectProvider}><option value="">Не указан</option>{#each targets("provider") as t}<option value={t.value}>{t.label}</option>{/each}</select></label></div><p role="status"><strong>{previewResolution.profile?.name || "Профиль не назначен"}</strong> · {previewResolution.via} · {config.enabled && previewResolution.profile?.enabled ? previewResolution.profile.mode==="detect" ? "только детект · данные открыты" : "маскирование включено" : "без маскирования"}</p></details>
    </section>
  </fieldset>
</section>
