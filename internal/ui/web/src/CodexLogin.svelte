<script lang="ts">
  import type { Connection } from "./types";
  import { act } from "./api";
  let { connection }: { connection: Connection } = $props();
  let busy = $state(false);
  let loginURL = $state("");
  let error = $state("");
  let dialog: HTMLDialogElement;

  async function login() {
    if (busy) return;
    busy = true;
    error = "";
    loginURL = "";
    // Open during the click, before awaiting the API, to retain user activation.
    let popup: Window | null = null;
    try {
      popup = window.open("about:blank", "_blank");
      if (popup) {
        popup.opener = null;
        popup.document.title = "Вход с ChatGPT";
        popup.document.body.textContent = "Открываем страницу входа в ChatGPT…";
      }
      const result = await act("codex.login", { provider: connection.name });
      if (!result.url) throw new Error("Роутер не вернул ссылку для входа. Повторите попытку.");
      const target = new URL(result.url, location.origin);
      if (target.protocol !== "https:" && target.origin !== location.origin)
        throw new Error("Роутер вернул недопустимый адрес входа.");
      loginURL = target.href;
      if (popup && !popup.closed) popup.location.replace(loginURL);
      else dialog.showModal();
    } catch (problem) {
      popup?.close();
      error = problem instanceof Error ? problem.message : "Не удалось начать вход. Попробуйте ещё раз.";
      dialog.showModal();
    } finally {
      busy = false;
    }
  }
</script>

<button type="button" class="primary" disabled={busy} onclick={login}>
  {busy ? "Открываем вход…" : "Continue with ChatGPT"}
</button>
<dialog bind:this={dialog} aria-label={"Вход с ChatGPT · " + connection.displayName}>
  <h2>Вход с ChatGPT · {connection.displayName}</h2>
  {#if error}<p class="danger" role="alert">{error}</p>
  {:else}<p>Браузер не открыл новую вкладку. Нажмите кнопку, чтобы продолжить вход.</p>{/if}
  <div class="actions">
    <button type="button" onclick={() => dialog.close()}>Закрыть</button>
    {#if loginURL}<a class="button primary" href={loginURL} target="_blank" rel="noopener noreferrer"
      >Continue with ChatGPT ↗</a>{/if}
  </div>
</dialog>
