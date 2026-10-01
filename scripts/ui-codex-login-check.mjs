// OAuth navigation is intercepted: these tests never log into a real account.
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
const require = createRequire(new URL('../internal/ui/web/package.json', import.meta.url));
const { expect } = require('@playwright/test');

export async function runCodexLoginChecks(browser, base) {
  const initial = await (await fetch(base + '/api/ui/state')).json();
  for (const mode of ['popup', 'blocked', 'error', 'unsafe-url']) {
    const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
    try {
      const state = structuredClone(initial);
      state.connections = Array.from({ length: 7 }, (_, i) => ({ name: 'login-test-' + i,
        displayName: 'Тест входа ' + i, type: 'codex', connected: i !== 6, pending: false,
        refreshing: false, error: '', baseURL: '', keySet: false, models: [], limits: [], updated: '' }));
      await context.route('https://auth.example.invalid/**', route => route.fulfill({
        contentType: 'text/html', body: '<h1>Test authorization</h1>' }));
      const page = await context.newPage();
      if (mode === 'blocked') await page.addInitScript(() => { window.open = () => null; });
      await page.route('**/api/ui/state', route => route.fulfill({ json: state }));
      await page.route('**/api/ui/events', route => route.abort());
      const actions = [];
      await page.route('**/api/ui/actions', route => {
        actions.push(route.request().postDataJSON());
        return route.fulfill(mode === 'error'
          ? { status: 409, json: { error: 'Уже идёт вход в другое подключение' } }
          : { json: { message: 'Откройте страницу входа Codex', url: mode === 'unsafe-url'
            ? 'javascript:alert(1)' : 'https://auth.example.invalid/oauth/authorize' } });
      });
      await page.goto(base + '/#/connections');
      const card = page.locator('.connection-card').filter({ has: page.getByRole('heading', { name: 'Тест входа 6', exact: true }) });
      await card.getByRole('button', { name: 'Войти в Codex', exact: true }).click();
      await expect.poll(() => actions.length).toBe(1);
      assert.deepEqual(actions[0], { action: 'codex.login', fields: { provider: 'login-test-6' } });
      if (mode === 'popup') {
        await expect.poll(() => context.pages().some(p => p.url().startsWith('https://auth.example.invalid/'))).toBe(true);
        const popup = context.pages().find(p => p !== page);
        assert(await popup.evaluate(() => window.opener === null), 'authorization page retained access to router');
      } else {
        const dialog = page.getByRole('dialog', { name: 'Вход в Codex · Тест входа 6', exact: true });
        await expect(dialog).toBeVisible();
        if (mode === 'blocked') {
          const link = dialog.getByRole('link', { name: 'Открыть вход в Codex ↗', exact: true });
          await expect(link).toBeInViewport();
          await link.click();
          await expect.poll(() => context.pages().some(p => p.url().startsWith('https://auth.example.invalid/'))).toBe(true);
        } else {
          await expect(dialog.getByRole('alert')).toContainText(mode === 'error' ? 'Уже идёт вход' : 'недопустимый адрес');
          await expect.poll(() => context.pages().length).toBe(1);
          await expect(dialog.getByRole('link')).toHaveCount(0);
        }
      }
      console.log('PASS Codex login: ' + mode);
    } finally { await context.close(); }
  }
}
