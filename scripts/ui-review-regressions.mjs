// Uses the real bundled UI with intercepted API data; no settings are written.
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
const require = createRequire(new URL('../internal/ui/web/package.json', import.meta.url));
const { chromium, webkit, expect } = require('@playwright/test');

export async function runReviewRegressions(browser, base) {
  const initial = await (await fetch(base + '/api/ui/state')).json();
  const failures = [];
  const name = 'claude-sonnet-review-z';
  const row = (model, destination, configured = true) => ({ model, label: model,
    choices: initial.efforts.map(effort => ({ effort, destination, inherited: 'anthropic', configured })) });
  async function check(title, test) {
    const state = structuredClone(initial);
    state.families = [];
    state.routes = [row(name, 'anthropic')];
    state.models = [{ key: 'p/model:tag', provider: 'p', model: 'model:tag', efforts: ['low','medium','high','xhigh','max'] }];
    state.pools = [{ name: 'review-pool', type: 'failover', members: [], failover: true }];
    const page = await browser.newPage();
    page.setDefaultTimeout(5000);
    const actions = [];
    let onAction = () => {};
    await page.route('**/api/ui/state', route => route.fulfill({ json: state }));
    await page.route('**/api/ui/events', route => route.abort());
    await page.route('**/api/ui/actions', async route => {
      const action = route.request().postDataJSON();
      actions.push(action);
      onAction(action);
      await route.fulfill({ json: { message: 'Тестовое сохранение' } });
    });
    const editor = model => page.locator('.route-editor').filter({ has: page.getByRole('heading', { name: model, exact: true }) });
    const choose = async (button, value) => {
      await button.click();
      const picker = page.getByRole('dialog', { name: 'Выбор цели маршрута', exact: true });
      await picker.getByRole('combobox').selectOption(value);
      await picker.getByRole('button', { name: 'Выбрать цель', exact: true }).click();
    };
    const open = async () => {
      await page.goto(base + '/#/routes');
      await page.getByText('Переопределения отдельных версий · 1', { exact: true }).click();
    };
    try {
      await test({ state, page, actions, editor, choose, open, onAction: fn => { onAction = fn; } });
      console.log('PASS review: ' + title);
    } catch (error) {
      failures.push(title + ': ' + error.message);
    } finally { await page.close(); }
  }
  await check('draft remains attached to model after insertion', async ({ state, page, actions, editor, choose, open, onAction }) => {
    await open();
    await choose(editor(name).getByRole('button', { name: 'Назначение ' + name, exact: true }), 'pool:review-pool');
    const inserted = 'claude-sonnet-review-a';
    onAction(action => { if (action.fields.model === inserted) state.routes.unshift(row(inserted, 'inherit', false)); });
    await page.getByLabel('ID модели Anthropic', { exact: true }).fill(inserted);
    await page.getByRole('button', { name: 'Добавить версию', exact: true }).click();
    await expect(page.locator('.route-editor')).toHaveCount(2);
    await expect(editor(name).getByText('Есть несохранённые изменения')).toBeVisible({ timeout: 1500 });
    await expect(editor(inserted).getByText('Есть несохранённые изменения')).toHaveCount(0);
    await editor(name).getByRole('button', { name: 'Сохранить', exact: true }).click();
    await expect.poll(() => actions.length).toBe(2);
    assert.equal(actions[1].fields.model, name);
    assert.equal(actions[1].fields.high, 'pool:review-pool');
  });
  await check('autofill preserves explicit disabled and fills absent levels', async ({ state, actions, editor, choose, open }) => {
    state.routes[0].choices.find(c => c.effort === 'high').destination = 'disabled';
    Object.assign(state.routes[0].choices.find(c => c.effort === 'low'), { destination: 'inherit', configured: false });
    await open();
    await choose(editor(name).getByRole('button', { name: name + ' / default', exact: true }), 'pool:review-pool');
    await expect(editor(name).getByRole('button', { name: name + ' / high', exact: true })).toContainText('Не настроено', { timeout: 1500 });
    await editor(name).getByRole('button', { name: 'Сохранить', exact: true }).click();
    await expect.poll(() => actions.length).toBe(1);
    assert.equal(actions[0].fields.high, 'disabled');
    assert.equal(actions[0].fields.low, 'pool:review-pool');
  });
  await check('inherited destination exceptions remain expanded', async ({ state, editor, open }) => {
    state.models[0].efforts = ['low','medium','high'];
    for (const choice of state.routes[0].choices) {
      choice.destination = ['xhigh','max'].includes(choice.effort) ? 'inherit' : 'model:p/model:tag:' + (choice.effort === 'default' ? '' : choice.effort);
      choice.inherited = 'pool:review-pool';
      choice.configured = choice.destination !== 'inherit';
    }
    await open();
    await expect(editor(name).getByRole('button', { name: name + ' / xhigh', exact: true })).toBeVisible({ timeout: 1500 });
    await expect(editor(name).getByText('Совпадающие effort', { exact: true })).toHaveCount(0);
  });
  await check('model identifiers containing a colon map all efforts', async ({ actions, editor, choose, open }) => {
    await open();
    await choose(editor(name).getByRole('button', { name: 'Назначение ' + name, exact: true }), 'model:p/model:tag:');
    await editor(name).getByRole('button', { name: 'Сохранить', exact: true }).click();
    await expect.poll(() => actions.length).toBe(1);
    for (const effort of initial.efforts)
      assert.equal(actions[0].fields[effort], 'model:p/model:tag:' + (effort === 'default' ? '' : effort));
  });
  assert.deepEqual(failures, [], 'UI review regressions');
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const browser = await (process.argv.includes('--webkit') ? webkit : chromium).launch({ headless: true });
  try { await runReviewRegressions(browser, process.argv[2]); }
  finally { await browser.close(); }
}
