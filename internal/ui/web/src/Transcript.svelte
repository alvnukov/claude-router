<script lang="ts">
  let {
    content,
    empty = "Содержимое не записано",
  }: { content: string; empty?: string } = $props();
  interface Part {
    role: string;
    text: string;
    tool: string;
  }
  function object(value: unknown): value is Record<string, unknown> {
    return typeof value === "object" && value !== null && !Array.isArray(value);
  }
  function parse(content: string): Part[] {
    if (!content) return [];
    let value: unknown;
    try {
      value = JSON.parse(content);
    } catch {
      return [{ role: "Текст", text: content, tool: "" }];
    }
    const parts: Part[] = [];
    function collect(value: unknown, role: string, depth = 0): void {
      if (depth > 12 || parts.length > 200) return;
      if (typeof value === "string") {
        if (value) parts.push({ role, text: value, tool: "" });
        return;
      }
      if (Array.isArray(value)) {
        for (const item of value) collect(item, role, depth + 1);
        return;
      }
      if (!object(value)) return;
      const who = typeof value.role === "string" ? value.role : role;
      if (
        value.type === "tool_use" ||
        value.type === "function_call" ||
        value.type === "tool_result" ||
        value.type === "function_call_output"
      ) {
        parts.push({
          role: who,
          text:
            value.type === "tool_result" ||
            value.type === "function_call_output"
              ? "Результат выполнения инструмента"
              : "Вызов инструмента",
          tool:
            typeof value.name === "string"
              ? value.name
              : typeof value.tool_use_id === "string"
                ? value.tool_use_id
                : "Инструмент",
        });
        return;
      }
      if (typeof value.text === "string") {
        collect(value.text, who, depth + 1);
        return;
      }
      if (value.messages !== undefined) {
        if (value.system !== undefined)
          collect(value.system, "system", depth + 1);
        collect(value.messages, who, depth + 1);
        return;
      }
      if (value.content !== undefined) {
        collect(value.content, who, depth + 1);
        return;
      }
      if (value.input !== undefined) {
        if (value.instructions !== undefined)
          collect(value.instructions, "system", depth + 1);
        collect(value.input, who, depth + 1);
        return;
      }
      if (value.output !== undefined) {
        collect(value.output, "assistant", depth + 1);
        return;
      }
      if (value.message !== undefined) {
        collect(value.message, who, depth + 1);
        return;
      }
      if (value.choices !== undefined) {
        collect(value.choices, "assistant", depth + 1);
        return;
      }
      if (
        value.type === "image" ||
        value.type === "image_url" ||
        value.type === "input_image"
      ) {
        parts.push({ role: who, text: "Изображение", tool: "" });
      }
    }
    collect(value, "Сообщение");
    return parts;
  }
  const parts = $derived(parse(content));
  function roleName(role: string): string {
    return (
      (
        {
          user: "Вы",
          assistant: "Модель",
          system: "Системные инструкции",
          developer: "Инструкции",
          tool: "Инструмент",
        } as Record<string, string>
      )[role] || role
    );
  }
</script>

<div class="transcript">
  {#each parts as part}<article class="message">
      <h3>{roleName(part.role)}</h3>
      {#if part.tool}<div class="tool-message">
          <strong>{part.tool}</strong>
          <p>{part.text}. Подробности — в разделе «Расширенное».</p>
        </div>{:else}<p class="message-text">{part.text}</p>{/if}
    </article>{/each}{#if !parts.length}<p class="empty">
      {content
        ? "Структурированные данные доступны в разделе «Расширенное»."
        : empty}
    </p>{/if}
</div>
