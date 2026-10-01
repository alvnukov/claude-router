import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
const require = createRequire(fileURLToPath(new URL('../internal/ui/web/package.json', import.meta.url)));
const { expect } = require('@playwright/test');

export async function runChatGPTPlanChecks(browser, base) {
  const initial = await (await fetch(base + '/api/ui/state')).json();
  for (const enabled of [false, true]) {
    const context = await browser.newContext();
    try {
      const state = structuredClone(initial);
      state.version = 'fixture-build-revision';
      state.connections = [{ name:'plan', displayName:'ChatGPT plan fixture', type:'codex',
        connected:true, authMode:'chatgpt-plan', subscriptionEnabled:enabled, pending:false,
        refreshing:false, error:'', baseURL:'', keySet:false, models:[], limits:[], updated:'',
        catalogUpdated:'2026-10-01T00:00:00Z', usageURL:'https://chatgpt.com/#settings/Usage' }];
      const page = await context.newPage();
      await page.route('**/api/ui/state', route => route.fulfill({json:state}));
      await page.route('**/api/ui/events', route => route.abort());
      await page.goto(base + '/#/connections');
      const card = page.locator('.connection-card');
      await expect(card.getByRole('button',{name:'Continue with ChatGPT',exact:true})).toBeVisible();
      await expect(card.getByText(enabled ? 'Доступ по подписке ChatGPT разрешён' : 'Доступ по подписке отключён.',{exact:false})).toBeVisible();
      await expect(card.getByRole('link',{name:'Настройки ChatGPT → Usage ↗'})).toHaveAttribute('href','https://chatgpt.com/#settings/Usage');
      await expect(card.getByRole('button',{name:'Обновить лимиты',exact:true})).toHaveCount(0);
      await expect(card.getByText('Доступно сбросов лимита:',{exact:false})).toHaveCount(0);
      await expect(page.locator('#bar').getByText('fixture-build-revision',{exact:true})).toBeVisible();
    } finally { await context.close(); }
  }
}
