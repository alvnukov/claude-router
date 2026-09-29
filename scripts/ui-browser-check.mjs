// An isolated router + local upstream; never uses the installed router home.
// npm run test:browser -- [--preview [--resume=<temporary-preview-home>]] [--webkit]
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, mkdir, writeFile, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import { spawn, execFileSync } from 'node:child_process';
import { once } from 'node:events';
import { runSessionChecks } from './ui-session-check.mjs';
import { runReviewRegressions } from './ui-review-regressions.mjs';
import { runCodexLoginChecks } from './ui-codex-login-check.mjs';
import { runPrivacyChecks } from './ui-privacy-check.mjs';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const require = createRequire(join(root, 'internal/ui/web/package.json'));
const { chromium, webkit, expect } = require('@playwright/test');
const preview = process.argv.includes('--preview');
const isWebkit = process.argv.includes('--webkit');
const resumeHome=process.argv.find(arg=>arg.startsWith('--resume='))?.slice(9);
if(resumeHome) assert(preview && dirname(resolve(resumeHome))===resolve(tmpdir()) && basename(resumeHome).startsWith('router-ui-check-'),'--resume requires a temporary router-ui-check preview home');
const home = resumeHome || await mkdtemp(join(tmpdir(), 'router-ui-check-'));
const shots = join(home, 'screenshots');
await mkdir(shots,{recursive:true});
await mkdir(join(home, 'claude'),{recursive:true});

const upstreamRequests = [];
const upstream = createServer(async (req, res) => {
  let body = '';
  for await (const chunk of req) body += chunk;
  res.setHeader('Content-Type', 'application/json');
  if (req.url.includes('models')) {
    res.end(JSON.stringify({data: [{id:'fast-model'}, {id:'careful-model'}]}));
  } else if (req.url.includes('chat/completions')) {
    upstreamRequests.push(JSON.parse(body));
    res.end(JSON.stringify({id:'fixture-response', object:'chat.completion',
      choices:[{index:0, message:{role:'assistant',content:'Локальный проверочный ответ'}, finish_reason:'stop'}],
      usage:{prompt_tokens:120, completion_tokens:30, total_tokens:150}}));
  } else {
    res.setHeader('anthropic-ratelimit-unified-5h-utilization', '0.42');
    res.setHeader('anthropic-ratelimit-unified-5h-reset', String(Math.floor(Date.now()/1000)+3600));
    res.end(JSON.stringify({id:'fixture-anthropic', type:'message', role:'assistant', model:'claude-sonnet-5',
      content:[{type:'text', text:'Локальный проверочный ответ'}], stop_reason:'end_turn', usage:{input_tokens:100,output_tokens:25}}));
  }
});
upstream.listen(0,'127.0.0.1');
await once(upstream,'listening');
const upstreamURL = `http://127.0.0.1:${upstream.address().port}`;
async function freePort() {
  const server = createServer(); server.listen(0, '127.0.0.1');
  await once(server, 'listening'); const port = server.address().port;
  await new Promise(resolve => server.close(resolve)); return port;
}
const requestedPort=process.argv.find(arg=>arg.startsWith('--port='))?.slice(7);
if(requestedPort) assert(preview && /^\d+$/.test(requestedPort) && Number(requestedPort)>1023 && Number(requestedPort)<65536,'--port requires --preview and a port between 1024 and 65535');
const uiPort = requestedPort ? Number(requestedPort) : await freePort(), apiPort = await freePort();
const base = `http://127.0.0.1:${uiPort}`;
const efforts = ['default','low','medium','high','xhigh','max'];
const cloud = Object.fromEntries(efforts.map(e=>[e,{mode:'anthropic'}]));
const profile = {
  family_routes:{opus:cloud, sonnet:{...cloud,high:{mode:'pool',pool:'Рабочий'}}, haiku:cloud}, routes:{},
  model_pools:{'Рабочий':[{model:'local-test/fast-model'},{model:'local-test/careful-model'}]},
  pool_settings:{'Рабочий':{type:'failover',failover:true,first_byte_seconds:10,probe_seconds:0,max_input_chars:0}},
};
if(!resumeHome) await writeFile(join(home,'providers.json'),JSON.stringify({
  providers:[{name:'local-test',display_name:'Локальный стенд',base_url:upstreamURL+'/v1',api_key:'fixture-secret-never-expose'}],
  models:[{provider:'local-test',model:'fast-model'},{provider:'local-test',model:'careful-model'}],
  active_profile:'default',profiles:{default:profile,economy:{...profile,family_routes:{opus:cloud,sonnet:cloud,haiku:cloud}}},
  ...profile,
}),{mode:0o600});
else {
  const saved=JSON.parse(await readFile(join(home,'providers.json'),'utf8'));
  const fixture=saved.providers.find(p=>p.name==='local-test');
  if(fixture) fixture.base_url=upstreamURL+'/v1';
  await writeFile(join(home,'providers.json'),JSON.stringify(saved),{mode:0o600});
}
const now = Date.now();
const prompts=['Проверь обработку ошибок','Добавь поиск по запросам','Обнови документацию','Найди причину таймаута'];
const records=Array.from({length:12},(_,i)=>{
  const session=`00000000-0000-4000-8000-${String(i%3+1).padStart(12,'0')}`;
  const prompt=prompts[i%prompts.length];
  const failed=i===4;
  const req={model:'claude-sonnet-5',max_tokens:100,metadata:{user_id:JSON.stringify({session_id:session})},messages:[{role:'user',content:prompt}]};
  const response={type:'message',content:[{type:'text',text:'Проверочный ответ локального стенда'}],stop_reason:'end_turn',usage:{input_tokens:100,output_tokens:25}};
  return {ID:`fixture-${i}`,Seq:i+1,Start:new Date(now-(12-i)*60000).toISOString(),End:new Date(now-(12-i)*60000+2300).toISOString(),
    Path:'/v1/messages',Model:'claude-sonnet-5',Route:i%3===0?'cloud':'local',Session:session,
    ReqBody:Buffer.from(JSON.stringify(req)).toString('base64'),
    OpenAIBody:Buffer.from(JSON.stringify({model:'fast-model',messages:req.messages})).toString('base64'),
    Served:i%3===0?'':'local-test/fast-model',Status:failed?503:200,RespCT:'application/json',
    RespBytes:Buffer.from(JSON.stringify(response)).toString('base64'),
    Resp:{Blocks:response.content.map(b=>({Type:b.type,Text:b.text})),Error:failed?'Проверочный таймаут подключения':''},
    Attempts:i%3===0?[]:[{Model:'local-test/fast-model',Err:failed?'Проверочный таймаут подключения':'',Dur:2300000000}]};
});
if(!resumeHome) await writeFile(join(home,'history.jsonl'),records.map(r=>JSON.stringify(r)).join('\n')+'\n',{mode:0o600});
if(!resumeHome) await writeFile(join(home,'limits.json'),JSON.stringify({with_headers:{at:new Date().toISOString(),headers:{
  'anthropic-ratelimit-unified-5h-utilization':'0.42',
  'anthropic-ratelimit-unified-5h-reset':String(Math.floor(Date.now()/1000)+3600),
}}}),{mode:0o600});
const binary=join(home,'localrouter');
execFileSync('go',['build','-o',binary,'.'],{cwd:root,stdio:'pipe'});
const env={...process.env};
for(const key of Object.keys(env)) if(key.startsWith('ROUTER_')) delete env[key];
Object.assign(env,{
  ROUTER_LISTEN:`127.0.0.1:${apiPort}`,ROUTER_UI_LISTEN:`127.0.0.1:${uiPort}`,ROUTER_PUBLIC_LISTEN:`127.0.0.1:${apiPort}`,
  ROUTER_UPSTREAM_URL:upstreamURL, ROUTER_PROVIDERS_FILE:join(home,'providers.json'),
  ROUTER_ENV_FILE:join(home,'env'),ROUTER_UI_HISTORY_FILE:join(home,'history.jsonl'),
  ROUTER_ANTHROPIC_LIMITS_FILE:join(home,'limits.json'),ROUTER_CODEX_AUTH_FILE:join(home,'codex-auth.json'),
  ROUTER_STATE_FILE:join(home,'state.json'),CLAUDE_CONFIG_DIR:join(home,'claude'),
  HTTPS_PROXY:'http://127.0.0.1:1',HTTP_PROXY:'http://127.0.0.1:1',NO_PROXY:'127.0.0.1,localhost',
});
const router=spawn(binary,['serve'],{cwd:home,env,stdio:['ignore','pipe','pipe']});
let log=''; router.stderr.on('data',chunk=>{log=(log+chunk).slice(-5000);});
let browser;
async function shutdown(){await browser?.close(); router.kill('SIGTERM'); upstream.close();}
process.once('SIGINT',()=>{void shutdown().then(()=>process.exit(0));});
process.once('SIGTERM',()=>{void shutdown().then(()=>process.exit(0));});
try {
  let ready=false;
  for(let i=0;i<100;i++){
    if(router.exitCode!==null) throw new Error(`Router exited: ${log}`);
    try {const response=await fetch(base+'/api/ui/state'); if(response.ok){ready=true;break;}}catch{}
    await new Promise(resolve=>setTimeout(resolve,100));
  }
  assert(ready,`Router did not become ready: ${log}`);
  // Exercise the real proxy and history capture before inspecting the UI.
  for(const effort of resumeHome ? [] : ['default','high']){
    const response=await fetch(`http://127.0.0.1:${apiPort}/v1/messages`,{
      method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({model:'claude-sonnet-5',max_tokens:100,
        ...(effort==='default'?{}:{output_config:{effort}}),
        metadata:{user_id:JSON.stringify({session_id:'browser-workflow'})},
        messages:[{role:'user',content:'Проверка нового интерфейса'}]}),
    });
    assert.equal(response.status,200,`fixture proxy ${effort}: ${await response.text()}`);
  }
  console.log(JSON.stringify({base,home,screenshots:shots,mode:preview?'preview':'check'}));
  if(preview){await once(router,'exit');process.exitCode=0;}
  else {
    browser=await (isWebkit?webkit:chromium).launch({headless:true});
    await runPrivacyChecks(browser,base,expect,shots,isWebkit,`http://127.0.0.1:${apiPort}`,upstreamRequests);
    await runReviewRegressions(browser, base);
    await runSessionChecks(browser, base);
    await runCodexLoginChecks(browser, base);
    const page=await browser.newPage({viewport:{width:1440,height:1000},locale:'ru-RU',colorScheme:'light'});
    async function capture(path){
      await page.evaluate(()=>{if(document.activeElement instanceof HTMLElement) document.activeElement.blur(); window.scrollTo(0,0);});
      await page.evaluate(async()=>Promise.all(document.getAnimations()
        .filter(animation=>animation.effect?.getComputedTiming().iterations!==Infinity)
        .map(animation=>animation.finished.catch(()=>{}))));
      if(!isWebkit){await page.screenshot({path,fullPage:true});return;}
      // Playwright itself inserts an inline stylesheet in WebKit screenshots.
      // Capture a separate page, keeping the functional/CSP probe untouched.
      const shot=await browser.newPage({viewport:page.viewportSize(),locale:'ru-RU',
        colorScheme:await page.evaluate(()=>matchMedia('(prefers-color-scheme: dark)').matches)?'dark':'light'});
      try{
        await shot.goto(page.url());
        await shot.locator('.workspace-strip').waitFor();
        await shot.screenshot({path,fullPage:true});
      } finally {await shot.close();}
    }
    const errors=[];
    page.on('pageerror',error=>errors.push(error.message));
    await page.addInitScript(()=>{window.__csp=[];document.addEventListener('securitypolicyviolation',e=>window.__csp.push(e.violatedDirective));});
    const stateResponse=await fetch(base+'/api/ui/state');
    assert(!JSON.stringify(await stateResponse.json()).includes('fixture-secret-never-expose'),'API exposed credential');
    await page.goto(base+'/connections');
    await page.getByRole('button',{name:'+ Добавить подключение',exact:true}).click();
    await page.getByLabel('Название для отображения',{exact:true}).fill('Проверочный Codex');
    await page.getByLabel(/^Короткий идентификатор/).fill('codex-ui-check');
    await page.getByRole('button',{name:'Добавить подключение',exact:true}).click();
    await expect(page.getByRole('heading',{name:'Проверочный Codex',exact:true})).toBeVisible();
    let saved=JSON.parse(await readFile(join(home,'providers.json'),'utf8'));
    const created=saved.providers.find(p=>p.name==='codex-ui-check');
    assert(created?.auth_id,'new Codex must get independent credential identity');
    const card=page.locator('article').filter({has:page.getByRole('heading',{name:'Проверочный Codex',exact:true})});
    await card.getByText('Настройки подключения',{exact:true}).click();
    await card.getByLabel('Название',{exact:true}).fill('Рабочий Codex');
    await card.getByRole('button',{name:'Сохранить',exact:true}).click();
    await expect(page.getByRole('heading',{name:'Рабочий Codex',exact:true})).toBeVisible();
    saved=JSON.parse(await readFile(join(home,'providers.json'),'utf8'));
    const renamed=saved.providers.find(p=>p.name==='codex-ui-check');
    assert.equal(renamed.auth_id,created.auth_id,'display rename changed credential identity');
    assert.equal(renamed.display_name,'Рабочий Codex');
    const localCard=page.locator('article').filter({has:page.getByRole('heading',{name:'Локальный стенд',exact:true})});
    await localCard.getByText('Настройки подключения',{exact:true}).click();
    await localCard.getByRole('button',{name:'Удалить подключение',exact:true}).click();
    await page.getByRole('dialog').getByRole('button',{name:'Подтвердить',exact:true}).click();
    await expect(page.getByText(/Сначала.*пул/).first()).toBeVisible();
    saved=JSON.parse(await readFile(join(home,'providers.json'),'utf8'));
    assert(saved.providers.some(p=>p.name==='local-test'),'referenced provider was removed');
    await page.goto(base+'/requests');
    await page.getByLabel('Поиск',{exact:true}).fill('Проверка нового интерфейса');
    await page.getByRole('button',{name:'Найти',exact:true}).click();
    await expect(page.locator('.request-row')).toHaveCount(2);
    await page.locator('.request-row').first().click();
    await expect(page.getByRole('dialog',{name:'Детали запроса'})).toBeVisible();
    await expect(page.getByRole('tabpanel')).toContainText('Локальный проверочный ответ');
    await page.keyboard.press('Escape');
    await page.goto(base+'/routes');
    const sonnet=page.locator('.route-editor').filter({has:page.getByRole('heading',{name:'Sonnet',exact:true})});
    await sonnet.getByRole('button',{name:'Sonnet / high',exact:true}).click();
    const picker=page.getByRole('dialog',{name:'Выбор цели маршрута',exact:true});
    // The wrapping label contains optgroup text; use the actual accessible name.
    await picker.getByRole('combobox',{name:'Цель маршрута',exact:true}).selectOption('anthropic');
    await picker.getByRole('button',{name:'Выбрать цель',exact:true}).click();
    await sonnet.getByRole('button',{name:'Сохранить',exact:true}).click();
    await expect.poll(async()=>{
      const state=await (await fetch(base+'/api/ui/state')).json();
      return state.families.find(row=>row.model==='sonnet').choices.find(c=>c.effort==='high').destination;
    }).toBe('anthropic');
    await page.getByRole('button',{name:'Опустить local-test/fast-model',exact:true}).click();
    await expect.poll(async()=>{
      const state=await (await fetch(base+'/api/ui/state')).json();
      return state.pools.find(pool=>pool.name==='Рабочий').members[0].model;
    }).toBe('local-test/careful-model');
    const poolCard=page.locator('article.card').filter({has:page.getByRole('heading',{name:'Рабочий',exact:true})});
    await expect(poolCard.locator('.pool-member').first()).toContainText('local-test/careful-model');
    const member=poolCard.locator('.pool-member').filter({has:page.locator('strong').filter({hasText:'local-test/careful-model'})});
    await member.getByRole('combobox',{name:'Усилие',exact:true}).selectOption('request');
    await page.waitForResponse(r=>r.url().endsWith('/api/ui/state') && r.status()===200);
    await expect(member.getByRole('combobox',{name:'Усилие',exact:true})).toHaveValue('request');
    await member.getByRole('button',{name:'Сохранить',exact:true}).click();
    const statePool=async(name='Рабочий')=>(await (await fetch(base+'/api/ui/state')).json()).pools.find(p=>p.name===name);
    await expect.poll(async()=>(await statePool()).members[0].effort).toBe('request');
    await member.locator('summary').click();
    await member.getByRole('combobox',{name:'Effort high для local-test/careful-model',exact:true}).selectOption('xhigh');
    await member.getByRole('combobox',{name:'Effort medium для local-test/careful-model',exact:true}).selectOption('low');
    await page.waitForResponse(r=>r.url().endsWith('/api/ui/state') && r.status()===200);
    await expect(member.getByRole('combobox',{name:'Effort high для local-test/careful-model',exact:true})).toHaveValue('xhigh');
    await member.getByRole('button',{name:'Сохранить сопоставление',exact:true}).click();
    await expect.poll(async()=>(await statePool()).members[0].effortMap).toEqual({high:'xhigh',medium:'low'});
    const originalPool=await statePool();
    await poolCard.getByRole('button',{name:'Клонировать пул Рабочий',exact:true}).click();
    const cloneDialog=page.getByRole('dialog',{name:'Клонирование пула',exact:true});
    await cloneDialog.getByLabel('Название копии').fill('Рабочий');
    await cloneDialog.getByRole('button',{name:'Создать копию',exact:true}).click();
    await expect(cloneDialog.getByRole('alert')).toContainText('уже существует');
    await cloneDialog.getByLabel('Название копии').fill('Рабочий — копия');
    await cloneDialog.getByRole('button',{name:'Создать копию',exact:true}).click();
    await expect(cloneDialog).not.toBeVisible();
    await expect.poll(async()=>await statePool('Рабочий — копия')).toEqual({...originalPool,name:'Рабочий — копия'});
    await sonnet.getByRole('button',{name:'Настроить по effort',exact:true}).click();
    await sonnet.getByRole('button',{name:'Sonnet / default',exact:true}).click();
    await picker.getByRole('combobox',{name:'Цель маршрута',exact:true}).selectOption('pool:Рабочий');
    await picker.getByRole('button',{name:'Выбрать цель',exact:true}).click();
    await sonnet.getByRole('button',{name:'Применить ко всем effort',exact:true}).click();
    await sonnet.getByRole('button',{name:'Свернуть сопоставление',exact:true}).click();
    await expect(sonnet.locator('.route-cells')).toHaveCount(0);
    await expect(sonnet.getByRole('button',{name:'Назначение Sonnet',exact:true})).toContainText('Пул · Рабочий');
    await sonnet.getByRole('button',{name:'Сохранить',exact:true}).click();
    await expect.poll(async()=>{
      const state=await (await fetch(base+'/api/ui/state')).json();
      return state.families.find(row=>row.model==='sonnet').choices.every(c=>c.destination==='pool:Рабочий');
    }).toBe(true);
    for(const [source,target] of [['medium','low'],['low','low'],['default',undefined],['high','xhigh']]){
      const response=await fetch(`http://127.0.0.1:${apiPort}/v1/messages`,{
        method:'POST',headers:{'Content-Type':'application/json'},
        body:JSON.stringify({model:'claude-sonnet-5',max_tokens:100,
          ...(source==='default'?{}:{output_config:{effort:source}}),
          messages:[{role:'user',content:'Проверка сопоставления effort'}]}),
      });
      assert.equal(response.status,200,await response.text());
      assert.equal(upstreamRequests.at(-1).model,'careful-model');
      assert.equal(upstreamRequests.at(-1).reasoning_effort,target);
    }
    await capture(join(shots,'routes-compact.png'));
    await page.setViewportSize({width:375,height:812});
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,'compact pool route/mobile overflow');
    await page.setViewportSize({width:1440,height:1000});
    await page.goto(base+'/requests');
    await page.getByLabel('Поиск',{exact:true}).fill('Проверка сопоставления effort');
    await page.getByRole('button',{name:'Найти',exact:true}).click();
    await expect(page.locator('.request-row')).toHaveCount(4);
    await page.locator('.request-row').first().click();
    await expect(page.locator('.request-routing')).toContainText('effort: high');
    await expect(page.locator('.request-routing')).toContainText('effort: xhigh');
    await page.keyboard.press('Escape');
    await page.goto(base+'/routes');
    await sonnet.getByRole('button',{name:'Настроить по effort',exact:true}).click();
    await sonnet.getByRole('button',{name:'Sonnet / high',exact:true}).click();
    await picker.getByRole('combobox',{name:'Цель маршрута',exact:true}).selectOption('anthropic');
    await picker.getByRole('button',{name:'Выбрать цель',exact:true}).click();
    await expect(sonnet.getByRole('button',{name:'Свернуть сопоставление',exact:true})).toHaveCount(0);
    await sonnet.getByRole('button',{name:'Сохранить',exact:true}).click();
    await expect.poll(async()=>{
      const state=await (await fetch(base+'/api/ui/state')).json();
      return state.families.find(row=>row.model==='sonnet').choices.find(c=>c.effort==='high').destination;
    }).toBe('anthropic');
    await page.reload();
    await expect(sonnet.locator('.route-cells')).toBeVisible();
    // Direct model mapping also collapses, and explicit exceptions stay visible.
    await sonnet.getByRole('button',{name:'Sonnet / default',exact:true}).click();
    await picker.getByRole('combobox',{name:'Цель маршрута',exact:true}).selectOption('model:local-test/careful-model:');
    await picker.getByRole('button',{name:'Выбрать цель',exact:true}).click();
    await sonnet.getByRole('button',{name:'Применить ко всем effort',exact:true}).click();
    await expect(sonnet.locator('.route-cells')).toHaveCount(0);
    await expect(sonnet.getByRole('button',{name:'Назначение Sonnet',exact:true})).toContainText('local-test/careful-model');
    await sonnet.getByRole('button',{name:'Сохранить',exact:true}).click();
    await expect.poll(async()=>{
      const state=await (await fetch(base+'/api/ui/state')).json();
      return state.families.find(row=>row.model==='sonnet').choices.every(c=>c.destination==='model:local-test/careful-model:'+(c.effort==='default'?'':c.effort));
    }).toBe(true);
    await page.getByRole('combobox',{name:'Пул для неизвестных моделей',exact:true}).selectOption('Рабочий');
    await page.getByRole('button',{name:'Сохранить пул по умолчанию',exact:true}).click();
    await expect.poll(async()=>(await (await fetch(base+'/api/ui/state')).json()).defaultPool).toBe('Рабочий');
    const unknownModel='diagnostic-browser-model';
    const fallback=await fetch(`http://127.0.0.1:${apiPort}/v1/messages`,{
      method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({model:unknownModel,max_tokens:100,output_config:{effort:'high'},messages:[{role:'user',content:'diagnostic-browser incoming'}]}),
    });
    assert.equal(fallback.status,200,await fallback.text());
    assert.equal(upstreamRequests.at(-1).model,'careful-model');
    assert.equal(upstreamRequests.at(-1).reasoning_effort,'xhigh');
    const unexpected=await fetch(`http://127.0.0.1:${apiPort}/v1/unsupported?token=diagnostic-secret-query`,{
      method:'POST',headers:{'Content-Type':'application/json','Authorization':'Bearer diagnostic-secret-header'},
      body:JSON.stringify({debug:'diagnostic-browser unknown payload'}),
    });
    assert.equal(unexpected.status,200,await unexpected.text());
    const malformed=await fetch(`http://127.0.0.1:${apiPort}/v1/messages`,{
      method:'POST',headers:{'Content-Type':'application/json'},body:'{"diagnostic-browser":',
    });
    assert.equal(malformed.status,400,await malformed.text());
    await page.goto(base+'/requests');
    await page.getByLabel('Не распознано',{exact:true}).check();
    await page.getByLabel('Поиск',{exact:true}).fill('diagnostic-browser');
    await page.getByRole('button',{name:'Найти',exact:true}).click();
    await expect(page.locator('.request-row')).toHaveCount(3);
    await expect(page.locator('.request-row').filter({hasText:unknownModel})).toContainText('Пул по умолчанию · Рабочий');
    await page.locator('.request-row').filter({hasText:'/v1/unsupported'}).click();
    const unknownDetail=page.getByRole('dialog',{name:'Детали запроса',exact:true});
    await expect(unknownDetail.getByRole('tabpanel')).toContainText('diagnostic-browser unknown payload');
    await expect(unknownDetail).toContainText('POST /v1/unsupported');
    await expect(unknownDetail).not.toContainText('diagnostic-secret');
    await unknownDetail.getByRole('tab',{name:'Ответ',exact:true}).click();
    await expect(unknownDetail.getByRole('tabpanel')).toContainText('fixture-anthropic');
    await page.keyboard.press('Escape');
    await page.getByLabel('Только ошибки',{exact:true}).check();
    await expect(page.locator('.request-row')).toHaveCount(1);
    await page.getByLabel('Только ошибки',{exact:true}).uncheck();
    await expect(page.locator('.request-row')).toHaveCount(3);
    await capture(join(shots,'requests-unrecognized.png'));
    await page.goto(base+'/routes');
    await page.getByRole('button',{name:'economy',exact:true}).click();
    await page.getByRole('dialog',{name:'Переключение профиля',exact:true}).getByRole('button',{name:'Активировать economy',exact:true}).click();
    await expect.poll(async()=>(await (await fetch(base+'/api/ui/state')).json()).activeProfile).toBe('economy');
    assert.equal((await (await fetch(base+'/api/ui/state')).json()).defaultPool,'','fallback leaked into another profile');
    for(const [path,label] of [['/','Обзор'],['/requests','Запросы'],['/routes','Маршруты'],['/connections','Подключения']]){
      await page.goto(base+path);
      await page.getByRole('heading',{level:1}).waitFor();
      await capture(join(shots,label+'.png'));
      assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,`desktop overflow: ${path}`);
      assert.deepEqual(await page.evaluate(()=>window.__csp),[],`CSP violations: ${path}`);
    }
    await page.goto(base+'/');
    await page.getByRole('heading',{level:1}).waitFor();
    await page.emulateMedia({colorScheme:'dark'});
    await capture(join(shots,'overview-dark.png'));
    await page.setViewportSize({width:375,height:812});
    for(const path of ['/','/requests','/routes','/connections']){
      await page.goto(base+path);
      await page.getByRole('heading',{level:1}).waitFor();
      assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,`mobile overflow: ${path}`);
      assert.deepEqual(await page.evaluate(()=>window.__csp),[],`mobile CSP violations: ${path}`);
    }
    await capture(join(shots,'connections-mobile.png'));
    assert.deepEqual(errors,[],'JavaScript page errors');
    assert.deepEqual(await page.evaluate(()=>window.__csp),[],'CSP violations');
    const bad=await fetch(base+'/api/ui/actions',{method:'POST',headers:{'Content-Type':'application/json',Origin:'https://foreign.invalid'},body:JSON.stringify({action:'providers',fields:{op:'remove',name:'local-test'}})});
    assert.equal(bad.status,403,'cross-origin mutation must fail');
    console.log('PASS: account identity, referenced deletion, request search/detail, route save, pool reorder/clone, effort mappings, compact routes/exceptions, default pool and profile isolation, unknown request capture/filter/raw debug, four screens, themes/mobile, no JS/CSP errors or API secrets, foreign-origin rejection');
  }
} finally { await shutdown(); }
