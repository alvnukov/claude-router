<script lang="ts">
  import type { Session } from "./types";
  import { sessionTitle, sessionContext } from "./session";
  let { session, id = "" }: { session?: Session; id?: string } = $props();
  const title = $derived(sessionTitle(session, id));
  const context = $derived(sessionContext(session));
  const key = $derived(session?.id || id);
</script>

<span class="session-identity">
  <strong title={title}>{title}</strong>
  {#if context}<small class="session-context" title={context}>{context}</small>{/if}
  {#if session?.title && key}<small class="session-id" title={key}>{key.slice(0, 8)}</small>{/if}
</span>

<style>
  .session-identity { display: flex; flex-direction: column; gap: 3px; min-width: 0; text-align: left; }
  strong { display: -webkit-box; -webkit-box-orient: vertical; -webkit-line-clamp: 2; line-clamp: 2; overflow: hidden; overflow-wrap: anywhere; line-height: 1.4; }
  small { display: block; color: var(--muted); font-weight: 400; }
  .session-context { font-size: 11px; overflow-wrap: anywhere; }
  .session-id { font-size: 10px; font-family: ui-monospace, monospace; letter-spacing: 0.03em; }
</style>
