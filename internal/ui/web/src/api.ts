import { writable } from "svelte/store";
export const notice = writable<{ message: string; error: boolean } | null>(
  null,
);
export const revision = writable(0);
export async function get<T>(url: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(url, {
    signal,
    headers: { Accept: "application/json" },
  });
  if (!response.ok)
    throw new Error(
      response.status === 404
        ? "Данные больше не доступны. Обновите страницу."
        : "Не удалось получить данные роутера. Попробуйте ещё раз.",
    );
  return response.json() as Promise<T>;
}
export async function act(
  action: string,
  fields: Record<string, string>,
): Promise<{ message?: string; url?: string }> {
  const response = await fetch("/api/ui/actions", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ action, fields }),
  });
  const data: { message?: string; error?: string; url?: string } =
    await response.json();
  if (!response.ok)
    throw new Error(
      data.error ||
        "Изменения не применены. Проверьте настройки и попробуйте ещё раз.",
    );
  notice.set({ message: data.message || "Изменения сохранены", error: false });
  revision.update((v) => v + 1);
  return data;
}
export function fail(error: unknown): void {
  notice.set({
    message:
      error instanceof Error
        ? error.message
        : "Нет связи с роутером. Проверьте, что он запущен.",
    error: true,
  });
}
export function time(value: string): string {
  const date = new Date(value);
  return Number.isFinite(date.getTime()) && date.getFullYear() > 2000
    ? date.toLocaleTimeString("ru-RU", {
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
      })
    : "—";
}
export function resetIn(value: string, now: string): string {
  const minutes = Math.ceil(
    (new Date(value).getTime() - new Date(now).getTime()) / 60000,
  );
  if (!Number.isFinite(minutes) || new Date(value).getFullYear() < 2000)
    return "Время сброса неизвестно";
  if (minutes <= 0) return "Ожидается обновление лимита";
  const days = Math.floor(minutes / 1440);
  const hours = Math.floor(minutes / 60) % 24;
  const rest = minutes % 60;
  const duration = days
    ? `${days} дн.${hours ? ` ${hours} ч.` : ""}`
    : hours
      ? `${hours} ч.${rest ? ` ${rest} мин` : ""}`
      : `${minutes} мин`;
  return "Сброс через " + duration;
}
export function resetAt(value: string): string {
  const date = new Date(value);
  return Number.isFinite(date.getTime()) && date.getFullYear() >= 2000
    ? date.toLocaleString("ru-RU", {
        day: "numeric",
        month: "long",
        year: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      })
    : "";
}
export function targetLabel(value: string): string {
  if (value === "anthropic") return "Anthropic";
  if (value === "disabled") return "Не настроено";
  if (value === "inherit") return "По семейству";
  return value
    .replace(/^pool:/, "Пул · ")
    .replace(/^model:/, "")
    .replace(/:$/, "")
    .replace(/:/g, " · ");
}
