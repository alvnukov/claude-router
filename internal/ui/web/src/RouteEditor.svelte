<script lang="ts">
  import { untrack } from "svelte";
  import type { UIState, Route } from "./types";
  import { act, fail, targetLabel } from "./api";
  let {
    data,
    route,
    family = false,
  }: { data: UIState; route: Route; family?: boolean } = $props();
  const editingProfile = untrack(() => data.activeProfile);
  let picks = $state<Record<string, string>>({});
  let configured = $state<Record<string, boolean>>({});
  let changed = $state(false);
  let busy = $state(false);
  let hint = $state("");
  let picker: HTMLDialogElement;
  let editingEffort = $state("default");
  let selectedTarget = $state("disabled");
  let pickerAll = $state(false);
  let expanded = $state(false);
  const uniformTarget = $derived.by(() => {
    const base = picks.default || "";
    if (base === "inherit" && family && data.defaultPool && data.efforts.every((e) => picks[e] === base)) return base;
    if (base.startsWith("pool:") || base === "anthropic" || base === "disabled")
      return data.efforts.every((e) => picks[e] === base) ? base : "";
    if (!base.startsWith("model:") || !base.endsWith(":")) return "";
    const key = base.slice(6, -1);
    const model = data.models.find((m) => m.key === key);
    if (!model) return "";
    return data.efforts.every((e) => e === "default" || (
      model.efforts.includes(e)
        ? picks[e] === "model:" + key + ":" + e
        : picks[e] === "disabled" || (picks[e] === "inherit" &&
          (family || route.choices.find(c => c.effort === e)?.inherited === "disabled"))
    )) ? base : "";
  });
  const summaryEffort = $derived(
    uniformTarget.startsWith("pool:") || uniformTarget === "inherit"
      ? data.pools.find((p) => p.name === (uniformTarget === "inherit" ? data.defaultPool : uniformTarget.slice(5)))?.members.every((m) => m.effort === "request" && !Object.keys(m.effortMap || {}).length)
        ? "Effort запроса"
        : "Effort из настроек пула"
      : uniformTarget === "anthropic" ? "Effort запроса" : uniformTarget === "disabled" ? "Запросы отклоняются" : "Совпадающие effort",
  );
  function label(value: string) {
    if (value === "inherit" && family)
      return data.efforts.every(e => picks[e] === "inherit") ? "По умолчанию · " + (data.defaultPool || "отклонять") : "Не настроено";
    return targetLabel(value);
  }
  function choose(effort: string, all = false) {
    editingEffort = effort;
    pickerAll = all;
    selectedTarget = picks[effort] || "disabled";
    picker.showModal();
  }
  function applyChoice() {
    set(editingEffort, selectedTarget);
    if (pickerAll) fill(true);
    picker.close();
  }
  $effect(() => {
    if (!changed) {
      picks = Object.fromEntries(
        route.choices.map((c) => [c.effort, c.destination]),
      );
      configured = Object.fromEntries(route.choices.map(c => [c.effort, c.configured]));
    }
  });
  function set(effort: string, value: string) {
    changed = true;
    picks[effort] = value;
    configured[effort] = value !== "inherit";
    if (effort === "default" && (value.startsWith("model:") || value.startsWith("pool:"))) fill(false);
  }
  function fill(force: boolean) {
    changed = true;
    hint = "";
    const base = picks.default || "disabled";
    for (const effort of data.efforts) {
      if (
        effort === "default" ||
        (!force && configured[effort])
      )
        continue;
      if (base.startsWith("model:")) {
        const key = base.slice(6, base.lastIndexOf(":"));
        const model = data.models.find((m) => m.key === key);
        if (model?.efforts.includes(effort)) {
          picks[effort] = "model:" + key + ":" + effort;
          configured[effort] = true;
        } else {
          hint =
            "Некоторые уровни усилий не поддерживаются моделью. Их прежние назначения сохранены.";
        }
      } else {
        picks[effort] = base;
        configured[effort] = base !== "inherit";
      }
    }
  }
  async function save() {
    busy = true;
    try {
      await act("route.save", {
        profile: editingProfile,
        scope: family ? "family" : "model",
        model: route.model,
        ...Object.fromEntries(data.efforts.map(e => [e, configured[e] ? picks[e] : "inherit"])),
      });
      changed = false;
    } catch (error) {
      fail(error);
    } finally {
      busy = false;
    }
  }
</script>

<div class="route-editor" class:compact={uniformTarget && !expanded}>
  <div class="route-title">
    <h3>{route.label || route.model}</h3>
    <small>{family ? "Все версии семейства" : "Переопределение версии"}</small>
  </div>
  {#if uniformTarget && !expanded}
    <div class="route-summary">
      <span class="route-arrow" aria-hidden="true">→</span>
      <button class="route-cell" aria-label={"Назначение " + (route.label || route.model)}
        aria-haspopup="dialog" disabled={busy} onclick={() => choose("default", true)}>
        <strong>{label(uniformTarget)}</strong><span aria-hidden="true">↗</span>
      </button>
      <span class="badge">{summaryEffort}</span>
    </div>
  {:else}
  <div class="route-cells">
    {#each route.choices as choice}
      <div class="route-cell-wrap">
        <span class="effort-label"
          >{choice.effort === "default" ? "Без effort" : choice.effort}</span
        >
        <button
          class="route-cell"
          class:unassigned={picks[choice.effort] === "disabled"}
          aria-label={(route.label || route.model) + " / " + choice.effort}
          title={label(picks[choice.effort] ?? choice.destination)}
          aria-haspopup="dialog"
          disabled={busy}
          onclick={() => choose(choice.effort)}
        >
          <strong
            >{label(picks[choice.effort] ?? choice.destination)}</strong
          >
          <span aria-hidden="true">↗</span>
        </button>
      </div>
    {/each}
  </div>
  {/if}
  <div class="route-footer">
    {#if changed || !uniformTarget || expanded}<button disabled={busy || !changed} class="primary" onclick={save}
      >{busy ? "Сохраняем…" : "Сохранить"}</button
    >{/if}{#if !uniformTarget || expanded}<button disabled={busy} onclick={() => fill(true)}
      >Применить ко всем effort</button
    >{/if}
    {#if uniformTarget}<button disabled={busy} onclick={() => expanded = !expanded}>
      {expanded ? "Свернуть сопоставление" : "Настроить по effort"}</button>{/if}
    {#if changed}<button disabled={busy} onclick={() => { changed = false; hint = ""; }}>Отменить изменения</button>
      <span class="badge">Есть несохранённые изменения</span>{/if}
  </div>
  {#if hint}<p class="help">{hint}</p>{/if}
</div>

<dialog bind:this={picker} aria-label="Выбор цели маршрута">
  <div class="spread">
    <h2>
      {route.label || route.model} · {pickerAll ? "все effort" : editingEffort === "default"
        ? "без effort"
        : editingEffort}
    </h2>
    <button
      type="button"
      aria-label="Закрыть выбор цели"
      onclick={() => picker.close()}>✕</button
    >
  </div>
  <p class="subtitle">{pickerAll ? "Куда направлять запросы всех уровней усилий." : "Куда направлять запросы этого уровня усилий."}</p>
  <label class="target-picker"
    >Цель маршрута
    <select bind:value={selectedTarget}>
      <option value="inherit">{family ? "Не задавать правило" : "Наследовать маршрут семейства"}</option>
      <option value="disabled">Не настроено</option>
      <option value="anthropic">Anthropic</option>
      {#if data.pools.length}<optgroup label="Пулы"
          >{#each data.pools as pool}<option value={"pool:" + pool.name}
              >{pool.name}</option
            >{/each}</optgroup
        >{/if}
      {#each data.models as model}
        <optgroup label={model.key}>
          <option value={"model:" + model.key + ":"}
            >{model.key} · усилие по умолчанию</option
          >
          {#each model.efforts as effort}<option
              value={"model:" + model.key + ":" + effort}
              >{model.key} · {effort}</option
            >{/each}
        </optgroup>
      {/each}
    </select>
  </label>
  {#if family}<p class="help">Когда у семейства нет ни одного правила, действует пул по умолчанию.
    При отдельных правилах незаданные уровни отклоняются.</p>{/if}
  {#if pickerAll}<p class="help">Новое назначение применяется ко всем поддерживаемым уровням.
    У пула усилие определяется настройкой каждого участника.</p>
  {:else if editingEffort === "default"}<p class="help">
      Выбор модели или пула заполнит свободные уровни. Уже заданные назначения сохранятся.
      У модели усилия совпадут; у пула они определяются настройкой каждого участника.
    </p>{/if}
  <div class="actions">
    <button type="button" onclick={() => picker.close()}>Отмена</button>
    <button type="button" class="primary" onclick={applyChoice}
      >Выбрать цель</button
    >
  </div>
</dialog>
