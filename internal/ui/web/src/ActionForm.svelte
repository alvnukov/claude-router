<script lang="ts">
  import type { Snippet } from "svelte";
  import { act, fail } from "./api";
  let {
    action,
    fields = {},
    confirm = "",
    class: className = "",
    children,
    ondone,
  }: {
    action: string;
    fields?: Record<string, string>;
    confirm?: string;
    class?: string;
    children: Snippet;
    ondone?: () => void;
  } = $props();
  let busy = $state(false);
  let dialog = $state<HTMLDialogElement>();
  let pending: Record<string, string> = {};
  async function execute() {
    dialog?.close();
    busy = true;
    try {
      await act(action, pending);
      ondone?.();
    } catch (error) {
      fail(error);
    } finally {
      busy = false;
    }
  }
  function submit(event: SubmitEvent) {
    event.preventDefault();
    const form = event.currentTarget;
    if (!(form instanceof HTMLFormElement)) return;
    pending = { ...fields };
    for (const [key, value] of new FormData(form, event.submitter))
      if (typeof value === "string") pending[key] = value;
    const baseURL = form.elements.namedItem("base_url");
    if (
      fields.op === "update" &&
      baseURL instanceof HTMLInputElement &&
      baseURL.value === baseURL.dataset.originalUrl
    )
      delete pending.base_url;
    if (confirm) dialog?.showModal();
    else void execute();
  }
</script>

<form class={className} onsubmit={submit} aria-busy={busy}>
  <fieldset disabled={busy}>{@render children()}</fieldset>
</form>
{#if confirm}<dialog bind:this={dialog} aria-label="Подтверждение действия">
    <h2>Подтвердите действие</h2>
    <p>{confirm}</p>
    <div class="actions">
      <button type="button" onclick={() => dialog?.close()}>Отмена</button
      ><button type="button" class="primary" onclick={execute}
        >Подтвердить</button
      >
    </div>
  </dialog>{/if}
