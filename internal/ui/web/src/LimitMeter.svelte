<script lang="ts">
  import type { Limit } from "./types";
  import { resetIn } from "./api";
  let { limit, now }: { limit: Limit; now: string } = $props();
</script>

<div
  class="limit"
  class:danger={limit.blocked || (limit.known && limit.remaining <= 15)}
>
  <div class="spread">
    <span
      >{limit.label === "unified-5h"
        ? "За 5 часов"
        : limit.label === "unified-7d"
          ? "За неделю"
          : limit.label === "primary"
            ? "Основной лимит"
            : limit.label === "secondary"
              ? "Дополнительный лимит"
              : limit.label}</span
    ><strong
      >{limit.known ? Math.round(limit.remaining) + "%" : "Неизвестно"}</strong
    >
  </div>
  {#if limit.known}<progress
      max="100"
      value={limit.remaining}
      aria-label={"Остаток лимита " + limit.label}
    ></progress>{/if}<small
    >{limit.blocked ? "Лимит исчерпан · " : ""}{resetIn(
      limit.reset,
      now,
    )}</small
  >
</div>
