import type { ConnectionUsage } from "./types";

const compact = new Intl.NumberFormat("ru-RU", { notation: "compact", maximumFractionDigits: 1 });
const exact = new Intl.NumberFormat("ru-RU");
export const tokens = (value: number): string => compact.format(value);
export const exactTokens = (value: number): string => exact.format(value);
export function cachePercent(usage: ConnectionUsage | undefined): number | null {
  return usage && usage.cacheMeasuredRequests > 0 && usage.cacheInputTokens > 0
    ? Math.round(100 * usage.cachedInputTokens / usage.cacheInputTokens)
    : null;
}
