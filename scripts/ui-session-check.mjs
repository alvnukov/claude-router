// Real bundled UI, isolated fixture API: no installed router or credentials.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
const require = createRequire(new URL('../internal/ui/web/package.json', import.meta.url));
const { chromium, webkit, expect } = require('@playwright/test');
const sessionRoute = (requestedModel, model, connection, effort = '') => ({requestedModel, model, connection, effort, route: connection === 'anthropic' ? 'cloud' : 'local', requests: 1, pending: 0});
const routes = [sessionRoute('claude-opus-5', 'p/main', 'p', 'high'), sessionRoute('claude-sonnet-5', 'claude-sonnet-5', 'anthropic'), sessionRoute('claude-haiku-5', 'retired/small', 'retired')];
const session = id => ({id, ...routes[0], preview: 'Session ' + id, requests: 3, lastAt: new Date().toISOString(), error: '', routes: routes.map(route => ({...route}))});
const fixture = () => ({now: new Date().toISOString(), started: new Date().toISOString(), lifecycle: 'active', activeProfile: 'default', defaultPool: '',
  profiles: [], models: [], families: [], routes: [], pools: [], efforts: [], reloadErrors: [],
  connections: ['p', 'anthropic'].map(name => ({name, displayName: name, limits: [], models: []})),
  summary: {total: 6, pending: 0, errors5m: 0}, sessions: [session('session-a'), session('session-b')], interception: {enabled: true}});
const requestList = id => ({items: [{id: 'request-' + id, session: id, model: 'claude-opus-5', served: 'p/main', start: new Date().toISOString(), durationMs: 10, status: 200, preview: 'Request ' + id}], total: 1, offset: 0, limit: 50});

export async function runSessionChecks(browser, base) {
  const failures = [];
  async function check(name, test) {
    const page = await browser.newPage({viewport: {width: 1440, height: 1000}});
    page.setDefaultTimeout(5000);
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const state = fixture();
    let snapshots = 0;
    await page.route('**/api/ui/state', route => {
      state.now = new Date(Date.now() + ++snapshots).toISOString();
      return route.fulfill({json: state});
    });
    await page.route('**/api/ui/events', route => route.abort());
    try {
      await test(page, state, () => snapshots);
      assert.deepEqual(errors, []);
      console.log('PASS sessions: ' + name);
    } catch (error) { failures.push(name + ': ' + error.message); }
    finally { await page.close(); }
  }
  await check('slow history survives frequent state refreshes', async (page, state, snapshots) => {
    await page.route('**/api/ui/events', route => route.fulfill({contentType: 'text/event-stream', body: 'retry: 50\nevent: refresh\ndata: {}\n\n'}));
    const pending = [];
    await page.route('**/api/ui/requests?*', route => { pending.push(route); });
    await page.goto(base);
    await page.locator('.session-node').first().click();
    await expect.poll(() => pending.length).toBeGreaterThan(0);
    const before = snapshots();
    await expect.poll(snapshots).toBeGreaterThan(before + 3);
    await pending[0].fulfill({json: requestList('session-a')});
    await expect(page.locator('.request-row')).toHaveCount(1);
    await expect(page.locator('.request-row')).toContainText('Request session-a');
  });
  await check('changed filters reject the previous response', async page => {
    const pending = new Map();
    await page.route('**/api/ui/requests?*', route => {
      const id = new URL(route.request().url()).searchParams.get('session');
      const entries = pending.get(id) || [];
      entries.push(route); pending.set(id, entries);
    });
    await page.goto(base + '/#/requests?session=session-a');
    await expect.poll(() => pending.has('session-a')).toBe(true);
    await page.locator('.filters select').nth(2).selectOption('session-b');
    await expect.poll(() => pending.has('session-b')).toBe(true);
    for (const route of pending.get('session-b')) await route.fulfill({json: requestList('session-b')});
    await expect(page.locator('.request-row')).toContainText('Request session-b');
    for (const route of pending.get('session-a')) await route.fulfill({json: requestList('session-a')});
    await expect(page.locator('.request-row')).toContainText('Request session-b');
  });
  await check('clearing history invalidates an in-flight snapshot', async page => {
    const pending = [];
    await page.route('**/api/ui/requests?*', route => { pending.push(route); });
    await page.route('**/api/ui/actions', route => {
      assert.equal(route.request().postDataJSON().action, 'requests.clear');
      return route.fulfill({json: {message: 'Очищено'}});
    });
    await page.goto(base + '/#/requests?session=session-a');
    await expect.poll(() => pending.length).toBe(1);
    await page.getByRole('button', {name: 'Очистить историю'}).click();
    await page.getByRole('dialog', {name: 'Подтверждение действия'}).getByRole('button', {name: 'Подтвердить', exact: true}).click();
    await expect.poll(() => pending.length).toBe(2);
    await pending[1].fulfill({json: {items: [], total: 0, offset: 0, limit: 50}});
    await pending[0].fulfill({json: requestList('session-a')});
    await expect(page.locator('.request-list')).toHaveAttribute('aria-busy', 'false');
    await expect(page.locator('.request-row')).toHaveCount(0);
  });
  await check('readable session names keep identity across navigation', async (page, state) => {
    Object.assign(state.sessions[0], {title: 'Разобрать кеш', project: 'coordinator', branch: 'main'});
    Object.assign(state.sessions[1], {title: 'Название второй сессии с очень длинным описанием задачи и проверкой переносов', project: 'another-project', branch: 'codex/session-names'});
    await page.route('**/api/ui/requests?*', route => route.fulfill({json: requestList('session-a')}));
    await page.goto(base);
    await expect(page.locator('.session-node').first()).toContainText('Разобрать кеш');
    await expect(page.locator('.session-node').first()).toContainText('coordinator · main');
    await expect(page.locator('.session-node').first()).toContainText('session-');
    await expect(page.locator('.session-row').first()).toContainText('Разобрать кеш');
    await page.setViewportSize({width: 390, height: 844});
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'readable names overflow on mobile');
    await page.screenshot({path: '/tmp/router-session-names-mobile.png', fullPage: true});
    await page.setViewportSize({width: 1440, height: 1000});
    await page.screenshot({path: '/tmp/router-session-names-desktop.png', fullPage: true});
    await page.locator('.session-node').first().click();
    await expect(page.locator('.request-session')).toContainText('Разобрать кеш');
    await expect(page.locator('.filters select').nth(2)).toHaveValue('session-a');
    assert.match(await page.locator('.filters select').nth(2).evaluate(el => el.selectedOptions[0].textContent), /Разобрать кеш/);
  });
  await check('destinations merge sessions and efforts without inventing a served model', async (page, state) => {
    state.connections.push({name: 'q', displayName: 'Second connection', models: [], limits: []});
    state.sessions[0].routes.push(
      {...sessionRoute('claude-sonnet-5', 'p/main', 'p', 'low'), requests: 4, pending: 1},
      sessionRoute('claude-sonnet-5', 'q/main', 'q', 'high'),
      sessionRoute('claude-fable-5-1', 'claude-fable-5-1', '', 'high'),
    );
    await page.goto(base);
    await expect(page.locator('.route-node')).toHaveCount(5);
    const main = page.locator('.route-node').filter({has: page.getByText('p', {exact: true})});
    await expect(main.locator('strong')).toHaveText('main');
    await expect(main).toContainText('6 запр.');
    await expect(main).toContainText('Сессий: 2');
    await expect(main).toContainText('В полёте: 1');
    await expect(main).toContainText('claude-opus-5');
    await expect(main).toContainText('claude-sonnet-5');
    await expect(page.locator('.route-node strong').filter({hasText: /^main$/})).toHaveCount(2);
    await expect(page.locator('.route-node strong').filter({hasText: 'Назначение не записано'})).toHaveCount(1);
    await expect(page.locator('.route-node strong').filter({hasText: 'claude-fable-5-1'})).toHaveCount(0);
    // Five edges from session A, three from B, three to known connections.
    await expect(page.locator('.patch-wires path')).toHaveCount(11);
    await page.screenshot({path: '/tmp/router-destinations-desktop.png', fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    await expect(page.locator('.route-node')).toHaveCount(5);
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'destinations overflow on mobile');
  });
  await check('distinct destinations retain their nodes and wires', async (page, state) => {
    state.sessions = [state.sessions[0]];
    await page.goto(base);
    await expect(page.locator('.route-node')).toHaveCount(3);
    await expect(page.locator('.route-node').nth(1)).toContainText('claude-sonnet-5');
    // Three session-to-route edges, two routes to still configured connections.
    await expect(page.locator('.patch-wires path')).toHaveCount(5);
    await page.screenshot({path: '/tmp/router-session-links-desktop.png', fullPage: true});
    await page.setViewportSize({width: 390, height: 844});
    await expect(page.locator('.route-node')).toHaveCount(3);
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'mobile overflow');
    await page.screenshot({path: '/tmp/router-session-links-mobile.png', fullPage: true});
  });
  assert.deepEqual(failures, [], 'session regressions');
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const dist = fileURLToPath(new URL('../internal/ui/dist/', import.meta.url));
  const server = createServer(async (req, res) => {
    const path = resolve(dist, '.' + (new URL(req.url, 'http://localhost').pathname === '/' ? '/index.html' : new URL(req.url, 'http://localhost').pathname));
    if (!path.startsWith(dist)) { res.writeHead(404).end(); return; }
    try {
      const body = await readFile(path);
      res.setHeader('Content-Type', {'.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml'}[extname(path)] || 'application/octet-stream');
      res.end(body);
    } catch { res.writeHead(404).end(); }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  let browser;
  try {
    browser = await (process.argv.includes('--webkit') ? webkit : chromium).launch({headless: true});
    await runSessionChecks(browser, `http://127.0.0.1:${server.address().port}`);
  } finally { await browser?.close(); await new Promise(resolve => server.close(resolve)); }
}
