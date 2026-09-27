<script lang="ts">
  import { onMount } from "svelte";
  import { bytes, demoProfile, examples, filters, kindName, privacyAPI } from "./privacy";
  import type { FilterProfile, PreviewResult } from "./privacy";
  let { profiles, initial, onresult }: { profiles: FilterProfile[]; initial: {profile:FilterProfile; filter:string} | null; onresult:()=>void } = $props();
  let chosen = $state("builtin:demo");
  let only = $state("");
  let mode = $state("text");
  let input = $state<string>(examples[0].input);
  let masked = $state<PreviewResult | null>(null);
  let response = $state("");
  let restored = $state<PreviewResult | null>(null);
  let busy = $state("");
  let error = $state("");
  let now = $state(Date.now());
  let alive = true;
  const abort = new AbortController();
  let temporary = $state<FilterProfile | null>(null);
  let maskedLabel = $state("");
  const selected = $derived(chosen === "builtin:demo" ? demoProfile : chosen === "draft:temporary" ? temporary : profiles.find(p=>"profile:"+p.id === chosen) || null);
  const remaining = $derived(masked ? Math.max(0, Math.ceil((Date.parse(masked.expires)-now)/1000)) : 0);
  const inputBytes = $derived(new TextEncoder().encode(input).length);
  const hasSecret = $derived(/(?:<|\\u003c)secret:/i.test(response));
  function forget(id: string) { if (id) void privacyAPI("/clear",{id}).catch(()=>{}); }
  function invalidate() {
    if (masked) forget(masked.id);
    masked = null; response = ""; restored = null; error = "";
  }
  function example(id: string) {
    const e = examples.find(e=>e.id === id);
    if (!e) return;
    invalidate(); mode = e.mode; input = e.input;
  }
  function clear() { invalidate(); input = ""; onresult(); }
  async function mask() {
    invalidate(); busy = "mask";
    try {
      if (!selected) throw new Error("Профиль больше не доступен. Выберите другой профиль.");
      maskedLabel = selected.name + (only ? " · только " + filters.find(f=>f.id===only)?.name : " · весь профиль");
      const result = await privacyAPI<PreviewResult>("/preview",{mode,input,rules:selected.rules,enabled:selected.enabled,filter:only},abort.signal);
      if (!alive) { forget(result.id); return; }
      masked = result; response = result.output;
    } catch(e) { if(alive) error = e instanceof Error ? e.message : "Проверка не выполнена"; }
    finally { busy = ""; if(alive) onresult(); }
  }
  async function restore() {
    if (!masked) return;
    busy = "restore"; error = ""; restored = null;
    try { const result = await privacyAPI<PreviewResult>("/restore",{id:masked.id,input:response},abort.signal); if(alive) restored = result; }
    catch(e) { if(alive) error = e instanceof Error ? e.message : "Восстановление не выполнено"; }
    finally { busy = ""; if(alive) onresult(); }
  }
  function corrupt() {
    // Deliberate demonstration only; never modify the user's response implicitly.
    response = response.replace(/(<|\\u003c)secret:([^:>]+):([a-f0-9]{8})(>|\\u003e)/i, (_all,start:string,family:string,hash:string,end:string)=>`${start}secret:${family}:${hash[0] === "0" ? "1" : "0"}${hash.slice(1)}${end}`);
    restored = null;
  }
  onMount(()=>{
    if(initial) { temporary = structuredClone($state.snapshot(initial.profile)); chosen = "draft:temporary"; only = initial.filter; }
    const tick = window.setInterval(()=>now = Date.now(),1000);
    return ()=>{ alive = false; abort.abort(); clearInterval(tick); if(masked) forget(masked.id); };
  });
</script>

<section class="privacy-lab" aria-label="Лаборатория фильтров">
  <div class="privacy-section-heading">
    <div><p class="eyebrow">ЛОКАЛЬНАЯ ЛАБОРАТОРИЯ</p><h2>Проверьте весь путь данных</h2><p class="muted">Текст → псевдонимы → восстановление. Без вызова модели и выполнения команд.</p></div>
    <button disabled={!!busy} onclick={clear}>Очистить данные</button>
  </div>
  <fieldset disabled={!!busy} class="privacy-controls">
    <label>Профиль для проверки<select aria-label="Профиль для проверки" bind:value={chosen} onchange={invalidate}><option value="builtin:demo">Демонстрационный · вымышленные данные</option>{#if temporary}<option value="draft:temporary">{temporary.name} · текущий черновик</option>{/if}{#each profiles as p}<option value={"profile:"+p.id}>{p.name}{p.enabled ? "" : " · выключен"}</option>{/each}</select></label>
    <label>Объём проверки<select aria-label="Объём проверки" bind:value={only} onchange={invalidate}><option value="">Весь профиль</option>{#each filters as f}<option value={f.id}>Только: {f.name}</option>{/each}</select></label>
    <label>Формат<select aria-label="Формат" bind:value={mode} onchange={invalidate}><option value="text">Обычный текст</option><option value="json">Anthropic Messages JSON</option></select></label>
  </fieldset>
  {#if only}<p class="privacy-hint">Изолированная проверка: включён только фильтр «{filters.find(f=>f.id===only)?.name}». Настройки профиля не изменяются. Остальные типы данных могут остаться открытыми.</p>{:else if selected && !selected.enabled}<p class="alert danger">Профиль выключен: проверка покажет передачу данных без маскирования.</p>{/if}
  <div class="privacy-examples"><span class="muted">Начать с примера</span>{#each examples as e}<button class="text-button" disabled={!!busy} onclick={()=>example(e.id)}>{e.label} ↗</button>{/each}</div>
  <div class="privacy-editors">
    <section class="privacy-editor">
      <div class="privacy-editor-head"><span class="privacy-step">01</span><div><h3>Исходные данные</h3><small>Остаются на этой машине</small></div><span class:danger={inputBytes>262144}>{bytes(inputBytes)}</span></div>
      <label class="sr" for="privacy-input">Исходные данные</label>
      <textarea id="privacy-input" class="privacy-code" bind:value={input} disabled={!!busy} oninput={invalidate} spellcheck="false" autocomplete="off" autocapitalize="off" placeholder="Вставьте текст или JSON для проверки…"></textarea>
      <div class="privacy-editor-foot"><small>До 256 КиБ · без обрезки</small><button class="primary" disabled={!!busy || !input || inputBytes>262144} onclick={mask}>{busy==="mask" ? "Маскируем…" : "Маскировать →"}</button></div>
    </section>
    <section class="privacy-editor privacy-masked">
      <div class="privacy-editor-head"><span class="privacy-step">02</span><div><h3>После фильтра</h3><small>Таким был бы вход модели</small></div>{#if masked}<span>{bytes(masked.outputBytes)}</span>{/if}</div>
      {#if masked}<label class="sr" for="privacy-masked">Маскированный текст</label><textarea id="privacy-masked" class="privacy-code" readonly value={masked.output} spellcheck="false"></textarea>{:else}<div class="privacy-placeholder"><span aria-hidden="true">◈</span><strong>Здесь появятся псевдонимы</strong><p>Связи внутри примера сохраняются.<br/>Реальные значения восстанавливаются локально.</p></div>{/if}
      <div class="privacy-editor-foot"><small>{masked ? `${masked.durationMs.toFixed(1)} мс · ${Object.values(masked.masked).reduce((a,b)=>a+b,0)} замен` : "Правила применяются одним снимком"}</small>{#if masked}<span class="badge" class:danger={!masked.roundtrip || !masked.enabled}>{!masked.enabled ? "Обход фильтра" : masked.roundtrip ? "Обратная проверка точна" : "Есть невосстановимые изменения"}</span>{/if}</div>
    </section>
  </div>
  {#if masked}
    <p class="help">Снимок: <strong>{maskedLabel}</strong>. Последующие изменения настроек не меняют словарь этого ответа.</p>
    <div class="privacy-kind-list">{#each Object.entries(masked.masked) as [kind,count]}<span class="badge">{kindName(kind)} <strong>{count}</strong></span>{/each}{#if !Object.keys(masked.masked).length}<span class="muted">Совпадений нет. Это не подтверждает отсутствие чувствительных данных.</span>{/if}</div>
    <p class="help">Точное восстановление проверяет обратимость найденных замен. Полноту обнаружения проверьте по содержимому: неизвестные имена, нестандартные секреты и косвенные признаки могут остаться видимыми.</p>
    <div class="privacy-section-heading"><div><p class="eyebrow">ПРОВЕРКА ОТВЕТА</p><h2>Дайте модели право на ошибку</h2><p class="muted">Измените ответ вручную. Фильтр не угадывает значения и не исправляет JSON.</p></div><span class="badge" class:danger={!remaining}>{remaining ? `Словарь: ${Math.floor(remaining/60)}:${String(remaining%60).padStart(2,"0")}` : "Словарь истёк"}</span></div>
    <div class="privacy-editors">
      <section class="privacy-editor">
        <div class="privacy-editor-head"><span class="privacy-step">03</span><div><h3>Ответ модели</h3><small>Редактируемая имитация</small></div><button class="text-button" disabled={!!busy || !hasSecret} onclick={corrupt}>Внести ошибку в секрет</button></div>
        <label class="sr" for="privacy-response">Ответ модели</label><textarea id="privacy-response" class="privacy-code" bind:value={response} oninput={()=>{restored=null;error="";}} disabled={!!busy} spellcheck="false" autocomplete="off" autocapitalize="off"></textarea>
        <div class="privacy-editor-foot"><button class="text-button" disabled={!!busy} onclick={()=>{response=masked?.output || ""; restored=null;error="";}}>Вернуть точный ответ</button><button class="primary" disabled={!!busy || !remaining} onclick={restore}>{busy==="restore" ? "Восстанавливаем…" : "Демаскировать →"}</button></div>
      </section>
      <section class="privacy-editor">
        <div class="privacy-editor-head"><span class="privacy-step">04</span><div><h3>Восстановленный ответ</h3><small>Результат для локальной стороны</small></div></div>
        {#if restored}<label class="sr" for="privacy-restored">Восстановленный ответ</label><textarea id="privacy-restored" class="privacy-code" readonly value={restored.output} spellcheck="false"></textarea>{:else}<div class="privacy-placeholder"><span aria-hidden="true">↩</span><strong>Только однозначные соответствия</strong><p>Для проверки строгого отказа используйте<br/>пример «Аргументы инструмента».</p></div>{/if}
        <div class="privacy-editor-foot"><small>{restored ? `${restored.durationMs.toFixed(1)} мс · ${Object.values(restored.unmasked).reduce((a,b)=>a+b,0)} восстановлений` : "Тот же словарь, что на первом шаге"}</small>{#if restored}<span class="badge" class:danger={restored.unexpected>0}>{restored.unexpected ? `Требуют исправления: ${restored.unexpected}` : "Обработка завершена"}</span>{/if}</div>
      </section>
    </div>
    {#if restored?.unexpected}<p class="alert danger" role="status">В свободном тексте неизвестные маркеры оставлены без изменения. В типизированных аргументах инструмента такие значения вызывают отказ. Исправьте ответ и повторите проверку.</p>{/if}
  {/if}
  {#if error}<p class="alert danger" role="alert">{error}</p>{/if}
  <p class="help privacy-local-note">Тексты не записываются в историю и хранилище браузера. Серверный словарь живёт 5 минут; очистка или уход со страницы освобождает его раньше. Отключение отдельных фильтров может оставить чувствительные данные открытыми.</p>
</section>
