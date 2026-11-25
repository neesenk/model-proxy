// r6_loaderguard.test.mjs — executable wiring contract for the loaders'
// latest-wins guard and the create-key in-flight guard (R6 F13 / F14 / F15).
//
// The sibling suites pin app.js wiring by reading function bodies as text.
// This one goes one step further and RUNS the real functions: each loader's
// declaration is cut out of app.js verbatim (brace matching — the fnBody
// helper contract of autorefresh.test.mjs) and compiled into a closure
// together with the state it owns, also cut verbatim. Free variables become
// the closure's parameters, so the test supplies controlled stand-ins: an
// apiGet/apiPost whose responses the test lands in whatever order it chooses,
// and the smallest element stand-ins the loaders touch. The closure is
// strict — a loader reaching for state the harness did not extract throws
// ReferenceError — and Node's own fetch and timers are bound to stand-ins
// that throw, so a loader that bypasses the helpers fails loudly instead of
// really hitting the network or leaving a timer behind.
//
// What that proves, per finding:
//   F13 / F14 — a superseded response landing late writes nothing (data,
//               error banner, re-armed timer) and the latest one lands; the
//               background commit gate is consulted exactly as before.
//   F15       — a second New key click / Enter while the POST is in flight
//               mints nothing; the button recovers on success and failure.
//
// The code that already carried a hand-rolled sequence counter — five loaders
// (loadRequests, loadSecurity, loadSecurityRules, loadMCPAnalytics,
// loadTakeover) and two request guards that are not loaders (runYamlLint and
// the takeover run dialog's loadPreviewNow) — is characterized the same way
// further down, landing point by landing point, so its move onto the shared
// guard is a refactor under test. The last section states the wiring rule
// over the whole of app.js: a latest-wins token is taken through seqGuard and
// only through seqGuard — by shape, whatever a counter is called, and by
// execution, with every guard driven by a world in this file.
//
// Ordering comes from the promise chain — each assertion waits on the
// loader's own completion promise or on a macrotask barrier — never from a
// timer (the one loader that schedules a status timer gets stand-ins for it).
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import nodeTest from 'node:test';
import assert from 'node:assert/strict';
import {
  esc, keyUsageTableHTML, keyCreatedBannerHTML,
  accountProviderKey, quotaUsageFromSec, tokensRangeQuery,
  analyticsGranOptions, analyticsGranularity, analyticsMetricAllowed,
  requestTableHeadHTML, securityRangeFromSecs, takeoverVariantLabel,
  staleDataText, seqGuard,
} from '../assets/pure.js';

const here = dirname(fileURLToPath(import.meta.url));
const appJs = readFileSync(join(here, '..', 'assets', 'app.js'), 'utf8');

// fnSrc extracts a top-level `function NAME(` / `async function NAME(`
// declaration — header plus body — via brace matching (app.js templates keep
// braces balanced).
function fnSrc(name) {
  const m = new RegExp(`^(?:async )?function ${name}\\(`, 'm').exec(appJs);
  assert.ok(m, `app.js no longer declares function ${name}()`);
  let i = appJs.indexOf('{', m.index);
  let depth = 0;
  for (; i < appJs.length; i++) {
    if (appJs[i] === '{') depth++;
    else if (appJs[i] === '}') {
      depth--;
      if (depth === 0) break;
    }
  }
  assert.ok(i < appJs.length, `function ${name}() body is unbalanced`);
  return appJs.slice(m.index, i + 1);
}

// stateSrc extracts the top-level `let|const NAME = …;` statement of a
// loader's module-level state, or null when app.js declares none.
function stateSrc(name) {
  const m = new RegExp(`^(?:let|const) ${name} = [^\\n]*?;`, 'm').exec(appJs);
  return m ? m[0] : null;
}

// closureDecl extracts a function-LOCAL (indented) `let|const NAME = …;`
// declaration — the state a dialog opener keeps in its closure — or null.
function closureDecl(name) {
  const m = new RegExp(`^[ \\t]+(?:let|const) ${name} = [^\\n]*?;`, 'm').exec(appJs);
  return m ? m[0].trim() : null;
}

// arrowSrc extracts a function-local `const NAME = (async) (…) => { … };` —
// the nested loader itself, brace matching like fnSrc.
function arrowSrc(name) {
  const m = new RegExp(`^[ \\t]+const ${name} = (?:async )?\\([^)]*\\) => \\{`, 'm').exec(appJs);
  assert.ok(m, `app.js no longer declares the arrow function ${name}`);
  let i = appJs.indexOf('{', m.index);
  let depth = 0;
  for (; i < appJs.length; i++) {
    if (appJs[i] === '{') depth++;
    else if (appJs[i] === '}') {
      depth--;
      if (depth === 0) break;
    }
  }
  assert.ok(i < appJs.length && appJs[i + 1] === ';', `arrow function ${name} is unbalanced`);
  return appJs.slice(m.index, i + 2);
}

// The compiled closure is strict, but strict mode does not stop a loader from
// reaching for Node's own fetch and timers: one that bypassed the recorded
// API helpers would really hit the network or leave a timer running, and the
// test would neither say so nor fail for it. These names are therefore bound
// to stand-ins that throw, unless the world supplies its own.
const NOT_IN_A_LOADER_WORLD = ['fetch', 'setTimeout', 'setInterval'];
const forbiddenCalls = []; // what the stand-ins below were asked for, so the first thing that trips over it can name the cause
function forbidden(name) {
  return () => {
    forbiddenCalls.push(name);
    throw new Error(`${name}() is not available in a loader world — a loader goes through the recorded apiGet/apiPost stand-ins, or through the timer stand-ins its world provides`);
  };
}

// test registers one case and fails it afterwards if the code under test
// reached for a forbidden name: the stand-ins above throw, but a swallowing
// try/catch in the loader would hide that throw, and the case would pass.
function test(name, fn) {
  return nodeTest(name, async (t) => {
    t.after(() => {
      const calls = [...new Set(forbiddenCalls.splice(0))];
      assert.deepEqual(calls, [],
        `the code under test called ${calls.join('() / ')}(), which a loader world forbids`);
    });
    await fn(t);
  });
}

// loudLog makes reading past the end of a world's request log explain itself.
// Code that never reached the API stand-ins (it called a forbidden fetch or
// timer instead) leaves the log short, and its own promise rejects with an
// error nobody may ever await — a bare "cannot read 'resolve'" would hide
// the cause.
function loudLog(log) {
  return new Proxy(log, {
    get(target, prop) {
      if (typeof prop === 'string' && /^(?:0|[1-9]\d*)$/.test(prop) && !(prop in target)) {
        const cause = forbiddenCalls.length
          ? `it called ${[...new Set(forbiddenCalls)].join('() / ')}(), which a loader world forbids`
          : 'it never reached the recorded apiGet/apiPost stand-ins';
        throw new Error(`the code under test issued only ${target.length} request(s), so there is no request #${prop} to land — ${cause}`);
      }
      return target[prop];
    },
  });
}

// fragment compiles real app.js pieces into one closure: the named state
// declarations first (real initial values), then any `raw` source cut from
// app.js by the other extractors, then the named functions, then accessors.
// deps are the closure's free variables (a world's own deps override the
// forbidden stand-ins above). State app.js does not declare is simply absent
// from the result: the behavior tests must also run against the UNFIXED
// loaders to show the bug, so the harness never invents stand-in state — the
// wiring tests demand each declaration explicitly.
function fragment({ state, fns, deps, exposes = '', raw = [] }) {
  const declared = state.map(stateSrc);
  // State is exposed through live getters: a loader reassigns its module
  // state while running, and the test must read what it holds NOW.
  const present = state.filter((_, i) => declared[i]).map((n) => `get ${n}() { return ${n}; }`);
  const bound = Object.assign(Object.fromEntries(NOT_IN_A_LOADER_WORLD.map((n) => [n, forbidden(n)])), deps);
  const names = Object.keys(bound);
  const sources = [...declared.filter(Boolean), ...raw.filter(Boolean), ...fns.map(fnSrc)];
  const exported = [...fns, ...present, ...(exposes ? [exposes] : [])];
  const body = [
    '"use strict";',
    ...sources,
    `return { ${exported.join(', ')} };`,
  ].join('\n');
  const app = new Function(...names, body)(...names.map((n) => bound[n]));
  // What this world really compiled. The wiring contract compares the guards
  // these sources reference against the guards the world claims.
  return Object.defineProperty(app, 'sources', { value: sources, enumerable: false });
}

// declaredGuards names the state entries app.js declares as `= seqGuard()`.
// A world lists its loader's guard under its old counter name and this says
// whether that declaration is the shared guard yet — so the same world runs
// against a hand-rolled counter and against seqGuard. declOf picks the
// extractor matching where the state lives (module level or a closure).
function declaredGuards(state, declOf = stateSrc) {
  return state.filter((name) => / = seqGuard\(\);\s*$/.test(declOf(name) || ''));
}

// spyBag builds no-op stand-ins that record every call's arguments.
function spyBag(names) {
  const calls = Object.fromEntries(names.map((n) => [n, []]));
  const fns = Object.fromEntries(names.map((n) => [n, (...args) => { calls[n].push(args); }]));
  return { calls, fns };
}

// pending is an API response the test has not landed yet.
function pending() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

// flush is a macrotask barrier: every promise continuation already scheduled
// (however many awaits deep) runs before it resolves.
const flush = () => new Promise((resolve) => setImmediate(resolve));

// apiRecorder returns an apiGet/apiPost pair that records each call in issue
// order and hands the test a response it lands when it chooses.
function apiRecorder(calls) {
  const api = (method) => (path, body) => {
    const d = pending();
    calls.push({ method, path, body, resolve: d.resolve, reject: d.reject });
    return d.promise;
  };
  return { apiGet: api('GET'), apiPost: api('POST') };
}

// interactionGate stands in for deferAutoRefresh: it answers from a plan the
// scenario writes up front (true = park) and records every parked callback.
// An ask the scenario did not plan is a harness error, not a silent pass.
function interactionGate(w) {
  return (panel, cb) => {
    w.gate.asks += 1;
    const blocked = w.gatePlan.shift();
    if (blocked === undefined) throw new Error(`deferAutoRefresh ask #${w.gate.asks} is unplanned`);
    if (blocked) w.parked.push(cb);
    return blocked;
  };
}

// guardSpawner wraps the real seqGuard so a world can show which guard
// instances its compiled app.js state built, and how they were driven: the
// wiring contract matches them by identity against the loaders' guard
// variables and requires each to have handed out tokens and settled both a
// latest and a superseded landing.
function guardSpawner() {
  const guards = [];
  const usage = new Map(); // instance -> { next, latest, stale }
  const spawn = () => {
    const real = seqGuard();
    const g = {
      next() { usage.get(g).next += 1; return real.next(); },
      isLatest(token) {
        const ok = real.isLatest(token);
        usage.get(g)[ok ? 'latest' : 'stale'] += 1;
        return ok;
      },
    };
    guards.push(g);
    usage.set(g, { next: 0, latest: 0, stale: 0 });
    return g;
  };
  return {
    guards,
    seqGuard: spawn,
    usageOf: (instance) => {
      const rec = usage.get(instance);
      assert.ok(rec, 'not a guard instance this world built');
      return rec;
    },
  };
}

// guardBooked finishes a world. Besides the spawner that built its guards it
// carries `starts` — each begins one guarded request of the code under test —
// because the wiring contract drives every world through them. `prepare`
// (optional) mounts what the starts need first.
function guardBooked(spawner, guardNames, starts, world, prepare) {
  const calls = Array.isArray(world.calls) ? loudLog(world.calls) : world.calls;
  return { ...world, calls, spawner, guards: spawner.guards, guardNames, starts, prepare };
}

// element is the smallest stand-in for a node. Assigning innerHTML replaces
// the subtree the way the DOM does: every `id="…"` the markup declares is
// served by document.getElementById until its owner's markup changes again,
// so a loader finds exactly the nodes its own template created. Attribute
// lookups (`[data-x]`) parse the owner's markup into button stand-ins whose
// listeners the test can fire.
function element(doc, id = '') {
  const owned = new Map(); // id -> descendant stand-in
  const attrButtons = new Map(); // selector -> [button]
  const listeners = {};
  const el = {
    id, doc, html: '', value: '', textContent: '', disabled: false,
    get innerHTML() { return el.html; },
    set innerHTML(markup) {
      for (const name of owned.keys()) doc.ids.delete(name);
      owned.clear();
      attrButtons.clear();
      el.html = String(markup);
      for (const m of el.html.matchAll(/ id="([^"]+)"/g)) {
        const child = element(doc, m[1]);
        owned.set(m[1], child);
        doc.ids.set(m[1], child);
      }
    },
    addEventListener(type, fn) { (listeners[type] ||= []).push(fn); },
    fire(type, ev = {}) {
      // Browser rule the in-flight guard leans on: a disabled button
      // receives no click.
      if (type === 'click' && el.disabled) return;
      for (const fn of listeners[type] || []) fn(ev);
    },
    focus() {},
    select() {},
    // Non-empty markup means a child element exists (the loaders' "only an
    // empty host shows the loading hint" checks read this).
    get firstElementChild() { return el.html ? {} : null; },
    querySelector() { return null; },
    querySelectorAll(selector) {
      if (!attrButtons.has(selector)) {
        const attr = /^\[data-([a-z-]+)\]$/.exec(selector);
        assert.ok(attr, `harness has no attribute parsing for selector ${selector}`);
        const prop = attr[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase());
        const re = new RegExp(`data-${attr[1]}="([^"]*)"`, 'g');
        attrButtons.set(selector, [...el.html.matchAll(re)].map((m) => {
          const button = element(doc);
          button.dataset = { [prop]: m[1] };
          return button;
        }));
      }
      return attrButtons.get(selector);
    },
  };
  return el;
}

function fakeDocument() {
  return { ids: new Map(), getElementById(id) { return this.ids.get(id) || null; } };
}

// ---------------------------------------------------------------------------
// Keys tab: usage card (F13) + create key (F15)
// ---------------------------------------------------------------------------

// Obviously not a credential — the banner renders whatever the API returns.
const TEST_TOKEN = 'TEST-ONLY-NOT-A-REAL-KEY';
const created = (label) => ({
  id: 'ab12cd34', label, prefix: 'mp-key-test…', created_at: '2026-11-25T00:00:00Z', token: TEST_TOKEN,
});
const rowsA = [{ model: 'm-a', provider: 'p-a', requests: 11, failures: 0, input: 100, output: 50 }];
const rowsB = [{ model: 'm-b', provider: 'p-b', requests: 22, failures: 1, input: 200, output: 60 }];
const posts = (w) => w.calls.filter((c) => c.method === 'POST');

const KEYS_STATE = ['keysUsageOpen', 'keysUsageDays', 'keysUsageReqSeq', 'keysCreating'];

function keysWorld() {
  const doc = fakeDocument();
  const spawner = guardSpawner();
  const calls = [];
  const refreshed = []; // refreshKeysList() calls
  const app = fragment({
    state: KEYS_STATE,
    fns: ['renderKeysTab', 'createAPIKey', 'toggleKeyUsage', 'renderKeyUsage'],
    deps: {
      document: doc,
      panels: { keys: element(doc) },
      ...apiRecorder(calls),
      esc, keyUsageTableHTML, keyCreatedBannerHTML,
      displayProvider: (key) => key,
      navigator: {},
      refreshKeysList: async () => { refreshed.push(true); },
      seqGuard: spawner.seqGuard,
    },
  });
  return guardBooked(spawner, declaredGuards(KEYS_STATE), [(w) => w.app.renderKeyUsage('key-a')],
    { app, doc, calls, refreshed }, async (w) => { await w.app.renderKeysTab(); });
}

test("F13: a late usage response for another key must not fill the newer key's card", async () => {
  const w = keysWorld();
  await w.app.renderKeysTab();
  const first = w.app.renderKeyUsage('key-a');
  const second = w.app.renderKeyUsage('key-b');
  assert.deepEqual(w.calls.map((c) => c.path), [
    '/api/keys/key-a/usage?days=7',
    '/api/keys/key-b/usage?days=7',
  ]);
  w.calls[1].resolve({ id: 'key-b', days: 7, models: rowsB });
  await second;
  assert.equal(w.doc.getElementById('keys-usage-body').innerHTML, keyUsageTableHTML(rowsB));
  w.calls[0].resolve({ id: 'key-a', days: 7, models: rowsA }); // lands last
  await first;
  assert.ok(w.doc.getElementById('keys-usage').innerHTML.includes('<h3>Usage — key-b</h3>'), 'the card still names key-b');
  assert.equal(w.doc.getElementById('keys-usage-body').innerHTML, keyUsageTableHTML(rowsB),
    "key-a's late response must not land under key-b's header");
});

test('F13: a late response for the previous window must not fill the card after a 1d/7d/30d switch', async () => {
  const w = keysWorld();
  await w.app.renderKeysTab();
  const week = w.app.renderKeyUsage('key-a');
  const card = w.doc.getElementById('keys-usage');
  assert.match(card.innerHTML, /class="btn small primary" data-key-days="7"/);
  card.querySelectorAll('[data-key-days]').find((b) => b.dataset.keyDays === '1').fire('click');
  assert.deepEqual(w.calls.map((c) => c.path), [
    '/api/keys/key-a/usage?days=7',
    '/api/keys/key-a/usage?days=1',
  ]);
  const dayRows = [{ model: 'm-day', provider: 'p', requests: 3, failures: 0, input: 4, output: 2 }];
  w.calls[1].resolve({ models: dayRows });
  await flush();
  assert.equal(w.doc.getElementById('keys-usage-body').innerHTML, keyUsageTableHTML(dayRows));
  w.calls[0].resolve({ models: rowsA }); // the 7d request lands last
  await week;
  assert.match(card.innerHTML, /class="btn small primary" data-key-days="1"/, 'the card still highlights 1d');
  assert.equal(w.doc.getElementById('keys-usage-body').innerHTML, keyUsageTableHTML(dayRows),
    'the 7d response must not land under the 1d header');
});

test('F13: a late failure of a superseded usage request must not raise an error over the current card', async () => {
  const w = keysWorld();
  await w.app.renderKeysTab();
  const first = w.app.renderKeyUsage('key-a');
  const second = w.app.renderKeyUsage('key-b');
  w.calls[1].resolve({ models: rowsB });
  await second;
  w.calls[0].reject(new Error('index unavailable'));
  await first;
  assert.equal(w.doc.getElementById('keys-usage-body').innerHTML, keyUsageTableHTML(rowsB),
    "the superseded failure must not replace key-b's rows with an error");
});

test('F13: sequential usage requests land in full — toggle wiring, request, header, body, collapse', async () => {
  const w = keysWorld();
  await w.app.renderKeysTab();
  await w.app.toggleKeyUsage('key-a');
  assert.equal(w.calls[0].path, '/api/keys/key-a/usage?days=7');
  w.calls[0].resolve({ models: rowsA });
  await flush();
  assert.equal(w.app.keysUsageOpen, 'key-a');
  const card = w.doc.getElementById('keys-usage');
  assert.ok(card.innerHTML.includes('<h3>Usage — key-a</h3>'));
  assert.equal(w.doc.getElementById('keys-usage-body').innerHTML, keyUsageTableHTML(rowsA));
  // key-b is requested only AFTER key-a landed: each request was the latest
  // when it landed, so both land.
  await w.app.toggleKeyUsage('key-b');
  w.calls[1].resolve({ models: rowsB });
  await flush();
  assert.ok(card.innerHTML.includes('<h3>Usage — key-b</h3>'));
  assert.equal(w.doc.getElementById('keys-usage-body').innerHTML, keyUsageTableHTML(rowsB));
  await w.app.toggleKeyUsage('key-b'); // collapse
  assert.equal(w.app.keysUsageOpen, null);
  assert.equal(card.innerHTML, '');
  assert.equal(w.doc.getElementById('keys-usage-body'), null);
});

test('F13: collapsing the card while its request is in flight leaves it closed when the response lands', async () => {
  const w = keysWorld();
  await w.app.renderKeysTab();
  await w.app.toggleKeyUsage('key-a');
  await w.app.toggleKeyUsage('key-a'); // collapse before the response
  w.calls[0].resolve({ models: rowsA });
  await flush();
  const card = w.doc.getElementById('keys-usage');
  assert.equal(card.innerHTML, '', 'a response for a collapsed card must not re-open it');
  assert.equal(w.app.keysUsageOpen, null);
});

test('F15: a double click on New key mints exactly one key; the button is disabled in flight and restored after', async () => {
  const w = keysWorld();
  await w.app.renderKeysTab(); // the real wiring: #keys-new click → createAPIKey
  w.refreshed.length = 0; // drop the tab's own initial list load from the log
  const btn = w.doc.getElementById('keys-new');
  const label = w.doc.getElementById('keys-new-label');
  label.value = 'phone';
  btn.fire('click');
  btn.fire('click');
  assert.deepEqual(posts(w).map((c) => [c.path, c.body]), [['/api/keys', { label: 'phone' }]],
    'the second click must not mint a second key');
  assert.equal(btn.disabled, true, 'New key must be disabled while the POST is in flight');
  const minted = created('phone');
  posts(w)[0].resolve(minted);
  await flush();
  assert.equal(btn.disabled, false, 'the button must be restored after success');
  assert.equal(w.doc.getElementById('keys-created').innerHTML, keyCreatedBannerHTML(minted));
  assert.equal(label.value, '', 'the label input is cleared after a successful mint');
  assert.deepEqual(w.refreshed, [true], 'the list is refreshed once');
});

test('F15: Enter in the label input (key auto-repeat included) is ignored while a mint is in flight', async () => {
  const w = keysWorld();
  await w.app.renderKeysTab();
  const label = w.doc.getElementById('keys-new-label');
  label.value = 'ci';
  // Enter goes straight to createAPIKey — the disabled button cannot stop it.
  label.fire('keydown', { key: 'Enter' });
  label.fire('keydown', { key: 'Enter' });
  label.fire('keydown', { key: 'Enter' });
  w.doc.getElementById('keys-new').fire('click');
  assert.equal(posts(w).length, 1, 're-entry during the in-flight POST must not mint');
  posts(w)[0].resolve(created('ci'));
  await flush();
  assert.equal(posts(w).length, 1);
  assert.equal(w.doc.getElementById('keys-created').innerHTML, keyCreatedBannerHTML(created('ci')));
});

test('F15: a failed mint restores the button and keeps the label for a retry', async () => {
  const w = keysWorld();
  await w.app.renderKeysTab();
  w.refreshed.length = 0; // drop the tab's own initial list load from the log
  const btn = w.doc.getElementById('keys-new');
  const label = w.doc.getElementById('keys-new-label');
  label.value = 'ci';
  btn.fire('click');
  assert.equal(btn.disabled, true, 'in flight');
  posts(w)[0].reject(new Error('api keys file not configured')); // the backend's message
  await flush();
  assert.equal(btn.disabled, false, 'a failed mint must re-enable the button');
  assert.equal(w.doc.getElementById('keys-created').innerHTML,
    `<p class="msg err">${esc('api keys file not configured')}</p>`, 'the backend message is shown, not swallowed');
  assert.equal(label.value, 'ci', 'the label stays for the retry');
  assert.deepEqual(w.refreshed, []);
  btn.fire('click');
  assert.deepEqual(posts(w).map((c) => c.body), [{ label: 'ci' }, { label: 'ci' }],
    'a retry after the failure must mint');
});

test('F15: re-rendering the tab mid-flight (leave and return) mounts the New key button disabled', async () => {
  const w = keysWorld();
  await w.app.renderKeysTab();
  w.doc.getElementById('keys-new-label').value = 'phone';
  w.doc.getElementById('keys-new').fire('click');
  await w.app.renderKeysTab(); // tab re-activated mid-flight
  const fresh = w.doc.getElementById('keys-new');
  // The in-flight flag alone would ignore the click — the Enter test proves
  // that — but a fresh ENABLED button offers a click that then does nothing.
  // The tab re-renders on every re-entry, so the mount itself must honor
  // keysCreating.
  assert.equal(fresh.disabled, true, 'the re-rendered button must be disabled while the mint is in flight');
  w.doc.getElementById('keys-new-label').value = 'phone'; // the label, retyped in the fresh tab
  fresh.fire('click');
  assert.equal(posts(w).length, 1, 'no second mint while the first POST is in flight');
  const minted = created('phone');
  posts(w)[0].resolve(minted);
  await flush();
  assert.equal(w.doc.getElementById('keys-created').innerHTML, keyCreatedBannerHTML(minted),
    'the one-time token still surfaces in the re-rendered tab');
  assert.equal(fresh.disabled, false, 'the mounted button is re-enabled once the mint settles');
  assert.equal(w.app.keysCreating, false);
  w.doc.getElementById('keys-new-label').value = 'next';
  fresh.fire('click');
  assert.equal(posts(w).length, 2, '…and works again');
});

test('F15: the empty-label prompt never arms the in-flight guard', async () => {
  const w = keysWorld();
  await w.app.renderKeysTab();
  const btn = w.doc.getElementById('keys-new');
  btn.fire('click'); // blank label: prompt, no POST
  assert.equal(posts(w).length, 0);
  assert.equal(w.doc.getElementById('keys-new-err').textContent, 'type a label first (e.g. phone, ci)');
  assert.equal(btn.disabled, false, 'a refused submit must not leave the button disabled');
  w.doc.getElementById('keys-new-label').value = 'phone';
  btn.fire('click');
  assert.equal(posts(w).length, 1, 'the guard must not be stuck after a refused submit');
});

// ---------------------------------------------------------------------------
// Analytics tab (F14)
// ---------------------------------------------------------------------------

const ANALYTICS_UNFILTERED = '/api/analytics?from=1000&to=2000&granularity=minute&by=model';

function analyticsWorld() {
  const calls = [];
  const armed = []; // analyticsMaybeAutoRefresh() calls
  const banners = []; // setRefreshError(panel, text)
  const layouts = []; // buildAnalyticsLayout(…, state, …) — one per rebuild
  const landed = []; // renderer calls in order
  const errEl = { hidden: true, textContent: '' };
  const spawner = guardSpawner();
  const w = { calls, armed, banners, layouts, landed, parked: [], gatePlan: [], gate: { asks: 0 }, errEl };
  const panel = {
    rendered: false, // #an-kpis exists once buildAnalyticsLayout ran
    querySelector(selector) {
      if (selector === '#an-kpis') return this.rendered ? {} : null;
      if (selector === '#an-error') return errEl;
      throw new Error(`harness has no stand-in for analytics selector ${selector}`);
    },
  };
  const controls = { range: { preset: '1h', customStart: '', customEnd: '' }, gran: 'auto', provider: '', model: '', agent: '', by: 'model', metric: 'tokens' };
  const render = (view) => (host, resp) => landed.push({ view, resp });
  const owned = ['anQuota', 'anAllTimeSince', 'analyticsReqSeq'];
  w.app = fragment({
    state: owned,
    fns: ['renderAnalyticsTab'],
    deps: {
      panels: { analytics: panel },
      apiGet: apiRecorder(calls).apiGet,
      deferAutoRefresh: interactionGate(w),
      analyticsStopAutoRefresh: () => {},
      analyticsState: () => ({ ...controls, range: controls.range }), // one snapshot per load
      ensureAccountLabels: async () => undefined,
      analyticsRangeBounds: () => ({ from: 1000, to: 2000 }),
      analyticsGranOptions, analyticsGranularity, analyticsMetricAllowed, // real gating
      buildAnalyticsLayout: (p, state) => { layouts.push(state); panel.rendered = true; },
      setRefreshError: (p, text) => banners.push(text),
      staleDataText,
      analyticsMaybeAutoRefresh: () => armed.push(true),
      analyticsFillDatalists: render('datalists'),
      analyticsFilterHint: () => {},
      analyticsRenderKpis: render('kpis'),
      analyticsRenderCharts: render('charts'),
      analyticsRenderHeatmap: render('heatmap'),
      analyticsRenderTable: render('table'),
      seqGuard: spawner.seqGuard,
    },
  });
  w.controls = controls;
  w.views = () => landed.map((l) => `${l.view}=${l.resp.tag}`);
  return guardBooked(spawner, declaredGuards(owned), [(w) => w.app.renderAnalyticsTab()], w);
}

// providerSwitch starts a load for the unfiltered view, then — with its
// /api/analytics in flight — a second one for provider p2, and lands the p2
// response first. Returns the superseded load's completion promise (boxed:
// an async function returning a bare promise would wait for it).
async function providerSwitch(w) {
  const superseded = w.app.renderAnalyticsTab();
  w.calls[0].resolve({ quota: {} });
  await flush();
  assert.equal(w.calls[1].path, ANALYTICS_UNFILTERED);
  w.controls.provider = 'p2'; // the toolbar commits a new provider filter
  const latest = w.app.renderAnalyticsTab();
  w.calls[2].resolve({ quota: {} });
  await flush();
  assert.equal(w.calls[3].path, ANALYTICS_UNFILTERED + '&provider=p2');
  w.calls[3].resolve({ tag: 'p2' });
  await latest;
  return { superseded };
}

test('F14 analytics: a response for the previous filter landing late must not re-render the panel with it', async () => {
  const w = analyticsWorld();
  const { superseded } = await providerSwitch(w);
  const rendered = ['datalists=p2', 'kpis=p2', 'charts=p2', 'heatmap=p2', 'table=p2'];
  assert.deepEqual(w.views(), rendered);
  assert.deepEqual(w.layouts.map((s) => s.provider), ['p2']);
  w.calls[1].resolve({ tag: 'unfiltered' }); // the previous filter's response lands last
  await superseded;
  assert.deepEqual(w.views(), rendered, 'the superseded response must not rebuild the panel');
  assert.deepEqual(w.layouts.map((s) => s.provider), ['p2'], 'the superseded response must not rebuild the toolbar');
  assert.deepEqual(w.armed, [true], 'only the latest load re-arms the live-window timer');
  // The latest load's success cleared the banner (the recovery rule); the
  // superseded load adds nothing.
  assert.deepEqual(w.banners, [null]);
  assert.equal(w.gate.asks, 0, 'a user-initiated load never consults the interaction gate');
});

test('F14 analytics: a late failure of a superseded load must not raise the stale banner', async () => {
  const w = analyticsWorld();
  const { superseded } = await providerSwitch(w);
  const rendered = w.views();
  w.calls[1].reject(new Error('daemon down'));
  await superseded;
  assert.deepEqual(w.banners, [null], 'a superseded failure must not mark the fresh view as stale');
  assert.deepEqual(w.views(), rendered);
  assert.deepEqual(w.armed, [true], 'a superseded failure must not re-arm the timer');
});

test('F14 analytics: a load superseded during its /api/status prefetch stops there', async () => {
  const w = analyticsWorld();
  const superseded = w.app.renderAnalyticsTab();
  w.controls.provider = 'p2';
  const latest = w.app.renderAnalyticsTab();
  w.calls[1].resolve({ quota: { fresh: true } }); // the NEWER load's status lands first
  await flush();
  assert.equal(w.calls.length, 3, "status x2 + the newer load's analytics query");
  w.calls[0].resolve({ quota: { stale: true } }); // the older status lands last
  await flush();
  assert.deepEqual(w.app.anQuota, { fresh: true }, 'a superseded load must not overwrite the quota snapshot');
  assert.equal(w.calls.length, 3, 'a superseded load must not go on to issue its analytics query');
  await superseded;
  w.calls[2].resolve({ tag: 'p2' });
  await latest;
  assert.deepEqual(w.views(), ['datalists=p2', 'kpis=p2', 'charts=p2', 'heatmap=p2', 'table=p2']);
});

test('F14 analytics: the gates are unchanged — an entry park retires nothing, a commit park writes nothing', async () => {
  const w = analyticsWorld();
  // Entry gate: a tick arriving mid-interaction returns without touching
  // the in-flight foreground load.
  const foreground = w.app.renderAnalyticsTab();
  w.calls[0].resolve({ quota: {} });
  await flush();
  w.gatePlan.push(true);
  await w.app.renderAnalyticsTab(true);
  assert.equal(w.calls.length, 2, 'a load parked at the entry gate must not fetch');
  w.calls[1].resolve({ tag: 'foreground' });
  await foreground;
  const rendered = ['datalists=foreground', 'kpis=foreground', 'charts=foreground', 'heatmap=foreground', 'table=foreground'];
  assert.deepEqual(w.views(), rendered, 'a tick parked at the entry gate must not retire the in-flight load');
  assert.equal(w.parked.length, 1);
  // Commit gate: a background load landing mid-interaction parks before
  // any write; the hold watcher re-runs a BACKGROUND render.
  w.gatePlan.push(false, true);
  const background = w.app.renderAnalyticsTab(true);
  w.calls[2].resolve({ quota: {} });
  await flush();
  w.calls[3].resolve({ tag: 'background' });
  await background;
  assert.deepEqual(w.views(), rendered, 'a load parked at the commit gate must not render');
  assert.equal(w.layouts.length, 1);
  assert.deepEqual(w.parked.map((cb) => cb.toString()), ['() => renderAnalyticsTab(true)', '() => renderAnalyticsTab(true)']);
  assert.equal(w.gate.asks, 3, 'entry park + entry pass + commit park');
});

test('F14 analytics: a superseded background load is dropped before the commit gate — it parks no redundant re-render', async () => {
  const w = analyticsWorld();
  w.gatePlan.push(false); // the tick's entry passes
  const tick = w.app.renderAnalyticsTab(true);
  w.calls[0].resolve({ quota: {} });
  await flush();
  w.controls.provider = 'p2';
  const foreground = w.app.renderAnalyticsTab();
  w.calls[2].resolve({ quota: {} });
  await flush();
  w.calls[3].resolve({ tag: 'p2' });
  await foreground;
  const rendered = ['datalists=p2', 'kpis=p2', 'charts=p2', 'heatmap=p2', 'table=p2'];
  // The user is interacting now — a background landing would be parked.
  w.gatePlan.push(true);
  w.calls[1].resolve({ tag: 'tick-stale' });
  await tick;
  assert.deepEqual(w.views(), rendered);
  assert.equal(w.gate.asks, 1, "only the tick's entry pass: a dropped load never reaches the commit gate");
  assert.equal(w.parked.length, 0, 'a superseded load must not leave a hold watcher re-rendering behind');
});

test('F14 analytics: the latest failure is still reported — stale banner over a kept view, inline error on first render', async () => {
  const w = analyticsWorld();
  const warm = w.app.renderAnalyticsTab();
  w.calls[0].resolve({ quota: {} });
  await flush();
  w.calls[1].resolve({ tag: 'ok' });
  await warm;
  const failing = w.app.renderAnalyticsTab();
  w.calls[2].resolve({ quota: {} });
  await flush();
  w.calls[3].reject(new Error('daemon down'));
  await failing;
  assert.deepEqual(w.banners, [null, staleDataText('Analytics unavailable: daemon down')]);
  assert.deepEqual(w.views(), ['datalists=ok', 'kpis=ok', 'charts=ok', 'heatmap=ok', 'table=ok'],
    'the latest failure keeps the previous view');
  assert.deepEqual(w.armed, [true, true], 'each landed load re-arms the timer');
  // Nothing rendered yet: skeleton + the inline error, toolbar still wired.
  const cold = analyticsWorld();
  const first = cold.app.renderAnalyticsTab();
  cold.calls[0].resolve({ quota: {} });
  await flush();
  cold.calls[1].reject(new Error('daemon down'));
  await first;
  assert.equal(cold.layouts.length, 1);
  assert.deepEqual(cold.landed, []);
  assert.equal(cold.errEl.hidden, false);
  assert.equal(cold.errEl.textContent, 'Analytics unavailable: daemon down');
  assert.deepEqual(cold.armed, [true]);
});

// ---------------------------------------------------------------------------
// Accounts tab (F14)
// ---------------------------------------------------------------------------

const usage = (requests) => [{ model: 'm', requests }];

function accountsWorld() {
  const doc = fakeDocument();
  const msgHost = element(doc, 'acc-msg');
  doc.ids.set('acc-msg', msgHost);
  const calls = [];
  const navs = []; // renderAccountsNav(providers)
  const banners = []; // setRefreshError(panel, text|null)
  const conns = []; // setConn(state)
  const msgs = []; // showMsg(host, kind, text)
  const spawner = guardSpawner();
  const w = { doc, msgHost, calls, navs, banners, conns, msgs, parked: [], gatePlan: [], gate: { asks: 0 } };
  const panel = {
    rendered: false, // .acct-nav-title exists once renderAccountsNav ran
    querySelector(selector) {
      if (selector === '.acct-nav-title') return this.rendered ? {} : null;
      throw new Error(`harness has no stand-in for accounts selector ${selector}`);
    },
  };
  const owned = ['accountsCache', 'accountsQuota', 'accountsTokens', 'accountsSelectedProvider', 'accountsRange',
    'accountsTokensQueryLast', 'accountsReqSeq'];
  w.app = fragment({
    state: owned,
    fns: ['accountsQuotaFromSec', 'accountsEffectiveRange', 'accountsTokensQuery', 'loadAccountsData'],
    deps: {
      document: doc,
      panels: { accounts: panel },
      apiGet: apiRecorder(calls).apiGet,
      accountProviderKey, quotaUsageFromSec, tokensRangeQuery, // the real window derivation
      deferAutoRefresh: interactionGate(w),
      renderAccountsNav: (providers) => { navs.push(providers); panel.rendered = true; },
      setRefreshError: (p, text) => banners.push(text),
      setConn: (state) => conns.push(state),
      staleDataText,
      showMsg: (host, kind, text) => msgs.push({ host, kind, text }),
      seqGuard: spawner.seqGuard,
    },
    exposes: 'setRange(range) { accountsRange = range; }',
  });
  return guardBooked(spawner, declaredGuards(owned), [(w) => w.app.loadAccountsData()], w);
}

// landAccounts settles the accounts load whose calls start at `from`
// (/api/status, then /api/accounts + /api/tokens in parallel).
async function landAccounts(w, from, usageRows) {
  w.calls[from].resolve({ quota: {} });
  await flush();
  w.calls[from + 1].resolve({ providers: [{ name: 'p1' }] });
  w.calls[from + 2].resolve({ usage: usageRows });
}

// tickThenRangeChange runs the brief's race: the 30s background tick is past
// its /api/status stage waiting on the all-time token query, the user applies
// a 7d range (a foreground re-fetch), and the foreground lands first.
// Returns the superseded tick's completion promise (boxed — see
// providerSwitch).
async function tickThenRangeChange(w) {
  // The tick consults the commit gate if its landing is not dropped first:
  // let it through, so a surviving stale commit shows up as the data bug
  // itself rather than as an unplanned gate ask.
  w.gatePlan.push(false);
  const tick = w.app.loadAccountsData(true);
  w.calls[0].resolve({ quota: {} });
  await flush();
  assert.deepEqual(w.calls.map((c) => c.path), ['/api/status', '/api/accounts', '/api/tokens']);
  w.app.setRange({ preset: '7d', customStart: '', customEnd: '' }); // the picker's apply path
  const foreground = w.app.loadAccountsData();
  w.calls[3].resolve({ quota: {} });
  await flush();
  assert.equal(w.calls.length, 6);
  assert.notEqual(w.calls[5].path, '/api/tokens', 'the foreground load queries the 7d window');
  w.calls[4].resolve({ providers: [{ name: 'p1' }] });
  w.calls[5].resolve({ usage: usage(7) });
  await foreground;
  assert.deepEqual(w.app.accountsTokens, usage(7));
  assert.deepEqual(w.navs, [[{ name: 'p1' }]]);
  assert.equal(w.app.accountsTokensQueryLast, w.calls[5].path.slice('/api/tokens'.length),
    'the landed payload is recorded with its own query');
  return { tick };
}

test('F14 accounts: a background tick landing after a range change must not put the previous window back', async () => {
  const w = accountsWorld();
  const { tick } = await tickThenRangeChange(w);
  const landedQuery = w.app.accountsTokensQueryLast;
  w.calls[1].resolve({ providers: [{ name: 'p1-stale' }] }); // the tick's payload lands last
  w.calls[2].resolve({ usage: usage(99) });
  await tick;
  assert.deepEqual(w.app.accountsTokens, usage(7), 'the superseded tick must not restore the all-time rows');
  assert.deepEqual(w.navs, [[{ name: 'p1' }]], 'the superseded tick must not re-render the nav');
  assert.equal(w.app.accountsTokensQueryLast, landedQuery);
  assert.equal(w.gate.asks, 0,
    'the foreground load is user-initiated and the superseded tick is dropped before the commit gate');
});

test('F14 accounts: a late failure of a superseded tick must not raise the stale banner', async () => {
  const w = accountsWorld();
  const { tick } = await tickThenRangeChange(w);
  w.calls[1].reject(new Error('daemon down'));
  await tick;
  assert.deepEqual(w.banners, [null], 'a superseded failure must not mark the fresh view as stale');
  assert.deepEqual(w.conns, [], 'a superseded failure must not flip the connection indicator');
  assert.deepEqual(w.app.accountsTokens, usage(7));
  assert.equal(w.navs.length, 1);
});

test('F14 accounts: a load superseded during its /api/status stage stops there', async () => {
  const w = accountsWorld();
  const tick = w.app.loadAccountsData(true);
  const foreground = w.app.loadAccountsData();
  w.calls[1].resolve({ quota: { fresh: true } }); // the NEWER load's status lands first
  await flush();
  assert.equal(w.calls.length, 4, "status x2 + the newer load's accounts + tokens");
  w.calls[0].resolve({ quota: { stale: true } }); // the older status lands last
  await flush();
  assert.deepEqual(w.app.accountsQuota, { fresh: true }, 'a superseded load must not overwrite the quota snapshot');
  assert.equal(w.calls.length, 4, 'a superseded load must not go on to fetch accounts/tokens');
  await tick;
  assert.equal(w.gate.asks, 0, 'a superseded background load never reaches the commit gate');
  w.calls[2].resolve({ providers: [{ name: 'p1' }] });
  w.calls[3].resolve({ usage: usage(7) });
  await foreground;
  assert.deepEqual(w.app.accountsTokens, usage(7));
  assert.deepEqual(w.navs, [[{ name: 'p1' }]]);
});

test('F14 accounts: the background commit gate is unchanged — a landing background load parks, a foreground load never asks', async () => {
  const w = accountsWorld();
  w.gatePlan.push(true);
  const tick = w.app.loadAccountsData(true);
  await landAccounts(w, 0, usage(1));
  await tick;
  assert.equal(w.gate.asks, 1);
  assert.equal(w.navs.length, 0, 'a load parked at the commit gate must not render');
  assert.equal(w.app.accountsCache, null, 'a load parked at the commit gate must not commit its data');
  // The hold watcher re-runs a BACKGROUND load: it consults the gate again
  // and lands once the interaction is over.
  w.gatePlan.push(false);
  assert.deepEqual(w.parked.map((cb) => cb.toString()), ['() => loadAccountsData(true)']);
  const rerun = w.parked[0]();
  assert.equal(w.calls.length, 4, 'the parked callback re-fetches /api/status');
  await landAccounts(w, 3, usage(1));
  await rerun;
  assert.equal(w.gate.asks, 2, 'the re-run is a background load');
  assert.deepEqual(w.navs, [[{ name: 'p1' }]]);
});

test('F14 accounts: a superseded background tick is dropped before the commit gate — it parks no redundant re-fetch', async () => {
  const w = accountsWorld();
  const tick = w.app.loadAccountsData(true);
  w.calls[0].resolve({ quota: {} });
  await flush();
  w.app.setRange({ preset: '7d', customStart: '', customEnd: '' });
  const foreground = w.app.loadAccountsData();
  w.calls[3].resolve({ quota: {} });
  await flush();
  w.calls[4].resolve({ providers: [{ name: 'p1' }] });
  w.calls[5].resolve({ usage: usage(7) });
  await foreground;
  // The user is interacting now — a background landing would be parked.
  w.gatePlan.push(true);
  w.calls[1].resolve({ providers: [{ name: 'p1-stale' }] });
  w.calls[2].resolve({ usage: usage(99) });
  await tick;
  assert.deepEqual(w.app.accountsTokens, usage(7));
  assert.equal(w.navs.length, 1);
  assert.equal(w.gate.asks, 0, 'a dropped load never reaches the commit gate');
  assert.equal(w.parked.length, 0, 'a superseded tick must not leave a hold watcher re-fetching behind');
});

test('F14 accounts: the latest failure is still reported — stale banner over a kept view, inline message on first load', async () => {
  const w = accountsWorld();
  w.gatePlan.push(false);
  const warm = w.app.loadAccountsData(true);
  await landAccounts(w, 0, usage(1));
  await warm;
  const failing = w.app.loadAccountsData(true);
  w.calls[3].resolve({ quota: {} });
  await flush();
  w.calls[4].reject(new Error('daemon down'));
  await failing;
  assert.deepEqual(w.banners, [null, staleDataText('refresh failed', ['accounts'])]);
  assert.deepEqual(w.conns, ['err']);
  assert.deepEqual(w.app.accountsTokens, usage(1), 'the previous rows stay on screen');
  assert.equal(w.navs.length, 1);
  // First load, nothing rendered: the inline message carries the backend text.
  const cold = accountsWorld();
  const first = cold.app.loadAccountsData();
  // /api/accounts is the load's hard half (a failed /api/status is settled
  // per-part and never fails the load).
  cold.calls[0].resolve({ quota: {} });
  await flush();
  cold.calls[1].reject(new Error('daemon down'));
  await first;
  assert.deepEqual(cold.msgs, [{ host: cold.msgHost, kind: 'err', text: 'daemon down' }]);
  assert.deepEqual(cold.conns, ['err']);
  assert.deepEqual(cold.banners, []);
});

// ---------------------------------------------------------------------------
// The loaders that already carried a hand-rolled sequence counter
// ---------------------------------------------------------------------------
//
// loadRequests / loadSecurity / loadSecurityRules / loadMCPAnalytics /
// loadTakeover guarded their landings with a module-level counter long before
// seqGuard existed. Each landing point is pinned here by behavior, so moving
// them onto the shared guard is a refactor under test: these cases passed
// against the hand-rolled counters and must keep passing unchanged.

// Every loadRequests call issues two requests in this order: the MCP session
// pool (kind=mcp, unfiltered) and then the list itself.
const REQUESTS_POOL = '/api/requests?kind=mcp&limit=500';
const REQUESTS_LIST = '/api/requests?limit=50';
const REQUESTS_STATE = ['reqVirt', 'reqLoadSeq', 'REQ_BROWSE_LIMIT', 'REQ_SESSION_LIMIT', 'REQ_MAX_LOADED'];

function requestsWorld() {
  const doc = fakeDocument();
  const table = element(doc, 'req-table');
  // The virtual-table hand-off reads two nodes out of the painted markup.
  table.querySelector = (selector) => (selector === 'table' ? { tBodies: [{}] } : {});
  doc.ids.set('req-table', table);
  doc.ids.set('req-session', element(doc, 'req-session'));
  const spawner = guardSpawner();
  const calls = [];
  // The page config is one stable object — the pool callback compares it by
  // identity before re-rendering the selectors.
  const page = { pool: 'mcp-records', table: {} };
  const spies = spyBag(['reqTearDown', 'dropRawBodies', 'hideRequestsSessionSummary', 'syncRequestFacets',
    'renderRequestsSessionSummary', 'renderRequestSelectors', 'wireReqScroll', 'reqHint', 'reqFrame']);
  const app = fragment({
    state: REQUESTS_STATE,
    fns: ['loadRequests'],
    deps: {
      document: doc,
      ...apiRecorder(calls),
      esc, requestTableHeadHTML,
      reqFilterParams: () => new URLSearchParams(),
      requestsFilter: { session: '' },
      activeRequestsPage: () => page,
      mcpPoolFromRecords: (records) => records.map((r) => r.session_id),
      bodyChunkRegistry: new Map(),
      ...spies.fns,
      seqGuard: spawner.seqGuard,
    },
  });
  return guardBooked(spawner, declaredGuards(REQUESTS_STATE),
    [(w) => w.app.loadRequests({ lastRecords: [], sessionPool: null })], { app, calls, table, spies: spies.calls });
}

test('loadRequests: a list response superseded in flight lands nowhere', async () => {
  const w = requestsWorld();
  const combos = { lastRecords: [], sessionPool: null };
  const stale = w.app.loadRequests(combos);
  const latest = w.app.loadRequests(combos);
  assert.deepEqual(w.calls.map((c) => c.path), [REQUESTS_POOL, REQUESTS_LIST, REQUESTS_POOL, REQUESTS_LIST]);
  w.calls[3].resolve({ enabled: true, records: [{ request_id: 'latest' }], facets: { providers: ['p2'] } });
  await latest;
  const landedRows = combos.lastRecords;
  assert.deepEqual(landedRows, [{ request_id: 'latest' }]);
  assert.deepEqual(w.spies.syncRequestFacets, [[{ providers: ['p2'] }, combos]]);
  const landedHtml = w.table.innerHTML;
  assert.match(landedHtml, /^<table class="table">/);
  w.calls[1].resolve({ enabled: true, records: [{ request_id: 'stale' }], facets: { providers: ['p1'] } });
  await stale;
  assert.equal(combos.lastRecords, landedRows, 'the superseded response must not replace the loaded records');
  assert.equal(w.spies.syncRequestFacets.length, 1, '…must not re-sync the facets');
  assert.equal(w.spies.renderRequestsSessionSummary.length, 1, '…must not re-render the session summary');
  assert.equal(w.table.innerHTML, landedHtml, '…must not repaint the table');
  assert.equal(w.spies.reqFrame.length, 1, '…must not start a second virtual table');
});

test('loadRequests: a session pool that outlives its list load does not write into the newer load', async () => {
  const w = requestsWorld();
  const combos = { lastRecords: [], sessionPool: ['before'] };
  const first = w.app.loadRequests(combos);
  // The first LIST lands while it is still the latest, so its pool callback
  // is armed; its POOL stays in flight across the next load.
  w.calls[1].resolve({ enabled: true, records: [{ request_id: 'first' }] });
  await first;
  const second = w.app.loadRequests(combos);
  w.calls[3].resolve({ enabled: true, records: [{ request_id: 'second' }] });
  await second;
  w.calls[0].resolve([{ session_id: 'stale-pool' }]);
  await flush();
  assert.deepEqual(combos.sessionPool, ['before'], 'a superseded pool must not be written');
  assert.deepEqual(w.spies.renderRequestSelectors, [], '…and must not re-render the selectors');
  w.calls[2].resolve([{ session_id: 'fresh-pool' }]);
  await flush();
  assert.deepEqual(combos.sessionPool, ['fresh-pool']);
  assert.deepEqual(w.spies.renderRequestSelectors, [[combos]]);
});

test('loadRequests: a superseded list failure paints no error', async () => {
  const w = requestsWorld();
  const combos = { lastRecords: [], sessionPool: null };
  const stale = w.app.loadRequests(combos);
  const latest = w.app.loadRequests(combos);
  w.calls[1].reject(new Error('stale boom'));
  await stale;
  assert.doesNotMatch(w.table.innerHTML, /msg err/, 'a superseded failure must not paint its error');
  assert.deepEqual(w.spies.hideRequestsSessionSummary, []);
  w.calls[3].reject(new Error('daemon down'));
  await latest;
  assert.equal(w.table.innerHTML, `<div class="msg err">${esc('daemon down')}</div>`, 'the latest failure is shown');
  assert.deepEqual(w.spies.hideRequestsSessionSummary, [[]]);
});

const SECURITY_STATE = ['securityFilter', 'securityReqSeq', 'securityLimit', 'securityAuditOn', 'securityFeedSkipped',
  'securityKpiData', 'securityFeedData', 'securityRulesData', 'securityRulesSeq'];

function securityWorld() {
  const doc = fakeDocument();
  doc.ids.set('sec-table', element(doc, 'sec-table'));
  const spawner = guardSpawner();
  const calls = [];
  const spies = spyBag(['securityRefreshOk', 'commitSecurityRender']);
  const refreshFail = [];
  const app = fragment({
    state: SECURITY_STATE,
    fns: ['loadSecurity', 'loadSecurityRules'],
    deps: {
      document: doc,
      ...apiRecorder(calls),
      esc, securityRangeFromSecs,
      ...spies.fns,
      // The real function also decides whether a first-load failure shows an
      // inline error card; false keeps that branch out of these scenes.
      securityRefreshFail: (part) => { refreshFail.push([part]); return false; },
      seqGuard: spawner.seqGuard,
    },
    exposes: 'setKind(kind) { securityFilter = { ...securityFilter, kind }; }',
  });
  spies.calls.securityRefreshFail = refreshFail;
  return guardBooked(spawner, declaredGuards(SECURITY_STATE),
    [(w) => w.app.loadSecurity(), (w) => w.app.loadSecurityRules()], { app, calls, spies: spies.calls });
}

test('loadSecurity: a superseded audit response lands nowhere', async () => {
  const w = securityWorld();
  const stale = w.app.loadSecurity();
  w.app.setKind('secret'); // the user narrows the feed
  const latest = w.app.loadSecurity();
  assert.deepEqual(w.calls.map((c) => c.path), ['/api/security?limit=100', '/api/security?limit=100&kind=secret']);
  w.calls[1].resolve({ enabled: true, skipped: 2, records: [{ id: 'latest' }], counts: { high: 3 } });
  await latest;
  assert.deepEqual(w.app.securityFeedData.records, [{ id: 'latest' }]);
  assert.deepEqual(w.app.securityKpiData.counts, { high: 3 });
  assert.equal(w.spies.commitSecurityRender.length, 1);
  w.calls[0].resolve({ enabled: false, skipped: 99, records: [{ id: 'stale' }], counts: { high: 99 } });
  await stale;
  assert.deepEqual(w.app.securityFeedData.records, [{ id: 'latest' }], 'the superseded response must not replace the feed');
  assert.deepEqual(w.app.securityKpiData.counts, { high: 3 }, '…nor the KPI counts');
  assert.equal(w.app.securityAuditOn, true, '…nor flip the audit-on flag');
  assert.equal(w.spies.securityRefreshOk.length, 1);
  assert.equal(w.spies.commitSecurityRender.length, 1, '…nor commit a render');
});

test('loadSecurity: a superseded audit failure flags no part', async () => {
  const w = securityWorld();
  const stale = w.app.loadSecurity();
  const latest = w.app.loadSecurity();
  w.calls[0].reject(new Error('stale boom'));
  await stale;
  assert.deepEqual(w.spies.securityRefreshFail, [], 'a superseded failure must not mark the part failed');
  w.calls[1].reject(new Error('daemon down'));
  await latest;
  assert.deepEqual(w.spies.securityRefreshFail, [['security']], 'the latest failure is still reported');
});

test('loadSecurityRules: a superseded response — success or failure — lands nowhere', async () => {
  const w = securityWorld();
  const stale = w.app.loadSecurityRules();
  const latest = w.app.loadSecurityRules();
  assert.deepEqual(w.calls.map((c) => c.path), ['/api/security?limit=1000', '/api/security?limit=1000']);
  w.calls[1].resolve({ records: [{ id: 'latest' }] });
  await latest;
  w.calls[0].resolve({ records: [{ id: 'stale' }] });
  await stale;
  assert.deepEqual(w.app.securityRulesData.records, [{ id: 'latest' }], 'the superseded response must not replace the leaderboard slice');
  assert.deepEqual(w.spies.securityRefreshOk, [['rules']]);
  assert.equal(w.spies.commitSecurityRender.length, 1);

  const failing = securityWorld();
  const staleFail = failing.app.loadSecurityRules();
  const latestFail = failing.app.loadSecurityRules();
  failing.calls[0].reject(new Error('stale boom'));
  await staleFail;
  assert.deepEqual(failing.spies.securityRefreshFail, [], 'a superseded failure must not mark the part failed');
  failing.calls[1].reject(new Error('daemon down'));
  await latestFail;
  assert.deepEqual(failing.spies.securityRefreshFail, [['rules']], 'the latest failure is still reported');
});

test('loadSecurity and loadSecurityRules run side by side — neither retires the other', async () => {
  // Every Security refresh fires both loaders at once, so their guards must
  // be independent: a token taken by one is not retired by the other.
  const w = securityWorld();
  const audit = w.app.loadSecurity();
  const rules = w.app.loadSecurityRules();
  assert.deepEqual(w.calls.map((c) => c.path), ['/api/security?limit=100', '/api/security?limit=1000']);
  w.calls[1].resolve({ records: [{ id: 'rules' }] });
  await rules;
  w.calls[0].resolve({ enabled: true, records: [{ id: 'audit' }] });
  await audit;
  assert.deepEqual(w.app.securityRulesData.records, [{ id: 'rules' }], 'the rules slice lands');
  assert.deepEqual(w.app.securityFeedData.records, [{ id: 'audit' }], 'the audit half lands too');
  assert.deepEqual(w.spies.commitSecurityRender.length, 2, 'both halves commit');
});

const MCP_ANALYTICS_PATH = '/api/mcp/analytics?from=1000&to=2000&granularity=hour';
const MCP_ANALYTICS_STATE = ['mcpAnalyticsData', 'mcpAnalyticsError', 'mcpAnalyticsLoading', 'mcpAnalyticsReqSeq',
  'mcpAllTimeSince'];

function mcpAnalyticsWorld() {
  const calls = [];
  const banners = []; // setRefreshError(panel, text|null)
  const spawner = guardSpawner();
  const spies = spyBag(['mcpAnalyticsStopAutoRefresh', 'renderMCPAnalytics', 'mcpAnalyticsMaybeAutoRefresh']);
  const controls = { server: '' };
  const host = {};
  const w = { calls, banners, host, controls, spies: spies.calls, parked: [], gatePlan: [], gate: { asks: 0 } };
  w.app = fragment({
    state: MCP_ANALYTICS_STATE,
    fns: ['loadMCPAnalytics'],
    deps: {
      panels: { mcp: {} },
      deferAutoRefresh: interactionGate(w),
      ...spies.fns,
      mcpAnalyticsState: () => ({ range: { preset: '1h', customStart: '', customEnd: '' }, gran: 'auto', metric: 'calls', server: controls.server, tool: '' }),
      mcpAnalyticsEffectiveGran: () => ({ bounds: { from: 1000, to: 2000 }, granOptions: [], granActive: 'auto', gran: 'hour' }),
      apiGet: apiRecorder(calls).apiGet,
      analyticsGranularity,
      setRefreshError: (p, text) => banners.push(text),
      staleDataText,
      seqGuard: spawner.seqGuard,
    },
  });
  return guardBooked(spawner, declaredGuards(MCP_ANALYTICS_STATE), [(w) => w.app.loadMCPAnalytics(w.host)], w);
}

test('loadMCPAnalytics: a superseded load renders neither its data nor its failure and leaves the in-flight flag to the latest one', async () => {
  const w = mcpAnalyticsWorld();
  const stale = w.app.loadMCPAnalytics(w.host);
  assert.equal(w.app.mcpAnalyticsLoading, true);
  w.controls.server = 'srv-b'; // the Server filter changes
  const latest = w.app.loadMCPAnalytics(w.host);
  assert.deepEqual(w.calls.map((c) => c.path), [MCP_ANALYTICS_PATH, MCP_ANALYTICS_PATH + '&name=srv-b']);
  w.calls[0].resolve({ tag: 'stale' }); // the superseded load lands first
  await stale;
  assert.equal(w.app.mcpAnalyticsData, null, 'a superseded response must not become the rendered data');
  assert.deepEqual(w.spies.renderMCPAnalytics, []);
  assert.deepEqual(w.banners, []);
  assert.equal(w.app.mcpAnalyticsLoading, true, "a superseded load's finally must not clear the in-flight flag");
  w.calls[1].resolve({ tag: 'latest' });
  await latest;
  assert.equal(w.app.mcpAnalyticsData.tag, 'latest');
  assert.deepEqual(w.spies.renderMCPAnalytics, [[w.host]]);
  assert.deepEqual(w.banners, [null]);
  assert.equal(w.app.mcpAnalyticsLoading, false, 'the latest load clears the flag');
  assert.deepEqual(w.spies.mcpAnalyticsMaybeAutoRefresh.length, 1, 'only the latest load re-arms the timer');
});

test('loadMCPAnalytics: a superseded failure raises no banner; the latest one is still reported', async () => {
  const w = mcpAnalyticsWorld();
  const warm = w.app.loadMCPAnalytics(w.host); // a rendered view exists
  w.calls[0].resolve({ tag: 'warm' });
  await warm;
  const stale = w.app.loadMCPAnalytics(w.host);
  const latest = w.app.loadMCPAnalytics(w.host);
  w.calls[1].reject(new Error('stale boom'));
  await stale;
  assert.deepEqual(w.banners, [null], 'a superseded failure must not mark the view stale');
  assert.equal(w.app.mcpAnalyticsLoading, true);
  w.calls[2].reject(new Error('daemon down'));
  await latest;
  assert.deepEqual(w.banners, [null, staleDataText('MCP analytics unavailable: daemon down')]);
  assert.equal(w.app.mcpAnalyticsData.tag, 'warm', 'the latest failure keeps the previous data');
  assert.equal(w.app.mcpAnalyticsLoading, false);
  // Nothing rendered yet: the latest failure is kept as the error and rendered.
  const cold = mcpAnalyticsWorld();
  const first = cold.app.loadMCPAnalytics(cold.host);
  cold.calls[0].reject(new Error('daemon down'));
  await first;
  assert.equal(cold.app.mcpAnalyticsError, 'daemon down');
  assert.deepEqual(cold.banners, [staleDataText('MCP analytics unavailable: daemon down')]);
  assert.deepEqual(cold.spies.renderMCPAnalytics, [[cold.host]]);
  assert.equal(cold.app.mcpAnalyticsLoading, false);
});

test('loadMCPAnalytics: a superseded success does not clear the error the latest failure left', async () => {
  // Nothing is rendered yet, so the LATEST load's failure message is what the
  // view shows. A superseded load succeeding afterwards must not wipe it:
  // with the error gone, the next render falls back to the loading skeleton
  // and quietly re-fetches instead of showing the failure.
  const w = mcpAnalyticsWorld();
  const stale = w.app.loadMCPAnalytics(w.host);
  const latest = w.app.loadMCPAnalytics(w.host);
  w.calls[1].reject(new Error('daemon down'));
  await latest;
  assert.equal(w.app.mcpAnalyticsError, 'daemon down');
  w.calls[0].resolve({ tag: 'stale' }); // the superseded load lands afterwards
  await stale;
  assert.equal(w.app.mcpAnalyticsError, 'daemon down',
    "a superseded success must not clear the latest failure's error");
});

const TAKEOVER_STATE = ['takeoverData', 'takeoverReqSeq'];

function takeoverWorld() {
  const spawner = guardSpawner();
  const calls = [];
  const banners = []; // setRefreshError(panel, text|null)
  const spies = spyBag(['renderTakeoverInto']);
  const app = fragment({
    state: TAKEOVER_STATE,
    fns: ['loadTakeover'],
    deps: {
      panels: { takeover: {} },
      ...apiRecorder(calls),
      ...spies.fns,
      setRefreshError: (p, text) => banners.push(text),
      seqGuard: spawner.seqGuard,
    },
  });
  return guardBooked(spawner, declaredGuards(TAKEOVER_STATE), [(w) => w.app.loadTakeover()],
    { app, calls, banners, spies: spies.calls });
}

test('loadTakeover: a superseded surface — success or failure — lands nowhere', async () => {
  const w = takeoverWorld();
  const stale = w.app.loadTakeover();
  const latest = w.app.loadTakeover();
  w.calls[1].resolve({ clients: [{ name: 'latest' }] });
  await latest;
  w.calls[0].resolve({ clients: [{ name: 'stale' }] });
  await stale;
  assert.deepEqual(w.app.takeoverData, { clients: [{ name: 'latest' }] }, 'the superseded surface must not overwrite the newer one');
  assert.equal(w.spies.renderTakeoverInto.length, 1, '…nor re-render');
  assert.deepEqual(w.banners, [null]);

  const failing = takeoverWorld();
  const staleFail = failing.app.loadTakeover();
  const latestFail = failing.app.loadTakeover();
  failing.calls[1].resolve({ clients: [] });
  await latestFail;
  failing.calls[0].reject(new Error('stale boom'));
  await staleFail;
  assert.deepEqual(failing.banners, [null], 'a superseded failure must not raise the banner');
  const latestFail2 = failing.app.loadTakeover();
  failing.calls[2].reject(new Error('daemon down'));
  await latestFail2;
  assert.deepEqual(failing.banners, [null, 'daemon down'], 'the latest failure is still reported');
});

// ---------------------------------------------------------------------------
// The two request guards that are not loaders
// ---------------------------------------------------------------------------
//
// The YAML editor's debounced lint and the takeover run dialog's preview take
// a latest-wins token for the same reason the loaders do — a slow older
// response must not land over a newer one — and rolled their own counters for
// just as long. Characterized the same way, landing point by landing point.

const YAML_LINT_STATE = ['yamlLintTimer', 'yamlLintSeq', 'yamlLintErrors'];

function yamlLintWorld() {
  const spawner = guardSpawner();
  const calls = [];
  const spies = spyBag(['renderYamlLint', 'renderRestartHint', 'updateYamlSaveState']);
  const editor = { text: 'a: 1' }; // what the mounted YAML editor holds
  const app = fragment({
    state: YAML_LINT_STATE,
    fns: ['runYamlLint'],
    deps: {
      ...apiRecorder(calls),
      getYamlValue: () => editor.text,
      ...spies.fns,
      seqGuard: spawner.seqGuard,
    },
  });
  return guardBooked(spawner, declaredGuards(YAML_LINT_STATE), [(w) => w.app.runYamlLint()],
    { app, calls, spies: spies.calls, editor });
}

test('runYamlLint: a lint response superseded by a newer edit lands nowhere', async () => {
  const w = yamlLintWorld();
  const stale = w.app.runYamlLint();
  w.editor.text = 'b: 2'; // the user keeps typing
  const latest = w.app.runYamlLint();
  assert.deepEqual(w.calls.map((c) => [c.path, c.body]), [
    ['/api/config/validate', { yaml: 'a: 1' }],
    ['/api/config/validate', { yaml: 'b: 2' }],
  ]);
  w.calls[1].resolve({ errors: [{ line: 2, message: 'latest' }] });
  await latest;
  assert.deepEqual(w.app.yamlLintErrors, [{ line: 2, message: 'latest' }]);
  assert.equal(w.spies.renderYamlLint.length, 1);
  w.calls[0].resolve({ errors: [{ line: 1, message: 'stale' }] });
  await stale;
  assert.deepEqual(w.app.yamlLintErrors, [{ line: 2, message: 'latest' }], 'the superseded response must not replace the lint result');
  assert.equal(w.spies.renderYamlLint.length, 1, '…nor re-render the lint');
  assert.equal(w.spies.updateYamlSaveState.length, 1, '…nor re-run the save-state arbitration');
});

test('runYamlLint: a superseded transport failure clears nothing; the latest one still does', async () => {
  const w = yamlLintWorld();
  const warm = w.app.runYamlLint();
  w.calls[0].resolve({ errors: [{ line: 1, message: 'warm' }] });
  await warm;
  const stale = w.app.runYamlLint();
  w.editor.text = 'c: 3';
  const latest = w.app.runYamlLint();
  w.calls[1].reject(new Error('stale boom'));
  await stale;
  assert.deepEqual(w.app.yamlLintErrors, [{ line: 1, message: 'warm' }], 'a superseded failure must not clear the lint result');
  w.calls[2].reject(new Error('daemon down'));
  await latest;
  assert.deepEqual(w.app.yamlLintErrors, [],
    'the latest transport failure still clears the errors — saving must not be blocked');
});

test('runYamlLint: nothing to lint means no token and no request', async () => {
  const w = yamlLintWorld();
  w.editor.text = null; // the editor is not mounted
  await w.app.runYamlLint();
  assert.deepEqual(w.calls, []);
  assert.deepEqual(w.spies.renderYamlLint, []);
  assert.equal(w.app.yamlLintTimer, 0, 'the early return still clears the debounce handle');
});

// loadPreviewNow lives in openTakeoverConfirm's closure and so does its guard:
// one instance per dialog open, exactly like the counter it replaces.
const TAKEOVER_PREVIEW_GUARDS = ['previewSeq'];

function takeoverPreviewWorld() {
  const spawner = guardSpawner();
  const calls = [];
  const timers = []; // setTimeout(fn, ms) stand-ins, in schedule order
  const cleared = []; // clearTimeout(id) stand-ins, in call order
  const doc = fakeDocument();
  const modal = element(doc, 'tk-run-modal');
  const writesEl = element(doc, 'tkr-writes');
  const msgEl = element(doc, 'tkr-msg');
  const runBtn = element(doc, 'tkr-run');
  const summary = element(doc, 'tkr-summary');
  // The opener resolves these nodes off the dialog markup once; the loader
  // re-resolves the status line on every call.
  modal.querySelector = (selector) => ({
    '#tkr-writes': writesEl, '#tkr-msg': msgEl, '#tkr-run': runBtn,
    '#tkr-status': element(doc, 'tkr-status'), '#tkr-summary': summary,
  })[selector] || null;
  modal.dataset = {};
  writesEl.querySelectorAll = () => [];
  const selection = { req: { client: 'claude', mode: 'unified' } };
  const spies = spyBag(['fail']);
  const app = fragment({
    state: [],
    raw: [closureDecl('statusTimer'), closureDecl('previewSeq'), arrowSrc('loadPreviewNow')],
    fns: [],
    deps: {
      msgEl, writesEl, runBtn, modal,
      buildReq: () => selection.req,
      statusEl: () => modal.querySelector('#tkr-status'),
      setTimeout: (fn, ms) => { timers.push({ fn, ms }); return timers.length; },
      clearTimeout: (id) => { cleared.push(id); },
      ...apiRecorder(calls),
      tkPreviewWriteHTML: (write) => `<p class="code">${esc(write.path)}</p>`,
      esc, takeoverVariantLabel,
      client: 'claude', sel: { client: 'claude', mode: 'unified' }, variants: [], multi: false,
      scopeModel: true, allModels: ['m1'], modelSel: null, scopeMCP: false, hasMCP: false, allMcp: [], mcpSel: null,
      fail: spies.fns.fail,
      window: { innerHeight: 900 },
      seqGuard: spawner.seqGuard,
    },
    exposes: 'loadPreviewNow, get previewSeq() { return previewSeq; }',
  });
  return guardBooked(spawner, declaredGuards(TAKEOVER_PREVIEW_GUARDS, closureDecl),
    [(w) => w.app.loadPreviewNow()],
    { app, calls, spies: spies.calls, selection, writesEl, runBtn, summary, timers, cleared });
}

test('takeover preview: a response superseded by a newer selection lands nowhere', async () => {
  const w = takeoverPreviewWorld();
  const stale = w.app.loadPreviewNow();
  w.selection.req = { client: 'codex', mode: 'unified' }; // the user picks another client
  const latest = w.app.loadPreviewNow();
  assert.deepEqual(w.calls.map((c) => c.body), [
    { managed_only: true, client: 'claude', mode: 'unified' },
    { managed_only: true, client: 'codex', mode: 'unified' },
  ]);
  w.calls[1].resolve({ writes: [{ path: 'latest' }] });
  await latest;
  assert.equal(w.writesEl.innerHTML, '<p class="code">latest</p>');
  assert.equal(w.runBtn.disabled, false);
  const landedSummary = w.summary.innerHTML;
  assert.ok(landedSummary.includes('codex') === false && landedSummary.length > 0, 'the summary bar mirrors the selection');
  w.calls[0].resolve({ writes: [{ path: 'stale' }] });
  await stale;
  assert.equal(w.writesEl.innerHTML, '<p class="code">latest</p>', 'the superseded response must not replace the preview');
  assert.equal(w.summary.innerHTML, landedSummary, '…nor rewrite the summary bar');
});

test('takeover preview: a superseded failure fails nothing; the latest one is still reported', async () => {
  const w = takeoverPreviewWorld();
  const stale = w.app.loadPreviewNow();
  const latest = w.app.loadPreviewNow();
  w.calls[0].reject(new Error('stale boom'));
  await stale;
  assert.deepEqual(w.spies.fail, [], 'a superseded failure must not fail the dialog');
  w.calls[1].reject(new Error('daemon down'));
  await latest;
  assert.deepEqual(w.spies.fail, [['daemon down']], 'the latest failure is still reported');
});

test('takeover preview: clearing the selection retires the preview in flight (the token precedes the nothing-selected return)', async () => {
  const w = takeoverPreviewWorld();
  const inFlight = w.app.loadPreviewNow();
  w.selection.req = null; // Models and MCP both turned off
  await w.app.loadPreviewNow();
  assert.equal(w.calls.length, 1, 'nothing selected means no request');
  assert.equal(w.runBtn.disabled, true);
  w.calls[0].resolve({ writes: [{ path: 'late' }] });
  await inFlight;
  assert.equal(w.writesEl.innerHTML,
    '<span class="hint">nothing selected — turn Models or MCP back on above</span>',
    'the preview in flight must not land over the nothing-selected state');
  assert.equal(w.runBtn.disabled, true, '…nor re-enable Confirm');
});

// ---------------------------------------------------------------------------
// Wiring: a loader takes its sequence token through seqGuard — and only
// through seqGuard
// ---------------------------------------------------------------------------

// Every request guard this file characterizes — the loaders and the two
// request guards that are not loaders. The wiring contract below is stated
// over the whole of app.js, but it also insists that each seqGuard instance
// app.js declares belong to one of these worlds: a guard cannot exist here
// without a test that drives it.
const WORLDS = [keysWorld, analyticsWorld, accountsWorld, requestsWorld, securityWorld, mcpAnalyticsWorld, takeoverWorld,
  yamlLintWorld, takeoverPreviewWorld];

// Sequence counters that stay hand-rolled: id generators and epoch counters
// that are not latest-wins guards, plus the one request guard kept
// hand-rolled by decision (see its reason). Every entry needs a reason; an
// entry app.js no longer carries is reported as dead (the CLASS_EXEMPT
// discipline of registry.test.mjs).
const HANDROLLED_EXEMPT = {
  comboMenuSeq: 'id generator (combobox menu DOM ids) — not a guard',
  rawBodySeq: 'id generator (raw-body registry keys) — not a guard',
  bodyChunkSeq: 'id generator (chunked-body registry keys) — not a guard',
  liveEventSeq: 'id generator (synthetic keys of standalone event rows) — not a guard',
  tlTipSeq: 'epoch counter of the timeline tooltip: show() bumps it into a new epoch and hide() bumps it WITHOUT starting any request — invalidate-without-restart, an operation seqGuard does not have — and every excerpt fill of one epoch shares that epoch number',
  tkPreviewSeq: 'the guard of the takeover template-editor preview lives with its modal: the counter is parked on the modal dataset so a reopen can invalidate across functions — moving it onto seqGuard needs a decision on where that instance lives',
};

// IDENT is an identifier or a property path (`gen`, `state.n`,
// `modal.dataset.tkPreviewSeq`).
const IDENT = String.raw`[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*`;
// A token edge: not glued to the end of a longer name, `)` or `]`, and not
// preceded by a `+` (the `++` of `x++` must never read as a prefix).
const EDGE = String.raw`(?<![\w$)\]+])`;
const escapeRe = (text) => text.replace(/[.$]/g, '\\$&');

// handRolledCounters finds, by SHAPE rather than from a list of loaders,
// every sequence counter app.js still rolls by hand. No shape relies on a
// `Seq` suffix — a new loader may call its counter anything. A counter is
// anything advanced by `++x` / `x++` / `x += 1` / `x = x + 1` (property paths
// included), and where it lives decides what makes it a guard:
//   module-level variable   — the increment alone: a module-level counter
//                             exists to be compared against later;
//   behind a property       — only if something is compared against it: a
//                             property also carries plain cursors (a chunk
//                             reader's `state.index += 1`);
//   function-local          — only if a local is copied from it and later
//                             compared against it (`gen++; const seq = gen;
//                             … seq !== gen`) — copying state that is never
//                             counted (`const prev = activeTab`) is an
//                             identity check, not a sequence guard.
// On top of that: a token taken straight off an increment, and `…Seq` names
// declared at 0, parked on a DOM dataset, or compared by hand.
// Returns name -> the shapes that matched.
function handRolledCounters() {
  const found = new Map();
  const note = (name, shape) => {
    const last = name.split('.').pop();
    if (!found.has(last)) found.set(last, new Set());
    found.get(last).add(shape);
  };
  const escaped = (text) => escapeRe(text);
  const strictComparisonWith = (name) => new RegExp(
    String.raw`(?:(?<![\w$])${escaped(name)}\s*(?:===|!==)|(?:===|!==)\s*${escaped(name)}(?![\w$]))`);
  const moduleLevel = new Set(
    [...appJs.matchAll(/^(?:let|const|var)\s+([A-Za-z_$][\w$]*)/gm)].map((m) => m[1]));
  const increments = [
    new RegExp(String.raw`${EDGE}\+\+(${IDENT})`, 'g'),
    new RegExp(String.raw`(${IDENT})\+\+(?!\+)`, 'g'),
    new RegExp(String.raw`${EDGE}(${IDENT})\s*\+=\s*1\b`, 'g'),
    // `x = String((Number(x) || 0) + 1)` — a counter advanced by assignment.
    new RegExp(String.raw`${EDGE}(${IDENT})\s*=\s*(?:String\(\s*)?\(?\s*(?:Number\(\s*\1\s*\)|\1)(?:\s*\|\|\s*0)?\s*\)?\s*\+\s*1\b`, 'g'),
  ];
  const counted = new Set(); // full path of everything app.js advances
  for (const re of increments) for (const m of appJs.matchAll(re)) counted.add(m[1]);
  for (const name of counted) {
    if (!name.includes('.')) {
      if (moduleLevel.has(name)) note(name, 'module-level counter incremented by hand');
    } else if (strictComparisonWith(name).test(appJs)) {
      note(name, 'counter behind a property, incremented and compared by hand');
    }
  }
  for (const m of appJs.matchAll(new RegExp(String.raw`\bconst\s+([A-Za-z_$][\w$]*)\s*=\s*(${IDENT})\s*;`, 'g'))) {
    const [, local, counter] = m;
    if (!counted.has(counter)) continue;
    const compared = new RegExp(String.raw`(?:(?<![\w$])${escaped(local)}\s*(?:===|!==)\s*${escaped(counter)}(?![\w$])|(?<![\w$])${escaped(counter)}\s*(?:===|!==)\s*${escaped(local)}(?![\w$]))`);
    if (compared.test(appJs)) note(counter, 'copied into a local that is compared against it');
  }
  const token = new RegExp(String.raw`\b(?:const|let|var)\s+[A-Za-z_$][\w$]*\s*=\s*(?:\+\+(${IDENT})|(${IDENT})\+\+|\((${IDENT})\s*\+=\s*1\))\s*;`, 'g');
  for (const m of appJs.matchAll(token)) note(m[1] || m[2] || m[3], 'token taken off an increment');
  for (const m of appJs.matchAll(/\b(?:let|var)\s+([A-Za-z_$][\w$]*Seq)\s*=\s*0\b/g)) note(m[1], 'counter declared at 0');
  for (const m of appJs.matchAll(/\bdataset\.([A-Za-z_$][\w$]*Seq)\b/g)) note(m[1], 'counter parked on a DOM dataset');
  for (const m of appJs.matchAll(new RegExp(String.raw`(${IDENT})\s*(?:===|!==)\s*(${IDENT})`, 'g'))) {
    for (const side of [m[1], m[2]]) if (side.endsWith('Seq')) note(side, 'compared by hand');
  }
  return found;
}

// guardDeclarations names every `const|let NAME = seqGuard();` in app.js —
// module level or inside a closure.
function guardDeclarations() {
  return [...appJs.matchAll(/^[ \t]*(?:const|let)\s+([A-Za-z_$][\w$]*)\s*=\s*seqGuard\(\);/gm)].map((m) => m[1]);
}

// guardsReferenced names the guards a world's compiled sources actually call.
function guardsReferenced(w, guards) {
  return guards.filter((name) => w.app.sources.some((src) => new RegExp(`\\b${name}\\.(?:next\\(\\)|isLatest\\()`).test(src)));
}

// driveThroughGuard starts one guarded request twice with both in flight,
// then fails every request the loader issued — repeating while it issues
// more: the later load must land its failure through the guard and the
// earlier one must be dropped by it. The guard is thereby driven through
// next(), through a latest landing and through a superseded one.
async function driveThroughGuard(w, start) {
  const loads = [start(w), start(w)];
  for (let round = 0; round < 6; round++) {
    for (const c of w.calls) c.reject(new Error('driven'));
    await flush();
  }
  await Promise.all(loads);
}

test('wiring: no loader rolls its own sequence counter — every guard is a seqGuard, used as one, and characterized here', async () => {
  // 1. Shape: nothing takes a latest-wins token by hand unless it is exempt
  //    above, with a reason — whatever a new loader calls its counter.
  const rolled = handRolledCounters();
  const unexempted = [...rolled].filter(([name]) => !Object.hasOwn(HANDROLLED_EXEMPT, name));
  assert.deepEqual(unexempted.map(([name, shapes]) => `${name} (${[...shapes].join(', ')})`), [],
    'hand-rolled sequence counter(s) in app.js — take the token through pure.js seqGuard, or exempt a non-loader in HANDROLLED_EXEMPT with a reason');
  const dead = Object.keys(HANDROLLED_EXEMPT).filter((name) => !rolled.has(name));
  assert.deepEqual(dead, [], `HANDROLLED_EXEMPT entries app.js no longer carries — remove them: ${dead.join(', ')}`);

  // 2. Use and coverage, by execution rather than by text. Every guard
  //    declared in app.js must belong to a world here, a world's list of
  //    guards must be exactly the guards its compiled code calls, and
  //    driving the world must actually run each of them: tokens handed out,
  //    a latest landing settled, a superseded one dropped.
  const guards = guardDeclarations();
  assert.ok(guards.length >= WORLDS.length,
    `app.js holds only ${guards.length} seqGuard instance(s) — the guards characterized here are gone`);
  const pinned = new Set();
  for (const build of WORLDS) {
    const w = build();
    const claimed = [...new Set(w.guardNames)].sort();
    assert.ok(claimed.length >= 1, `${build.name} characterizes no guard`);
    assert.deepEqual(claimed, guardsReferenced(w, guards).sort(),
      `${build.name}: the guards it lists must be exactly the guards its compiled code calls`);
    if (w.prepare) await w.prepare(w);
    for (const start of w.starts) await driveThroughGuard(w, start);
    for (const name of claimed) {
      pinned.add(name);
      const instance = w.app[name];
      assert.ok(w.guards.includes(instance), `${name} must be an instance the shared seqGuard built`);
      const used = w.spawner.usageOf(instance);
      assert.ok(used.next >= 2, `${name}: no test drives two loads through it (next() called ${used.next}x)`);
      assert.ok(used.latest >= 1, `${name}: no load ever landed as the latest one through it`);
      assert.ok(used.stale >= 1, `${name}: no superseded load was ever dropped by it`);
    }
  }
  const unpinned = guards.filter((name) => !pinned.has(name));
  assert.deepEqual(unpinned, [],
    `seqGuard instance(s) in app.js that no world in this file characterizes: ${unpinned.join(', ')} — a guard without a test is a refactor waiting to happen`);
});

test('F15 wiring: the create-key in-flight flag is module state', () => {
  assert.equal(keysWorld().app.keysCreating, false, 'app.js must declare keysCreating');
});
