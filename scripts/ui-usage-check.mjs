// UI-only fixtures: no router home, credentials, network generation or quota.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile, readdir, mkdtemp } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";
import { once } from "node:events";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const require = createRequire(join(root, "internal/ui/web/package.json"));
const { chromium, expect } = require("@playwright/test");
const dist = join(root, "internal/ui/dist");
const files = new Map([["/index.html", await readFile(join(dist, "index.html"))]]);
for (const name of await readdir(join(dist, "assets"))) files.set("/assets/" + name, await readFile(join(dist, "assets", name)));
const server = createServer((req, res) => {
  const path = new URL(req.url, "http://localhost").pathname;
  if (path === "/api/ui/events") {
    res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
    res.write(": fixture connection stays open\n\n");
    req.on("close", () => res.end());
    return;
  }
  const key = files.has(path) ? path : "/index.html";
  res.setHeader("Content-Type", key.endsWith(".js") ? "text/javascript" : key.endsWith(".css") ? "text/css" : "text/html");
  res.setHeader("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'");
  res.end(files.get(key));
});
server.listen(0, "127.0.0.1");
await once(server, "listening");
const base = `http://127.0.0.1:${server.address().port}`;
const screenshots = await mkdtemp(join(tmpdir(), "router-usage-ui-"));
console.log(JSON.stringify({ screenshots }));
const now = new Date().toISOString();
const usage = {
  since: new Date(Date.now() - 86400000).toISOString(), requests: 12, measuredRequests: 12, cacheMeasuredRequests: 12,
  inputTokens: 1200000, cacheInputTokens: 1200000, cachedInputTokens: 1020000, uncachedInputTokens: 180000,
  outputTokens: 45000, cacheWriteTokens: 0, reasoningTokens: 36000, reasoningMeasuredRequests: 12,
  upstreamCalls: 14, continuationRequests: 2, invalidRequests: 0, lowCache: false,
};
const unknown = { ...usage, cacheMeasuredRequests: 0, cacheInputTokens: 0, cachedInputTokens: 0, uncachedInputTokens: 0 };
const empty = Object.fromEntries(Object.entries(usage).map(([key, value]) => [key, typeof value === "number" ? 0 : value]));
const low = { ...usage, cachedInputTokens: 80000, uncachedInputTokens: 1120000, lowCache: true };
const connection = (name, displayName, data, type = "codex") => ({ name, displayName, type, connected: true, pending: false, refreshing: false, error: "", baseURL: "", keySet: false, models: [], limits: [], updated: now, usage: data });
const session = (id, data) => ({ id, model: "codex/gpt-6-sol", requestedModel: "claude-sonnet", preview: "Локальная проверка", connection: "codex", effort: "high", route: "local", pending: 0, lastAt: now, error: "", requests: 12, usage: data });
const state = {
  now, started: now, lifecycle: "active", activeProfile: "default", defaultPool: "", profiles: [],
  connections: [connection("codex", "Рабочий Codex", usage), connection("second", "Второй Codex", low), connection("unknown", "Старые измерения", unknown, "openai"), connection("anthropic", "Anthropic", empty, "anthropic")],
  models: [], families: [], routes: [], pools: [], efforts: ["default", "high"],
  summary: { total: 24, pending: 0, errors5m: 0 }, sessions: [session("good-session-1", usage), session("low-session-2", low)],
  interception: { enabled: true, canRestore: true, error: "" }, reloadErrors: [],
};
const browser = await chromium.launch({ headless: true });
const page = await browser.newPage({ viewport: { width: 1440, height: 1080 }, locale: "ru-RU", colorScheme: "light" });
async function capture(name) {
  await page.evaluate(async () => {
    await new Promise(resolve => requestAnimationFrame(resolve));
    await Promise.all(document.getAnimations()
      .filter(animation => animation.effect?.getComputedTiming().iterations !== Infinity)
      .map(animation => animation.finished.catch(() => {})));
  });
  await page.screenshot({ path: join(screenshots, name), fullPage: true });
}
const errors = [];
page.on("pageerror", error => errors.push(error.message));
await page.addInitScript(() => { window.__usageCSP = []; document.addEventListener("securitypolicyviolation", event => window.__usageCSP.push(event.violatedDirective)); });
await page.route("**/api/ui/state", route => route.fulfill({ json: state }));
await page.route("**/api/ui/requests?*", route => {
  const selected = new URL(route.request().url()).searchParams.get("session");
  return route.fulfill({ json: { items: [], total: 0, offset: 0, limit: 50, ...(selected ? { sessionUsage: selected === "low-session-2" ? low : usage } : {}) } });
});
try {
  await page.goto(base + "/connections");
  await expect(page.locator(".connection-card .token-usage")).toHaveCount(4);
  await expect(page.getByText("Не удаётся обновить данные.", { exact: false })).toHaveCount(0);
  const good = page.locator(".connection-card").filter({ has: page.getByRole("heading", { name: "Рабочий Codex", exact: true }) });
  const bad = page.locator(".connection-card").filter({ has: page.getByRole("heading", { name: "Второй Codex", exact: true }) });
  const missing = page.locator(".connection-card").filter({ has: page.getByRole("heading", { name: "Старые измерения", exact: true }) });
  await expect(good.locator(".cache-rate")).toContainText("85%");
  await expect(good.locator(".usage-numbers")).toContainText("180");
  await expect(good.locator(".usage-notice")).toHaveCount(0);
  await expect(bad.locator(".usage-notice")).toHaveText("● Мало кеша");
  await expect(missing.locator(".cache-rate")).toHaveText("Кеш —");
  await expect(missing.locator(".usage-numbers dd").nth(0)).toHaveText("—");
  await expect(missing.locator(".usage-coverage")).toContainText("суммы неполные");
  await expect(missing.locator(".usage-notice")).toHaveCount(0);
  await good.getByText("Как читать расход", { exact: true }).click();
  await expect(good.locator(".usage-details")).toContainText("а не сумма списания по подписке");
  await expect(good.locator(".usage-details")).toContainText("Внутренние продолжения: 2");
  await good.getByText("Как читать расход", { exact: true }).click();
  await capture("connections-light.png");
  await page.emulateMedia({ colorScheme: "dark" });
  await capture("connections-dark.png");
  await page.setViewportSize({ width: 390, height: 844 });
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "connection cards overflow on mobile");
  await capture("connections-mobile.png");
  await page.setViewportSize({ width: 1440, height: 1080 });
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto(base + "/overview");
  await expect(page.locator(".session-token-summary")).toHaveCount(2);
  await expect(page.locator(".session-token-summary.usage-attention")).toContainText("Мало кеша");
  await page.locator('.session-row[href*="low-session-2"]').click();
  await expect(page.getByRole("region", { name: "Токены сессии за 24 часа" })).toBeVisible();
  await expect(page.locator(".session-usage-panel .usage-notice")).toContainText("Мало кеша");
  await page.getByRole("combobox", { name: /^Сессия/ }).selectOption("good-session-1");
  await expect(page.locator(".session-usage-panel .cache-rate")).toContainText("85%");
  await expect(page.locator(".session-usage-panel .usage-notice")).toHaveCount(0);
  await capture("session.png");
  await page.getByRole("button", { name: "Все сессии", exact: true }).click();
  await expect(page.locator(".session-usage-panel")).toHaveCount(0);
  state.connections[1].usage = usage;
  await page.goto(base + "/connections");
  await expect(page.locator(".usage-notice")).toHaveCount(0);
  assert.deepEqual(errors, [], "browser errors");
  assert.deepEqual(await page.evaluate(() => window.__usageCSP), [], "CSP violations");
  console.log(JSON.stringify({ ok: true, screenshots }));
} finally {
  await browser.close();
  await new Promise(resolve => server.close(resolve));
}
