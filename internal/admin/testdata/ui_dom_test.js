// Headless-free DOM smoke test: run the dashboard's JS against a fake DOM and
// a fake fetch, asserting it renders the tanks, the exact-figures table and the
// request log, and that the poll tick re-fetches (the frozen-dashboard bug).
//
// Usage: node ui_dom_test.js <path-to-index.html>
const fs = require('fs');
const html = fs.readFileSync(process.argv[2], 'utf8');
const js = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m => m[1]).join('\n');
const css = [...html.matchAll(/<style>([\s\S]*?)<\/style>/g)].map(m => m[1]).join('\n');

// The wave loops only if the mask's repeat period and the drift keyframe's
// end offset are the same number. Nothing else in the page couples them, and
// a mismatch is invisible in a screenshot (headless virtual time does not
// advance CSS animations) — so assert it statically.
const tile = (css.match(/mask-size:\s*(\d+)px/) || [])[1];
// Require a delimiter before the property, or this also matches the
// -webkit-mask-position value and the check silently passes on a mismatch.
const shift = (css.match(/to\s*{[^}]*?[;{\s]mask-position:\s*(\d+)px/) || [])[1];

const now = Date.now();
// Issue #209: /api/accounts is now sorted server-side by (priority, name),
// and the dashboard must render tanks (and the figures table built from the
// same payload) in whatever order it is given. Declared here in NEITHER
// alphabetical NOR the accounts' own priority order -- three, one, two --
// so a test asserting the tanks follow the payload is actually exercising
// that, rather than passing vacuously because the fixture already reads
// top-to-bottom.
const ACCOUNTS = [{
  // Issue #194: a family-scoped 429 forges a 100%-used row so the exclusion
  // is visible. It was never measured, so the provenance column must say so
  // rather than "measured".
  name: 'you@example-three.com', type: 'claude-oauth', source: 'yaml', state: 'ok', inFlight: 0,
  quotaWindows: [
    { name: '7d-fable', limit: 1, used: 1, resetAt: new Date(now + 6 * 3600e3).toISOString(), source: 'rejected' },
  ],
}, {
  name: 'you@example-one.com', label: 'work', type: 'claude-oauth', source: 'yaml', state: 'ok', inFlight: 1,
  quotaWindows: [{ name: '5h', limit: 1, used: 0.42, resetAt: new Date(now + 3600e3).toISOString(), source: 'headers' }],
  // Issue #110: cache hit rate and create/read volume, beside burn/h and
  // dry-in in the same figures row.
  cacheHitRate: 0.304, cacheCreateTokens: 4165, cacheReadTokens: 1816,
}, {
  name: 'you@example-two.com', type: 'claude-oauth', source: 'yaml', state: 'ok', inFlight: 0,
  quotaWindows: [
    { name: '5h', limit: 1, used: 0.20, resetAt: new Date(now + 7200e3).toISOString(), source: 'poll' },
    // Fully spent, refilling in two hours: the dry-tank countdown case.
    { name: '7d', limit: 1, used: 1, resetAt: new Date(now + 7200e3).toISOString(), source: 'poll' },
    // Spent, but its reset passed an hour ago with nothing re-measuring it
    // (issue #135): must read as unknown, never as 0% with "refills 0s".
    { name: '7d-fable', limit: 1, used: 1, resetAt: new Date(now - 3600e3).toISOString(), source: 'headers', expired: true },
  ],
}];
// Issue #167's actual shape: /api/quota-history returns a series per account
// PER WINDOW, so eight accounts is twenty-four series, not eight. The three
// accounts that also appear in ACCOUNTS keep shapes the tank and burn-rate
// assertions depend on -- in particular example-one's 5h curve is the one the
// "no window runs dry" verdict is computed from, and example-two's spent 7d
// is deliberately FLAT so it raises no dry-window alarm. The other five exist
// only in the history, which is what the chart gets for an account retired
// from the pool whose samples have not aged out yet.
//
// Their domains differ in the first few characters on purpose: short() falls
// back to the domain's first label, and three accounts all reading
// "example-..." is a fixture artefact, not what a real pool looks like.
const H_TS = [now - 3600e3, now - 1800e3, now - 60e3];
const HIST_ACCOUNTS = [
  'you@example-one.com', 'you@example-two.com', 'you@example-three.com',
  'you@acme.com', 'you@initech.com', 'you@hooli.io', 'you@globex.net', 'you@umbrella.org',
];
const H_5H = [[0.75, 0.62, 0.58], [0.86, 0.83, 0.80], [0.55, 0.52, 0.49], [0.91, 0.88, 0.84],
              [0.40, 0.36, 0.31], [0.70, 0.66, 0.61], [0.25, 0.21, 0.18], [0.60, 0.57, 0.55]];
const H_7D = [[0.44, 0.43, 0.42], [0, 0, 0], [0.66, 0.65, 0.64], [0.80, 0.79, 0.78],
              [0.30, 0.29, 0.28], [0.52, 0.51, 0.50], [0.15, 0.14, 0.13], [0.90, 0.89, 0.88]];
// Every fable window spent, which is the state the issue describes for 5h:
// eight lines flat along the floor, a shape and not a reading.
const H_FABLE = HIST_ACCOUNTS.map(() => [0, 0, 0]);
const HISTORY = [
  ...HIST_ACCOUNTS.map((account, i) => ({ account, window: '5h', ts: H_TS, headroom: H_5H[i] })),
  ...HIST_ACCOUNTS.map((account, i) => ({ account, window: '7d', ts: H_TS, headroom: H_7D[i] })),
  ...HIST_ACCOUNTS.map((account, i) => ({ account, window: '7d-fable', ts: H_TS, headroom: H_FABLE[i] })),
];
const ACTIVITY = [{ ts: now - 60e3, count: 3, errors: 0, rotated: 0, p95_ms: 800 }];
const REQUESTS = [
  { ts: new Date(now).toISOString(), account: 'you@example-one.com', path: '/v1/messages',
    status: 200, duration_ms: 12, bytes: 34, event: 'served',
    model_asked: 'claude-sonnet-4-6', model_served: 'claude-sonnet-4-6' },
  // Cross-provider rewrite: the client asked for one model and another served.
  { ts: new Date(now).toISOString(), account: 'kimi', path: '/v1/messages',
    status: 200, duration_ms: 40, bytes: 90, event: 'served',
    model_asked: 'claude-sonnet-4-6', model_served: 'k3' },
];

const SETTINGS = {
  exhaustedMode: 'notify', holdMax: '4h', switchThreshold: '0.98',
  probeOnStart: true, probeInterval: '30m', crossProvider: false,
  accounts: { 'you@example-one.com': { label: 'work', disabled: false } },
};
const fetchCount = { accounts: 0, requests: 0, history: 0, activity: 0, settings: 0, state: 0 };
// settingsPuts records every PUT /api/settings body the dashboard sent, so a
// test can assert what was written and how often. Separate from
// fetchCount.settings, which stays a count of GETs.
const settingsPuts = [];
// probeCalls records what the "check now" control (#192) sent to
// POST /api/accounts/probe, so a test can assert both that a refusal was NOT
// silently forced and that a confirmed retry actually carried force:true.
const probeCalls = [];
// pinState is /api/state's view of the pin (#11) -- the ONLY thing the pin
// tests below mutate directly to simulate another client (the CLI
// `spillway switch`) changing it out from under the dashboard. pinCalls
// records what the dashboard itself sent to POST/DELETE /api/pin.
let pinState = { pinned: "" };
const pinCalls = [];
// ── fixture routes ──────────────────────────────────────────────────────
// Keyed on METHOD and the exact path, never a substring (#202). Two defects
// of that shape have already shipped here: /api/settings matched by
// substring with no method branch, so a PUT was answered with the read
// fixture and a write that never happened looked like it worked; and
// /api/accounts matched by substring, which would have swallowed the
// /api/accounts/probe POST (#192) and made that control look functional
// too. A path that exists but does not answer this method is a hard error,
// not a fallback — a lenient fixture is how the page under test gets to be
// wrong and pass.
const ok200 = (body) => ({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });
const ROUTES = {
  'GET /api/accounts':      () => { fetchCount.accounts++; return ok200(ACCOUNTS); },
  'GET /api/quota-history': () => { fetchCount.history++;  return ok200(HISTORY); },
  'GET /api/activity':      () => { fetchCount.activity++; return ok200(ACTIVITY); },
  'GET /api/requests':      () => { fetchCount.requests++; return ok200(REQUESTS); },
  'GET /api/state':         () => { fetchCount.state++;    return ok200(pinState); },
  'GET /api/settings':      () => { fetchCount.settings++; return ok200(SETTINGS); },

  'PUT /api/settings': (opts) => {
    // The Authorization header is recorded too — a write sent with auth()
    // applied to the URL instead of the init is unauthenticated and
    // invisible on a loopback dashboard, which has no token at all.
    const body = JSON.parse(opts.body);
    settingsPuts.push({ body, auth: (opts.headers || {}).Authorization || '' });
    // Mirrors the server: config.Settings' pointer fields mean only the keys
    // actually named are applied, and everything else is left as it was. So
    // a partial body here must leave the rest of SETTINGS alone.
    Object.assign(SETTINGS, body);
    return { ok: true, status: 200, text: async () => JSON.stringify(SETTINGS), json: async () => SETTINGS };
  },

  'POST /api/accounts/probe': (opts) => {
    const req = JSON.parse(opts.body);
    // The Authorization header is recorded, not just the body: the pin
    // control shipped once with auth() applied to the URL instead of the
    // init, which left the request unauthenticated and was invisible on a
    // loopback dashboard because it has no token at all.
    probeCalls.push({ name: req.name, force: req.force,
                      auth: (opts.headers || {}).Authorization || '' });
    // Fixture behaviour keyed by account, mirroring the pin fixture:
    //   example-one -> 502 (probe attempted, did not complete: no retry)
    //   example-two -> 409 unless forced (would be charged: retry offered)
    if (req.name === 'you@example-one.com') {
      return { ok: false, status: 502, text: async () => 'spillway: probe of "you@example-one.com" failed: upstream timeout' };
    }
    if (req.name === 'you@example-two.com' && !req.force) {
      return { ok: false, status: 409, text: async () =>
        'spillway: probing there would spend money: "you@example-two.com" is out of quota and has extra usage permitted, so this probe is a charged request' };
    }
    const body = { account: req.name, billed: !!req.force,
                   quotaWindows: [{ name: '5h', limit: 1, used: 0.07 }, { name: '7d', limit: 1, used: 0 }] };
    return { ok: true, status: 200, text: async () => JSON.stringify(body), json: async () => body };
  },

  'POST /api/pin': (opts) => {
    const req = JSON.parse(opts.body);
    pinCalls.push({ method: 'POST', account: req.account, force: req.force });
    // Fixed fixture behaviour, keyed by account, so the test can exercise
    // every documented outcome without a real backend:
    //   example-one -> always 400 (malformed/unknown account: no retry)
    //   example-two -> 409 unless forced (would bill: retry offered)
    if (req.account === 'you@example-one.com') {
      return { ok: false, status: 400, text: async () => 'spillway: malformed body' };
    }
    if (req.account === 'you@example-two.com' && !req.force) {
      return { ok: false, status: 409, text: async () =>
        'spillway: pinning there would spend money: "you@example-two.com" is out of quota and would serve from paid extra usage' };
    }
    pinState = { pinned: req.account };
    const body = { pinned: req.account, warning: 'prompt cache is per account, so the next request will miss it' };
    return { ok: true, status: 200, text: async () => JSON.stringify(body), json: async () => body };
  },

  'DELETE /api/pin': () => {
    pinCalls.push({ method: 'DELETE' });
    pinState = { pinned: "" };
    return { ok: true, status: 200, text: async () => '{}', json: async () => ({}) };
  },
};
global.fetch = async (u, opts) => {
  const path = String(u).split('?')[0].split('#')[0];
  const method = String((opts && opts.method) || 'GET').toUpperCase();
  const h = ROUTES[method + ' ' + path];
  if (h) return h(opts || {});
  const otherMethods = Object.keys(ROUTES)
    .filter(k => k.slice(k.indexOf(' ') + 1) === path)
    .map(k => k.slice(0, k.indexOf(' ')));
  if (otherMethods.length) {
    throw new Error('fixture: ' + method + ' ' + path + ' has no route; this path answers only ' +
      otherMethods.join('/') + '. A write must never be answered by a read (#202).');
  }
  throw new Error('fixture: no route for ' + method + ' ' + path +
    ' — the page fetched something this harness does not model.');
};
global.EventSource = class { constructor() { this.onmessage = this.onopen = this.onerror = null; } };

let intervals = 0, pollFn = null;
global.setInterval = (fn) => { intervals++; pollFn = fn; return 1; };

const els = {};
const waves = [];

// Minimal class/tag matcher: enough for the '.body' / '.wave' / '.depth'
// lookups the dashboard does when updating a tank in place.
//
// Checks the class ATTRIBUTE as well as the property. An SVG element's
// className is a read-only SVGAnimatedString in a real browser, so the
// dashboard sets SVG classes with setAttribute — and a matcher that only
// looked at the property silently found none of them.
function matches(el, sel) {
  // Attribute selectors, for collectSettings' document.querySelectorAll(
  // "[data-account]"). Without this the account rows were unreachable and
  // every per-account setting went uncollected — see the querySelectorAll
  // comment on `document` below (#202).
  if (sel.startsWith('[') && sel.endsWith(']')) {
    const name = sel.slice(1, -1);
    const dm = /^data-(.+)$/.exec(name);
    if (dm) {
      const key = dm[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase());
      return !!el.dataset && el.dataset[key] !== undefined;
    }
    return !!el.attrs && el.attrs[name] !== undefined;
  }
  // Tag selectors as well as classes. Without this findAllIn(root, 'input')
  // returned [] for every call, so the assertion that extra usage is NOT an
  // editable input passed while testing nothing at all.
  if (!sel.startsWith('.')) return String(el.tagName || '').toLowerCase() === sel.toLowerCase();
  const want = sel.slice(1);
  const has = (v) => String(v || '').split(/\s+/).includes(want);
  return has(el.className) || has(el.attrs && el.attrs.class);
}
function findAllIn(root, sel, out = []) {
  for (const c of root.children) {
    if (matches(c, sel)) out.push(c);
    findAllIn(c, sel, out);
  }
  return out;
}
function findIn(root, sel) { return findAllIn(root, sel)[0] || null; }
// This fake DOM keeps no parent pointers, so the only way to get from a
// tank's pin button to that SAME tank's message box is to search card by
// card rather than the button up. Matched by title text rather than
// position -- there is no layout engine here, so "second card" is not a
// safe way to mean "the account.com one".
function findCardByBtnTitle(accounts, re) {
  for (const card of accounts.children) {
    const btn = findIn(card, '.pin-btn');
    if (btn && re.test(btn.title)) {
      return { card, btn, msg: findIn(card, '.pin-msg'), probe: findIn(card, '.probe-btn') };
    }
  }
  return null;
}
function mkEl(tag) {
  const el = {
    tagName: String(tag || 'div').toUpperCase(),
    _html: '', _text: '', hidden: false, children: [], value: '',
    dataset: {}, className: '', title: '', attrs: {},
    style: { _p: {}, setProperty(k, v) { this._p[k] = v; }, getPropertyValue(k) { return this._p[k]; } },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    getAttribute(k) { return this.attrs[k]; },
    // Recorded, not fired: this fake DOM has no event loop of its own, so a
    // click is simulated by the test calling el._on.click() directly. Only
    // the last listener per type is kept -- every element in this page
    // registers at most one.
    addEventListener(ev, fn) { this._on = this._on || {}; this._on[ev] = fn; },
    appendChild(c) {
      this.children.push(c);
      if (String(c.className).startsWith('wave ')) waves.push(c);
      return c;
    },
    prepend(c) { this.children.unshift(c); },
    // reorderTanks (#209) moves an existing card within its current parent
    // (#accounts) rather than rebuilding it, so this needs to actually
    // relocate the node -- unlike appendChild above, which never has to
    // handle a node that's already one of this.children.
    insertBefore(node, ref) {
      const at = this.children.indexOf(node);
      if (at !== -1) this.children.splice(at, 1);
      const ri = ref == null ? -1 : this.children.indexOf(ref);
      this.children.splice(ri === -1 ? this.children.length : ri, 0, node);
      return node;
    },
    querySelector(sel) { return findIn(this, sel); },
    setAttribute2() {},
    querySelectorAll(sel) { return findAllIn(this, sel); },
    get _isWave() { return String(this.className).startsWith('wave'); },
    focus() {}, remove() {},
  };
  // classList, backed by className so assertions can read either. The dry-tank
  // countdown toggles a class rather than rebuilding, which is the pattern the
  // wave animations forced on everything here.
  el.classList = {
    add: (c) => { if (!String(el.className).split(/\s+/).includes(c)) el.className = (el.className ? el.className + ' ' : '') + c; },
    remove: (c) => { el.className = String(el.className).split(/\s+/).filter(x => x && x !== c).join(' '); },
    contains: (c) => String(el.className).split(/\s+/).includes(c),
    toggle: (c, on) => (on ? el.classList.add(c) : el.classList.remove(c)),
  };
  // Live, not a snapshot: the dashboard appends empty elements and fills
  // their text afterwards, so an append-time copy misses everything.
  Object.defineProperty(el, 'innerHTML', {
    get() { return this._html + this.children.map(c => c.innerHTML).join(''); },
    set(v) { this._html = v; this.children.length = 0; },
  });
  // An element the page gave an id to must be findable by that id. Without
  // this, document.getElementById auto-created a fresh empty element for
  // every "set-<key>" the settings panel had just built, so collectSettings
  // read phantoms: a Save sent exhaustedMode:"" and no switchThreshold at
  // all, and every assertion about what the form submits passed against a
  // body the dashboard never produces.
  Object.defineProperty(el, 'id', {
    get() { return this._id || ''; },
    set(v) { this._id = String(v); els[this._id] = this; },
  });
  // esc() relies on textContent -> innerHTML escaping; emulate it.
  Object.defineProperty(el, 'textContent', {
    get() { return this._text; },
    set(v) {
      this._text = String(v);
      this._html = String(v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
      this.children.length = 0;
    },
  });
  return el;
}
// Every id index.html's own markup declares, seeded as an empty element —
// that is what a browser hands the page on load. Anything else must have been
// given an id by the page at runtime (the "set-<key>" and "park-<name>"
// controls, through the id setter above). A lookup that is neither is a test,
// or a page, asking for something that does not exist, and inventing an
// element for it is how collectSettings came to read phantoms and submit a
// body the real server would 400 (#202).
// Seeded with the TAG and the CLASS the markup gave them, not just the id. A
// class-less, tag-less seed is the same family of lie as the phantom
// getElementById (#202): index.html's own stylesheet targets those classes,
// so a test could not ask which CSS rule governs an element the markup
// declares — and the layout rules are the whole of #167's mobile reflow.
const MARKUP_IDS = [];
for (const m of html.replace(/<script>[\s\S]*?<\/script>/g, '')
    .matchAll(/<([a-z][a-z0-9]*)\b([^>]*)>/gi)) {
  const id = /\bid="([^"]+)"/.exec(m[2]);
  if (!id) continue;
  const e = mkEl(m[1]);
  e.id = id[1];
  const cls = /\bclass="([^"]*)"/.exec(m[2]);
  if (cls) e.className = cls[1];
  MARKUP_IDS.push(id[1]);
}
if (!MARKUP_IDS.length) throw new Error('harness: index.html declares no ids — the id scrape is broken');

global.document = {
  getElementById: (id) => {
    const el = els[id];
    if (!el) {
      throw new Error('getElementById(' + JSON.stringify(id) + '): no such element. ' +
        'index.html does not declare it and the page never created it. ' +
        'The harness will not invent one (#202).');
    }
    return el;
  },
  // Searches every root the markup declares, rather than returning nothing.
  // The stub used to be `() => []`, which is the same lie as the phantom
  // getElementById and hid the same kind of defect: collectSettings reaches
  // the label, priority and park controls ONLY through
  // querySelectorAll("[data-account]"), so every per-account setting was
  // silently dropped from every Save and no assertion could see it (#202).
  querySelectorAll: (sel) => {
    const out = [];
    for (const id of MARKUP_IDS) findAllIn(els[id], sel, out);
    return out;
  },
  createElement: (tag) => mkEl(tag),
  createElementNS: (_ns, tag) => mkEl(tag),
  addEventListener: (ev, fn) => { if (ev === 'DOMContentLoaded') fn(); },
};
global.location = { search: '?token=T', href: '' };
global.sessionStorage = { getItem: () => 'T', setItem() {}, removeItem() {} };
global.window = global;

// The page declares "use strict", so its declarations stay inside the eval's
// own scope and nothing here can name them. Everything below reaches the
// dashboard through the DOM or through a callback it handed out (pollFn via
// setInterval) — except buildSettings, which a test re-runs with a payload
// the fixture cannot express. Published explicitly rather than by relaxing
// the page's own strictness.
eval(js + '\n;global.__page = { buildSettings };');

// Results live out here, and every exit path prints them. A throw used to
// take the whole run with it, so a missing element crashed the harness
// instead of failing it (#168) — and now that an unknown id throws by design
// (#202), that mattered more than ever. An abort is reported as a named
// failure with whatever was already established still printed.
const ok = {};
function report() {
  let fail = 0;
  for (const [k, v] of Object.entries(ok)) { console.log((v ? 'PASS' : 'FAIL') + ': ' + k); if (!v) fail++; }
  console.log('fetches:', JSON.stringify(fetchCount));
  process.exit(fail ? 1 : 0);
}
function abort(what, err) {
  console.log('--- ' + what + ' ---\n' + ((err && err.stack) || err));
  ok['the harness ran to completion'] = false;
  report();
}
// The page's controls are fire-and-forget: a click handler returns a promise
// nobody awaits, so a rejection inside one would otherwise kill node with no
// assertion output at all.
process.on('unhandledRejection', (err) => abort('unhandled rejection', err));

async function run() {
  // start() is invoked by the stubbed DOMContentLoaded during eval; give its
  // async fetch chain time to settle.
  await new Promise(r => setTimeout(r, 200));

  const tanks = els['accounts'] ? els['accounts'].innerHTML : '';
  const figures = els['figures'] ? els['figures'].innerHTML : '';
  const reqs = els['requests'] ? els['requests'].innerHTML : '';
  const burn = els['burn'] ? els['burn'].textContent : '';

  Object.assign(ok, {
    'tank shows configured label': tanks.includes('work'),
    // Issue #135: an expired window says so in the figures, and never
    // fabricates a zero-second refill countdown.
    'expired window says expired, not a countdown': figures.includes('expired') && !figures.includes('0s'),
    // Falls back to the domain, never the local part: both fixtures share
    // local part, so a local-part fallback would render them identically.
    'unlabelled tank falls back to domain': tanks.includes('example-two'),
    'full address shown in small print': tanks.includes('you@example-one.com'),
    // Gauges show HEADROOM (remaining), not usage: 0.42 used -> 58%.
    'gauge shows headroom (58%)': tanks.includes('58'),
    'figures table lists the window': figures.includes('58%') && figures.includes('5h'),
    // §6.5: a polled reading is up to a minute stale and a measured one is
    // exact; showing both as a bare percentage implies equal confidence.
    'quota source shown per window': figures.includes('measured') && figures.includes('polled'),
    // Issue #194: the forged row a window rejection writes must read
    // "rejected", never "measured" — nothing measured it, and the dashboard
    // is where a user decides whether the exclusion is right.
    'forged rejection row not shown as measured': figures.includes('rejected'),
    // Issue #110: cache hit rate (30.4%) and create/read volume (4165/1816)
    // rendered beside burn/h and dry-in.
    'cache hit rate shown': figures.includes('30.4%'),
    'cache create/read volume shown': figures.includes('4165 / 1816'),
    'burn-rate verdict stated': burn.length > 10,
    'requests rendered': reqs.includes('/v1/messages'),
    'settings panel fetched': fetchCount.settings > 0,
    'settings panel rendered': (els['settings'] ? els['settings'].innerHTML : '').length > 0,
    'served model shown': reqs.includes('sonnet-4-6'),
    // The rewritten one must show what ACTUALLY served, not what was asked.
    'rewritten model shows served name': reqs.includes('k3'),
    'rewritten model is flagged': reqs.includes('swap'),
    'polling armed': intervals > 0,
    'history + activity fetched': fetchCount.history > 0 && fetchCount.activity > 0,
    // Every tank sharing one timeline made all six surfaces heave in step.
    'wave layers created': waves.length >= 6,
    'mask tile matches drift distance': !!tile && tile === shift,
    // Assert the property CSS actually consumes. The previous version checked
    // a custom property that a higher-specificity `animation:` shorthand was
    // silently resetting, so it passed while every tank started in step.
    'tanks do not animate in sync': (() => {
      const delays = waves.map(w => w.style.animationDelay).filter(Boolean);
      const rates = new Set(waves.map(w => w.style.getPropertyValue('--rate')));
      return delays.length === waves.length && new Set(delays).size === waves.length && rates.size > 1;
    })(),
    'phases spread across the cycle': (() => {
      const secs = waves.map(w => Math.abs(parseFloat(w.style.animationDelay || '0')));
      return secs.length > 1 && (Math.max(...secs) - Math.min(...secs)) > 6;
    })(),
  });

  // ── controls sit in their own row, not the name/flag row (#210) ──────
  // A wrapping .tank-head let the pin/probe pair drop onto a line of their
  // own whenever a long name plus the serving flag overran the card, so the
  // header is now two deterministic rows: .tank-head (swatch/name/flag) and
  // .tank-controls (pin/probe). There is no layout engine here, so the only
  // checkable property is structural, not "it doesn't wrap": the buttons are
  // not descendants of .tank-head, and .tank-controls holds exactly one of
  // each, on every card the fixture renders -- not just the one this test
  // happens to look at, or a defect on a single card would pass unseen.
  const cardFor = (acct) => els.accounts.children.find(c => c.dataset.acct === acct);
  ok['pin and probe buttons are not inside .tank-head, on every card'] =
    els.accounts.children.length === ACCOUNTS.length &&
    els.accounts.children.every(c => {
      const head = findIn(c, '.tank-head');
      return !!head && findAllIn(head, '.pin-btn').length === 0 &&
        findAllIn(head, '.probe-btn').length === 0;
    });
  ok['pin and probe buttons live together in their own controls row, on every card'] =
    els.accounts.children.length === ACCOUNTS.length &&
    els.accounts.children.every(c => {
      const controls = findIn(c, '.tank-controls');
      return !!controls && findAllIn(controls, '.pin-btn').length === 1 &&
        findAllIn(controls, '.probe-btn').length === 1;
    });
  // example-one has inFlight > 0 (flag "serving"); example-two/three sit at
  // inFlight 0 (flag "standby") -- the controls row must not depend on which.
  const servingCard = cardFor('you@example-one.com');
  const standbyCard = cardFor('you@example-two.com');
  ok['controls row present when the serving flag is set'] =
    !!servingCard && findIn(servingCard, '.flag').textContent === 'serving' &&
    !!findIn(servingCard, '.tank-controls');
  ok['controls row present when the serving flag is not set'] =
    !!standbyCard && findIn(standbyCard, '.flag').textContent !== 'serving' &&
    !!findIn(standbyCard, '.tank-controls');
  // And on the shortest displayed name in the fixture ("work", example-one's
  // label) -- so this isn't only proven against a name long enough to need
  // row 1's ellipsis.
  ok['controls row present on a card with a short name'] =
    !!servingCard && findIn(servingCard, '.tank-name').textContent === 'work' &&
    !!findIn(servingCard, '.tank-controls');

  // ── tank order follows the payload (issue #209) ──────────────────────
  // /api/accounts is sorted server-side by (priority, name); the dashboard
  // just has to render whatever order it is given, for both the tanks and
  // the figures table. domOrder() reads dataset.acct back off the live
  // #accounts children, since this fake DOM keeps no separate index of
  // "current card order" for the page to have gotten wrong.
  const domOrder = () => els.accounts.children.map(c => c.dataset.acct);

  ok['tanks render in the order the payload was delivered in, not sorted client-side'] =
    JSON.stringify(domOrder()) === JSON.stringify(ACCOUNTS.map(a => a.name));

  // ensureCard() early-returns for a name it has already built, so the bug
  // this issue describes is specifically about EXISTING cards: a priority
  // change reorders /api/accounts on the next poll, and the tanks must
  // follow even though every card in play was created on an earlier poll,
  // not this one. Reversing the fixture's own order guarantees the new
  // order differs from both the original declaration and every prior
  // scramble, so this cannot pass by the new order accidentally matching
  // the old one.
  const orderBeforeReorder = domOrder();
  const wavesBeforeReorder = waves.length;
  ACCOUNTS.reverse();
  if (pollFn) { await pollFn(); await new Promise(r => setTimeout(r, 150)); }
  ok['a later poll with a changed priority reorders the existing cards'] =
    JSON.stringify(domOrder()) === JSON.stringify(ACCOUNTS.map(a => a.name)) &&
    JSON.stringify(domOrder()) !== JSON.stringify(orderBeforeReorder);
  // Moved, not rebuilt: a reorder that went through ensureCard/appendChild
  // instead of relocating the existing node would tear down and recreate
  // the wave elements, which is exactly the twitch #209 calls out.
  ok['reordering existing cards does not recreate their wave elements'] =
    waves.length === wavesBeforeReorder;

  // The converse: a poll whose order has NOT changed must not touch
  // #accounts at all, because appendChild/insertBefore detach-and-reattach
  // even a node that is already in the right place, which restarts that
  // tank's wave/bubble animation. Spy on the container's own methods
  // (not the page's) so a card's internal updates -- filling in text,
  // building a window's glass -- can't be mistaken for a reorder.
  const accountsEl = els.accounts;
  const realAppend = accountsEl.appendChild.bind(accountsEl);
  const realInsert = accountsEl.insertBefore.bind(accountsEl);
  let accountsTouched = 0;
  accountsEl.appendChild = (c) => { accountsTouched++; return realAppend(c); };
  accountsEl.insertBefore = (c, r) => { accountsTouched++; return realInsert(c, r); };
  if (pollFn) { await pollFn(); await new Promise(r => setTimeout(r, 150)); }
  accountsEl.appendChild = realAppend;
  accountsEl.insertBefore = realInsert;
  ok['a poll with unchanged order does not move any card'] = accountsTouched === 0;

  // ── headroom small multiples (#167) ──────────────────────────────────
  // Eight accounts x three windows was twenty-four lines on one 760x190 plot.
  // Everything below is anchored on the facets actually being there: a count
  // or an .every() over an empty container passes while testing nothing
  // (#202), and this whole section would be exactly that if the chart were
  // absent.
  const facets = findAllIn(els.headrooms, '.facet');
  const plots = findAllIn(els.headrooms, '.plot');
  const plotIn = (f) => findIn(f, '.plot');
  const facetWin = (f) => { const p = plotIn(f); return p && p.getAttribute('data-window'); };
  const seriesIn = (f) => findAllIn(f, '.series');

  ok['headroom is split into one plot per quota window'] =
    facets.length === 3 && plots.length === 3 &&
    JSON.stringify(facets.map(facetWin)) === JSON.stringify(['5h', '7d', '7d-fable']);
  // The split has to be a split. Every line in a plot belongs to that plot's
  // window, and each plot carries one per account.
  ok['each plot draws only its own window, one line per account'] =
    facets.length === 3 && facets.every(f => {
      const w = facetWin(f), s = seriesIn(f);
      return s.length === 8 && s.every(p => p.getAttribute('data-window') === w) &&
        new Set(s.map(p => p.getAttribute('data-account'))).size === 8;
    });
  ok['all 24 series are drawn, three ways rather than overlaid'] =
    plots.length === 3 && findAllIn(els.headrooms, '.series').length === 24;

  // The property the whole split rests on: colour follows the account, never
  // its rank inside a plot -- otherwise the three cannot be read together,
  // which is the only reason to have split them. Both halves: one account is
  // ONE hue in all three, AND two accounts are not the same hue (without
  // which "all identical" would pass on a chart painted in a single colour).
  const strokeOf = (f, acct) => {
    const p = seriesIn(f).find(s => s.getAttribute('data-account') === acct);
    return p && p.getAttribute('stroke');
  };
  const twoHues = facets.map(f => strokeOf(f, 'you@example-two.com'));
  const acmeHues = facets.map(f => strokeOf(f, 'you@acme.com'));
  const isSlot = (c) => /^var\(--series-\d+\)$/.test(String(c));
  ok['one account keeps one colour across all three plots'] =
    twoHues.length === 3 && twoHues.every(c => c && c === twoHues[0]) && isSlot(twoHues[0]);
  ok['two accounts do not share a colour'] =
    acmeHues.length === 3 && acmeHues.every(c => c && c === acmeHues[0]) &&
    isSlot(acmeHues[0]) && acmeHues[0] !== twoHues[0];

  // The ask was the same space, not three times it. A facet renders at a
  // third of the row's width, so its height is width/3 x (vbH/vbW); the row
  // is no taller than the old single plot iff vbH/vbW <= 3 x (190/760). The
  // column gaps and the caption line come out of the slack that leaves.
  const ratioOf = (f) => {
    const p = plotIn(f);
    const n = String((p && p.getAttribute('viewBox')) || '').trim().split(/\s+/).map(Number);
    return (n.length === 4 && n[2] > 0 && n[3] > 0) ? n[3] / n[2] : null;
  };
  const ratios = facets.map(ratioOf);
  ok['the three plots stand no taller than the one they replace'] =
    ratios.length === 3 && ratios.every(r => r !== null && r === ratios[0]) &&
    ratios[0] <= 3 * (190 / 760);

  // The reflow is a CSS rule, so assert the rule -- tied to the class the
  // container actually carries, so renaming one without the other fails here
  // instead of quietly leaving a three-column grid on a phone.
  const plotsCls = String(els.headrooms.className || els.headrooms.attrs.class || '').trim();
  const wideRule = new RegExp('\\.' + plotsCls +
    '\\s*\\{[^}]*grid-template-columns:\\s*repeat\\(3,\\s*minmax\\(0');
  const narrowRule = new RegExp('@media[^{]*max-width[^{]*\\{\\s*\\.' + plotsCls +
    '\\s*\\{[^}]*grid-template-columns:\\s*minmax\\(0,\\s*1fr\\)');
  ok['the plots are a three-column grid on a wide screen'] =
    !!plotsCls && facets.length === 3 && wideRule.test(css);
  // minmax(0, ...) and not a bare 1fr: `1fr` means minmax(auto, 1fr), and an
  // SVG's intrinsic width then floors the track, which is precisely how this
  // grid would give a 400px phone a sideways scrollbar.
  ok['the plots stack to one column on a narrow screen'] =
    !!plotsCls && facets.length === 3 && narrowRule.test(css);
  ok['no plot carries a pixel width'] = plots.length === 3 &&
    plots.every(p => !p.getAttribute('width') && !/\d\s*px/.test(String(p.attrs.style || '')));

  // The top y-tick is "100%", the widest label on the plot, and it is drawn
  // end-anchored -- so it grows LEFTWARDS out of the left gutter. A facet's
  // plot clips (deliberately: past the edge is the NEXT facet's chart), so a
  // gutter narrower than the label silently ate the leading digit and every
  // facet's top tick rendered as "00%" (#206). There is no layout engine
  // here, so the width is estimated: 6 units per character at 9.5px is a fair
  // ceiling -- the digits measure nearer 5.2 and the "%" nearer 8.
  const AXIS_CH = 6;
  const gutterLbls = plots.reduce((a, p) => a.concat(findAllIn(p, '.axis')), [])
    .filter(t => t.getAttribute('text-anchor') === 'end');
  ok['no y-axis label is clipped by the gutter it hangs in'] =
    gutterLbls.length === 3 * 5 &&
    gutterLbls.every(t => parseFloat(t.getAttribute('x')) - String(t.textContent).length * AXIS_CH >= 0);

  // Eight flat lines on the floor is a shape, not a reading. The plot says so
  // in words, keeps drawing the data, and does not stack eight direct labels
  // on one pixel of line to prove it.
  const fableFacet = facets.find(f => facetWin(f) === '7d-fable');
  const fiveFacet = facets.find(f => facetWin(f) === '5h');
  const noteOf = (f) => { const n = f && findIn(f, '.facet-note'); return (n && n.textContent) || ''; };
  ok['an all-spent plot says so in words'] =
    !!fableFacet && /below the spill point/.test(noteOf(fableFacet));
  ok['an all-spent plot still draws its series'] =
    !!fableFacet && seriesIn(fableFacet).length === 8;
  // Anchored on the other plot HAVING labels, or "no labels here" passes on a
  // page that draws none anywhere.
  ok['an all-spent plot drops the unreadable direct labels'] =
    !!fableFacet && !!fiveFacet &&
    findAllIn(fableFacet, '.lbl').length === 0 &&
    findAllIn(fiveFacet, '.lbl').length > 0;
  ok['a readable plot states its series count instead'] =
    !!fiveFacet && /^8 accounts$/.test(noteOf(fiveFacet));

  // Text never wears the data colour: three of the eight light-mode slots are
  // under 3:1 on white and illegible as text. Identity rides the end dot and
  // the leader line beside the label -- and there is one leader per label, or
  // a nudged label belongs to no line at all.
  const fiveLbls = fiveFacet ? findAllIn(fiveFacet, '.lbl') : [];
  const fiveLeads = fiveFacet ? findAllIn(fiveFacet, '.lead') : [];
  ok['direct labels are ink, never the series hue'] =
    fiveLbls.length > 0 && fiveLbls.every(t => !t.getAttribute('fill'));
  ok['every direct label has a coloured leader back to its line'] =
    fiveLbls.length > 0 && fiveLeads.length === fiveLbls.length &&
    fiveLeads.every(p => isSlot(p.getAttribute('stroke')));

  // A legend is mandatory past a few series, and it is the dependable channel
  // here: the direct labels are clipped to nine characters, the legend is not.
  ok['the legend names every account exactly once'] = (() => {
    const html = els.legend.innerHTML;
    const names = ['work', 'example-two', 'example-three', 'acme', 'initech',
                   'hooli', 'globex', 'umbrella'];
    return names.every(n => html.split('>' + n + '</span>').length === 2) &&
      (html.match(/<i /g) || []).length === 8;
  })();

  // ── the categorical palette itself (#167) ────────────────────────────
  // Eight series per plot is the documented ceiling for categorical colour,
  // so the eight slots have to hold up rather than be assumed to. Only the
  // computable gate is asserted here: OKLCH lightness inside the band for the
  // surface each set is drawn on. Dark slots 7 and 8 were outside it (L 0.714
  // and 0.785 against a 0.48-0.67 band) until #167 re-stepped them.
  const okL = (hex) => {
    const v = [1, 3, 5].map(i => parseInt(hex.slice(i, i + 2), 16) / 255)
      .map(c => c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4));
    const l = Math.cbrt(0.4122214708 * v[0] + 0.5363325363 * v[1] + 0.0514459929 * v[2]);
    const m = Math.cbrt(0.2119034982 * v[0] + 0.6806995451 * v[1] + 0.1073969566 * v[2]);
    const s = Math.cbrt(0.0883024619 * v[0] + 0.2817188376 * v[1] + 0.6299787005 * v[2]);
    return 0.2104542553 * l + 0.7936177850 * m - 0.0040720468 * s;
  };
  const slotsIn = (text) => {
    const m = {};
    for (const x of text.matchAll(/--series-(\d+)\s*:\s*(#[0-9a-f]{6})/gi)) m[+x[1]] = x[2];
    return Array.from({ length: 8 }, (_, i) => m[i + 1]);
  };
  const darkAt = css.indexOf('prefers-color-scheme: dark');
  const inBand = (hexes, lo, hi) => hexes.length === 8 && hexes.every(Boolean) &&
    hexes.every(h => okL(h) >= lo && okL(h) <= hi);
  ok['light series slots 1-8 sit inside the light lightness band'] =
    darkAt > 0 && inBand(slotsIn(css.slice(0, darkAt)), 0.43, 0.77);
  ok['dark series slots 1-8 sit inside the dark lightness band'] =
    darkAt > 0 && inBand(slotsIn(css.slice(darkAt)), 0.48, 0.67);

  // ── guards on the harness itself (#202) ──────────────────────────────
  // This file is the only check on a page with no compiler and no type
  // checker, and twice it has quietly agreed with whatever it was asked: it
  // invented an element for any id, and it answered a PUT with the read
  // fixture. Nothing below tests the dashboard — they test that the fixture
  // still refuses to lie, so the next defect of that shape lands here.
  const throws = async (fn) => {
    try { await fn(); return false; } catch (e) { return true; }
  };
  ok['harness throws on an unknown element id'] =
    await throws(() => document.getElementById('no-such-element-anywhere'));
  // The other half of the same property: an id the page really did create
  // must come back as the page's own element, not a fresh empty one. A
  // harness that threw for everything would pass the assertion above and
  // still be useless.
  ok['harness returns the page’s own element for an id it created'] = (() => {
    // Caught, so a page that stopped giving its controls ids reports THIS
    // assertion red rather than aborting the run on the very lookup that is
    // under test.
    try {
      const el = document.getElementById('set-exhaustedMode');
      return !!el && el.tagName === 'SELECT' && el.dataset.key === 'exhaustedMode';
    } catch (e) { return false; }
  })();
  ok['harness serves an id declared only in the markup'] =
    document.getElementById('settings-msg') === els['settings-msg'];
  // A write answered by a read is defect 2 of #202. /api/accounts is
  // read-only here, so a PUT to it must be an error rather than the account
  // list.
  ok['harness refuses a write to a read-only route'] =
    await throws(() => fetch('/api/accounts', { method: 'PUT', body: '{}' }));
  ok['harness refuses a read of a write-only route'] =
    await throws(() => fetch('/api/pin'));
  // The #192 trap: /api/accounts/probe must never be reached by a substring
  // match on /api/accounts, in either direction.
  ok['harness matches the probe path exactly, not by prefix'] =
    await throws(() => fetch('/api/accounts/probe')) &&
    await throws(() => fetch('/api/accounts/probe/extra', { method: 'POST', body: '{}' }));
  // And a GET of /api/settings must still be a GET: the PUT branch records
  // into settingsPuts, so a read that landed there would be counted as a
  // write by every slider assertion below.
  ok['a settings read is not recorded as a write'] = await (async () => {
    const before = settingsPuts.length;
    await fetch('/api/settings');
    return settingsPuts.length === before;
  })();

  // Dry-tank countdown: an empty tank is a blank rectangle, and the only
  // thing worth saying over it is when it comes back.
  const ringsOn = findAllIn(els.accounts, '.dryring')
    .filter(r => String(r.className).includes('on'));
  const ringsOff = findAllIn(els.accounts, '.dryring')
    .filter(r => !String(r.className).includes('on'));
  // Two dry tanks in the fixture: account two's 7d, and (issue #194)
  // account three's forged 7d-fable rejection row, which is dry by
  // construction and must be drawn like any other empty tank.
  ok['countdown ring shown on the spent window'] = ringsOn.length === 2;
  // Only the spent ones: a ring over a tank that is 80% full says nothing.
  ok['countdown ring hidden on healthy windows'] = ringsOff.length >= 2;
  ok['countdown ring states the time left'] = ringsOn.length === 2 &&
    ringsOn.every(r => /^\dh(\d+m)?$|^\d+m$/.test(findIn(r, '.left')?.textContent || ''));
  // No arc any more, and its absence is the assertion: a progress arc needs
  // a start as well as an end, the API reports only the reset time, and the
  // length was guessed from the window's name. That is right for "5h" and
  // wrong for "7d", whose reset is when the oldest usage ages out — hours
  // away, not days — so the arc sat above 90% full permanently.
  // Anchored on the tanks being there at all: an absence assertion over an
  // empty panel passes while testing nothing (#202).
  ok['no progress arc is drawn'] =
    findAllIn(els.accounts, '.cyl').length > 0 &&
    findAllIn(els.accounts, '.arc').length === 0 &&
    findAllIn(els.accounts, '.track').length === 0;
  // One number in the glass, not a boxed label duplicating the caption below
  // it. The line under the tank already reads "refills 12h58m"; a plaque
  // saying "REFILLS IN 13h" over it looked like the two disagreed.
  ok['countdown is one bare figure'] = ringsOn.length === 2 &&
    ringsOn.every(r => findAllIn(r, '.plaque').length === 0 &&
      findAllIn(r, '.cap').length === 0);

  // Bubbles have to rise the height of the glass, and --climb is what says
  // how far. It used to be a percentage, and a percentage inside translate()
  // resolves against the element's own box — a bubble is under six pixels
  // across, so -90% lifted it four pixels and every bubble sat on the floor
  // of the tank. There is no layout engine here, so the assertion is on the
  // declaration rather than the position: a length, never a percentage.
  const bubs = findAllIn(els.accounts, '.bub');
  ok['bubbles exist on a wet tank'] = bubs.length > 0;
  ok['bubble climb is a length, not a percentage'] = bubs.length > 0 && bubs.every(b => {
    const m = /--climb:\s*([^;]+)/.exec(String(b.style?.cssText || ''));
    return m && !/%\s*$/.test(m[1].trim());
  });
  // And it must be measured against the glass, so the rise ends at the
  // waterline rather than at a number that drifts when the tank is resized.
  ok['bubble climb is measured against the glass'] = bubs.length > 0 && bubs.every(b =>
    /--climb:[^;]*--glass-h/.test(String(b.style?.cssText || '')));

  // Extra usage is shown but NOT editable. The panel is a browser page, and
  // this is the only setting that decides whether the user is charged.
  // One header row for the account table, not a caption per control, and the
  // explanatory note once rather than on every row.
  ok['account rows have a single header'] =
    findAllIn(els.settings, '.acct').filter(r => String(r.className).includes('head')).length === 1;
  ok['account note appears once'] = findAllIn(els.settings, '.acctnote').length === 1;

  const ro = findAllIn(els.settings, '.readonly');
  ok['extra usage state shown per account'] = ro.length >= 1;
  // Over the per-account rows, and only after confirming there ARE some: the
  // previous version ran .every() over a collection that is empty whenever
  // the panel fails to build, which passes without testing anything (#202).
  // Each such row must carry the read-only cell and no overage input.
  const acctRows = findAllIn(els.settings, '.acct').filter(r => !matches(r, '.head'));
  ok['extra usage is not an input'] =
    acctRows.length > 0 &&
    acctRows.every(r => findAllIn(r, '.readonly').length === 1) &&
    acctRows.every(r => !findAllIn(r, 'input').some(i => i.dataset && i.dataset.field === 'allowOverage'));

  // Fire the poll tick: it must re-fetch every panel (the frozen-dashboard bug).
  const before = { ...fetchCount };
  const wavesBeforePoll = waves.length;
  const ringCountBefore = findAllIn(els.accounts, '.dryring').length;
  if (pollFn) { await pollFn(); await new Promise(r => setTimeout(r, 120)); }
  // Rebuilding the cards on every poll restarted every wave animation, which
  // looked like a smooth drift snapping back every 5 seconds. The elements
  // must survive a refresh.
  ok['poll does not recreate wave elements'] = waves.length === wavesBeforePoll;
  // Same lesson as the waves: rebuilding the ring on every poll restarts its
  // transition, so the arc jumps backwards every five seconds.
  ok['poll does not recreate countdown rings'] =
    findAllIn(els.accounts, '.dryring').length === ringCountBefore;
  ok['poll tick refetches accounts'] = fetchCount.accounts > before.accounts;
  ok['poll tick refetches requests'] = fetchCount.requests > before.requests;
  ok['poll tick refetches history'] = fetchCount.history > before.history;
  ok['poll tick refetches pin state'] = fetchCount.state > before.state;

  // ── pin control (#11) ────────────────────────────────────────────────
  const oneBtn = findCardByBtnTitle(els.accounts, /work/);   // labelled account
  const twoBtn = findCardByBtnTitle(els.accounts, /example-two/);
  ok['pin control rendered per account'] = !!oneBtn && !!twoBtn;
  ok['nothing pinned initially'] =
    !!oneBtn && !!twoBtn &&
    !String(oneBtn.btn.className).includes('active') &&
    !String(twoBtn.btn.className).includes('active');
  ok['pin message box starts hidden'] =
    !!oneBtn && !!twoBtn &&
    !String(oneBtn.msg.className).includes('show') &&
    !String(twoBtn.msg.className).includes('show');

  // 400 (fixture: example-one always refuses): show why, offer no retry.
  oneBtn.btn._on.click();
  await new Promise(r => setTimeout(r, 150));
  ok['400 shows the server message'] =
    String(oneBtn.msg.className).includes('show') &&
    String(oneBtn.msg.className).includes('bad') &&
    oneBtn.msg.innerHTML.includes('malformed body');
  ok['400 offers no retry'] = findAllIn(oneBtn.msg, 'button').length === 0;
  ok['400 does not pin the account'] = !String(oneBtn.btn.className).includes('active');

  // 409 (fixture: example-two refuses unless forced): surface the reason,
  // offer a forced retry, do NOT force silently and do NOT give up silently.
  twoBtn.btn._on.click();
  await new Promise(r => setTimeout(r, 150));
  ok['409 surfaces the refusal reason'] =
    String(twoBtn.msg.className).includes('conflict') &&
    twoBtn.msg.innerHTML.includes('spend money') &&
    !twoBtn.msg.innerHTML.includes('spillway:');
  ok['409 offers a retry, not a silent force'] = findAllIn(twoBtn.msg, 'button').length === 2;
  ok['409 does not pin without confirmation'] = !String(twoBtn.btn.className).includes('active');

  // Confirm the forced retry: the fixture now accepts it (200), and the
  // response's warning field must say the pin costs the prompt cache.
  const forceBtn = findAllIn(twoBtn.msg, 'button').find(b => !String(b.className).includes('ghost'));
  // Guarded rather than assumed present: a defect that skips the conflict
  // panel (silently forcing, or silently giving up) must show up as the
  // assertions above going red, not as this crashing before they print.
  if (forceBtn) forceBtn._on.click();
  await new Promise(r => setTimeout(r, 150));
  ok['forced pin succeeds'] = String(twoBtn.btn.className).includes('active');
  ok['success states the prompt-cache cost'] = twoBtn.msg.innerHTML.toLowerCase().includes('prompt cache');
  ok['pinned tank is visibly marked'] = String(twoBtn.card.className).includes('pinned');
  ok['only the pinned account is marked'] =
    !String(oneBtn.card.className).includes('pinned') &&
    !String(oneBtn.btn.className).includes('active');

  // Clicking the now-active control returns to automatic (DELETE /api/pin).
  twoBtn.btn._on.click();
  await new Promise(r => setTimeout(r, 150));
  ok['clicking the pinned control returns to automatic'] =
    !String(twoBtn.btn.className).includes('active') &&
    !String(twoBtn.card.className).includes('pinned');

  // The critical property (#11): the pinned indicator must be driven by
  // state.pinned from the poll, not by anything a click set locally. Change
  // it the way another client would (`spillway switch` from the CLI) --
  // directly in the fixture the dashboard never touches -- with NO click
  // anywhere in this dashboard, and confirm the NEXT poll alone updates it.
  // Seed the pin onto account two through the poll alone first. Without
  // this step, two had ALREADY been unpinned by the click above, so 'the
  // account that lost the pin updates too' passed whatever the poll did —
  // it asserted a state that was true before the poll ran (#202). Losing
  // the pin has to be a transition to be observable.
  pinState = { pinned: 'you@example-two.com' };
  if (pollFn) { await pollFn(); await new Promise(r => setTimeout(r, 150)); }
  ok['a pin set by another client appears after the next poll'] =
    String(twoBtn.btn.className).includes('active') &&
    String(twoBtn.card.className).includes('pinned');
  // Now move it, again with no click anywhere in this dashboard.
  pinState = { pinned: 'you@example-one.com' };
  if (pollFn) { await pollFn(); await new Promise(r => setTimeout(r, 150)); }
  ok['a pin moved by another client follows it'] =
    String(oneBtn.btn.className).includes('active') &&
    String(oneBtn.card.className).includes('pinned');
  ok['the account that lost the pin updates too'] =
    !String(twoBtn.btn.className).includes('active') &&
    !String(twoBtn.card.className).includes('pinned');

  // ── check now (#192) ─────────────────────────────────────────────────
  // The gap this closes: a user buys a reset, spillway is deep in #90's
  // re-probe backoff, and the only way to make it look was to restart the
  // daemon. So the control has to exist per account, must not spend money
  // without being told twice, and must say when it did.
  const threeBtn = findCardByBtnTitle(els.accounts, /example-three/);
  ok['check-now control rendered per account'] =
    !!oneBtn.probe && !!twoBtn.probe && !!(threeBtn && threeBtn.probe);

  // 502 (fixture: example-one's probe always fails): report it, offer no
  // retry. Forcing cannot fix an upstream that did not answer.
  // Guarded rather than assumed present, the same way the pin block guards
  // its forced-retry click: a defect that drops the control entirely must
  // show up as 'check-now control rendered per account' going red, not as a
  // TypeError that kills the run before any assertion prints.
  const clickProbe = (e) => { if (e && e.probe && e.probe._on) e.probe._on.click(); };
  const probesBefore = probeCalls.length;
  clickProbe(oneBtn);
  await new Promise(r => setTimeout(r, 150));
  ok['probe sends the account name and does not force by default'] =
    probeCalls.length === probesBefore + 1 &&
    probeCalls[probesBefore].name === 'you@example-one.com' &&
    probeCalls[probesBefore].force === false;
  ok['probe carries the bearer token'] =
    probeCalls.length > probesBefore &&
    probeCalls[probesBefore].auth === 'Bearer T';
  ok['502 shows the server message'] =
    String(oneBtn.msg.className).includes('show') &&
    String(oneBtn.msg.className).includes('bad') &&
    oneBtn.msg.innerHTML.includes('upstream timeout');
  ok['502 offers no retry'] = findAllIn(oneBtn.msg, 'button').length === 0;
  ok['check-now button returns to idle after a failed probe'] =
    !!oneBtn.probe && !String(oneBtn.probe.className).includes('busy');

  // A free probe (fixture: example-three succeeds unforced) must just work,
  // with no talk of charges — that is the common case and the whole reason
  // the refusal below is narrow.
  clickProbe(threeBtn);
  await new Promise(r => setTimeout(r, 150));
  ok['a free probe succeeds with no ceremony'] =
    String(threeBtn.msg.className).includes('ok') &&
    threeBtn.msg.innerHTML.includes('5h');
  ok['a free probe does not claim it was charged'] =
    !threeBtn.msg.innerHTML.toLowerCase().includes('charged');

  // 409 (fixture: example-two would be billed): surface the reason, offer a
  // forced retry, and do NOT force silently.
  const beforeConflict = probeCalls.length;
  clickProbe(twoBtn);
  await new Promise(r => setTimeout(r, 150));
  ok['409 surfaces the charge refusal'] =
    String(twoBtn.msg.className).includes('conflict') &&
    twoBtn.msg.innerHTML.includes('spend money') &&
    !twoBtn.msg.innerHTML.includes('spillway:');
  ok['probe 409 offers a retry, not a silent force'] = findAllIn(twoBtn.msg, 'button').length === 2;
  ok['409 did not force the probe'] =
    probeCalls.length === beforeConflict + 1 && probeCalls[beforeConflict].force === false;
  // The retry button has to say what it costs. "Yes" on a message the reader
  // skimmed is not consent to a charge.
  const forceProbe = findAllIn(twoBtn.msg, 'button').find(b => !String(b.className).includes('ghost'));
  ok['the forced retry names the cost'] =
    !!forceProbe && /charg/i.test(String(forceProbe.textContent || forceProbe._text || ''));

  const beforeForce = probeCalls.length;
  if (forceProbe) forceProbe._on.click();
  await new Promise(r => setTimeout(r, 150));
  ok['the forced retry carries force'] =
    probeCalls.length === beforeForce + 1 && probeCalls[beforeForce].force === true;
  ok['a charged probe says so'] =
    String(twoBtn.msg.className).includes('ok') &&
    twoBtn.msg.innerHTML.toLowerCase().includes('charged');
  // Probing is not pinning. The two controls share a card and a message box;
  // they must not share an effect.
  // Both halves: two must not acquire the pin, AND one must not lose it.
  // The negative alone passed over a two that was already unpinned (#202).
  ok['probing does not pin the account'] =
    !String(twoBtn.btn.className).includes('active') &&
    !String(twoBtn.card.className).includes('pinned') &&
    String(oneBtn.btn.className).includes('active') &&
    String(oneBtn.card.className).includes('pinned');

  // ── rotate-away slider (#168) ────────────────────────────────────────
  // Last in the file on purpose: a slider write calls refresh(), which
  // re-fetches and re-renders the tanks, so running it earlier would move
  // the ground under the pin and probe assertions above.
  const slider = findAllIn(els.settings, 'input')
    .find(i => i.dataset && i.dataset.key === 'switchThreshold');
  const readout = findIn(els.settings, '.rangeval');
  ok['rotate-away is a slider'] = !!slider && slider.type === 'range';
  // 0.50-1.00 in hundredths. Below half a window used this stops being
  // predictive rotation and becomes a different strategy; 1.00 is the
  // config's own ceiling and means "never rotate early".
  ok['slider bounds are 0.50 to 1.00'] = !!slider &&
    parseFloat(slider.getAttribute('min')) === 0.5 &&
    parseFloat(slider.getAttribute('max')) === 1;
  ok['slider steps in hundredths'] = !!slider && parseFloat(slider.getAttribute('step')) === 0.01;
  ok['slider starts at the configured value'] = !!slider && parseFloat(slider.value) === 0.98;
  // A bare slider hides what it is set to, and this is a number people quote.
  ok['slider shows its numeric value'] = !!readout && readout.textContent === '0.98';

  // The label has to say what it does, not just name the field: it changes
  // routing for every request.
  const thrRow = findAllIn(els.settings, '.setrow')
    .find(r => findAllIn(r, 'input').some(i => i.dataset && i.dataset.key === 'switchThreshold'));
  const thrText = thrRow ? thrRow.innerHTML : '';
  ok['slider is labelled in the README’s words'] =
    /Rotate away at/.test(thrText) && /predictive rotation/i.test(thrText) &&
    /skipped/i.test(thrText) && /every request/i.test(thrText);

  // Every assertion below drives the control. Pre-seeded false and guarded,
  // so a defect that drops the slider or its readout shows up as the
  // assertions above going red rather than as a TypeError that kills the run
  // before anything prints — the same guard the pin and probe blocks use.
  for (const k of ['Save of an untouched slider does not rewrite the value',
                   'no write is issued mid-drag',
                   'readout tracks the slider while dragging',
                   'dragging 12 positions issues one write',
                   'the write carries the final position',
                   'the slider write is authenticated',
                   'the slider write names only its own key',
                   'an unrelated setting survives the slider write',
                   'the low bound renders as 0.50',
                   'the high bound renders as 1.00',
                   'a config value under the floor widens the slider, not the other way round',
                  ]) ok[k] = false;

  if (slider && readout && slider._on && slider._on.input) {
    // An untouched slider must write back what the SERVER sent, not what the
    // control holds: config.Validate accepts any fraction in (0, 1], and a
    // browser snaps a range input's value onto the step grid on read.
    // Simulate that snap, then Save some other field, and confirm the
    // hand-edited value survived. Deliberately before any input event below
    // — once the user has moved it, the control IS the value.
    const saveBtn = findAllIn(els.settings, 'button')[0];
    slider.value = '0.97';                        // the browser, not the user
    const beforeUntouched = settingsPuts.length;
    if (saveBtn && saveBtn._on) saveBtn._on.click();
    await new Promise(r => setTimeout(r, 200));
    ok['Save of an untouched slider does not rewrite the value'] =
      settingsPuts.length === beforeUntouched + 1 &&
      settingsPuts[beforeUntouched].body.switchThreshold === '0.98';
    // Defect 1 of #202, stated head-on: collectSettings read phantom
    // elements, so a Save submitted exhaustedMode:"" with no switchThreshold
    // and no probeOnStart — a body the page cannot produce and the real
    // server answers with a 400. Assert the WHOLE body against the values
    // the fixture served, so a lookup that finds nothing real is a failure
    // here rather than a passing test of an empty form.
    const saved = settingsPuts.length > beforeUntouched ? settingsPuts[beforeUntouched].body : null;
    ok['Save submits every settings field with the panel\u2019s own value'] =
      !!saved && saved.exhaustedMode === 'notify' && saved.holdMax === '4h' &&
      saved.switchThreshold === '0.98' && saved.probeInterval === '30m' &&
      saved.probeOnStart === true && saved.crossProvider === false;
    // The per-account rows go out on the same body, and they are read by a
    // different path (querySelectorAll over [data-account], not by id).
    ok['Save submits the per-account rows'] = (() => {
      const a = saved && saved.accounts && saved.accounts['you@example-one.com'];
      return !!a && a.label === 'work' && a.disabled === false && a.priority === 0;
    })();
    slider.value = '0.98';                        // undo the simulated snap

    // Debounce: dragging fires an input event per pixel, and each write is a
    // config rewrite plus a pool.Apply on the running daemon.
    const beforeDrag = settingsPuts.length;
    const drag = ['0.97', '0.96', '0.95', '0.94', '0.93', '0.92',
                  '0.91', '0.90', '0.89', '0.88', '0.87', '0.86'];
    for (const v of drag) {
      slider.value = v;
      slider._on.input();
      await new Promise(r => setTimeout(r, 15)); // ~180ms of drag, under the debounce
    }
    // Nothing may have gone out yet: the drag has not paused.
    ok['no write is issued mid-drag'] = settingsPuts.length - beforeDrag === 0;
    // The readout tracks the thumb the whole way, with no write behind it.
    ok['readout tracks the slider while dragging'] = readout.textContent === '0.86';
    await new Promise(r => setTimeout(r, 700));  // past WRITE_DEBOUNCE_MS
    const wrote = settingsPuts.length - beforeDrag;
    ok['dragging 12 positions issues one write'] = wrote === 1;
    ok['the write carries the final position'] =
      wrote === 1 && settingsPuts[beforeDrag].body.switchThreshold === '0.86';
    ok['the slider write is authenticated'] =
      wrote === 1 && settingsPuts[beforeDrag].auth === 'Bearer T';
    // The write path is shared with every other setting, and a body naming
    // more than it changed is how a slider clobbers an unrelated field.
    ok['the slider write names only its own key'] =
      wrote === 1 && Object.keys(settingsPuts[beforeDrag].body).join(',') === 'switchThreshold';
    // Same property from the other side: the stored settings the fixture
    // applies each body to must still hold everything the slider never named.
    // Gated on the write having happened: the stored settings are untouched
    // when NOTHING was written, which is precisely the state defect 2 of
    // #202 produced, so the ungated version passed hardest when the write
    // path was most broken.
    ok['an unrelated setting survives the slider write'] =
      wrote === 1 &&
      SETTINGS.exhaustedMode === 'notify' && SETTINGS.holdMax === '4h' &&
      SETTINGS.probeInterval === '30m' && SETTINGS.crossProvider === false;

    // Both bounds render as the two-decimal figure the config states, and
    // the rendered figure is the slider's own position — not a stale one.
    const atBound = async (v) => {
      slider.value = v;
      slider._on.input();
      await new Promise(r => setTimeout(r, 10));
      return readout.textContent;
    };
    const atMin = await atBound('0.5');
    const atMax = await atBound('1');
    ok['the low bound renders as 0.50'] = atMin === '0.50' && parseFloat(atMin) === 0.5;
    ok['the high bound renders as 1.00'] = atMax === '1.00' && parseFloat(atMax) === 1;
    // Let the trailing debounce fire so it cannot land after the summary.
    await new Promise(r => setTimeout(r, 700));

    // A config value under the slider's floor is legal — config.Validate
    // accepts anything in (0, 1] — so the floor moves down to meet it rather
    // than the control clamping a hand-edited number the next time anything
    // on the panel is saved. Rebuilt from a fresh payload, because the panel
    // is built once per page load; last of all, so nothing above sees it.
    __page.buildSettings({ switchThreshold: '0.3', exhaustedMode: 'notify', accounts: {} });
    const low = findAllIn(els.settings, 'input')
      .find(i => i.dataset && i.dataset.key === 'switchThreshold');
    ok['a config value under the floor widens the slider, not the other way round'] =
      !!low && parseFloat(low.getAttribute('min')) === 0.3 && parseFloat(low.value) === 0.3;
  }

  ok['the harness ran to completion'] = true;
}

run().then(report, (err) => abort('harness aborted', err));
