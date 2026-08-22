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
const ACCOUNTS = [{
  name: 'you@example-one.com', label: 'work', type: 'claude-oauth', source: 'yaml', state: 'ok', inFlight: 1,
  quotaWindows: [{ name: '5h', limit: 1, used: 0.42, resetAt: new Date(now + 3600e3).toISOString(), source: 'headers' }],
}, {
  name: 'you@example-two.com', type: 'claude-oauth', source: 'yaml', state: 'ok', inFlight: 0,
  quotaWindows: [
    { name: '5h', limit: 1, used: 0.20, resetAt: new Date(now + 7200e3).toISOString(), source: 'poll' },
    // Fully spent, refilling in two hours: the dry-tank countdown case.
    { name: '7d', limit: 1, used: 1, resetAt: new Date(now + 7200e3).toISOString(), source: 'poll' },
  ],
}];
const HISTORY = [{
  account: 'you@example-one.com', window: '5h',
  ts: [now - 3600e3, now - 1800e3, now - 60e3],
  headroom: [0.75, 0.62, 0.58],
}];
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
// pinState is /api/state's view of the pin (#11) -- the ONLY thing the pin
// tests below mutate directly to simulate another client (the CLI
// `spillway switch`) changing it out from under the dashboard. pinCalls
// records what the dashboard itself sent to POST/DELETE /api/pin.
let pinState = { pinned: "" };
const pinCalls = [];
global.fetch = async (u, opts) => {
  const url = String(u);
  const ok = (body) => ({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });
  if (url.includes('/api/accounts'))       { fetchCount.accounts++; return ok(ACCOUNTS); }
  if (url.includes('/api/quota-history'))  { fetchCount.history++;  return ok(HISTORY); }
  if (url.includes('/api/activity'))       { fetchCount.activity++; return ok(ACTIVITY); }
  if (url.includes('/api/requests'))       { fetchCount.requests++; return ok(REQUESTS); }
  if (url.includes('/api/settings'))       { fetchCount.settings++; return ok(SETTINGS); }
  if (url.includes('/api/state'))          { fetchCount.state++;    return ok(pinState); }
  if (url.includes('/api/pin')) {
    const method = (opts && opts.method) || 'POST';
    if (method === 'DELETE') {
      pinCalls.push({ method });
      pinState = { pinned: "" };
      return { ok: true, status: 200, text: async () => '{}', json: async () => ({}) };
    }
    const req = JSON.parse(opts.body);
    pinCalls.push({ method, account: req.account, force: req.force });
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
  }
  return { ok: false, status: 404, json: async () => ({}) };
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
    if (btn && re.test(btn.title)) return { card, btn, msg: findIn(card, '.pin-msg') };
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
global.document = {
  getElementById: (id) => els[id] || (els[id] = mkEl()),
  querySelectorAll: () => [],
  createElement: (tag) => mkEl(tag),
  createElementNS: (_ns, tag) => mkEl(tag),
  addEventListener: (ev, fn) => { if (ev === 'DOMContentLoaded') fn(); },
};
global.location = { search: '?token=T', href: '' };
global.sessionStorage = { getItem: () => 'T', setItem() {}, removeItem() {} };
global.window = global;

eval(js);

(async () => {
  // start() is invoked by the stubbed DOMContentLoaded during eval; give its
  // async fetch chain time to settle.
  await new Promise(r => setTimeout(r, 200));

  const tanks = els['accounts'] ? els['accounts'].innerHTML : '';
  const figures = els['figures'] ? els['figures'].innerHTML : '';
  const reqs = els['requests'] ? els['requests'].innerHTML : '';
  const burn = els['burn'] ? els['burn'].textContent : '';

  const ok = {
    'tank shows configured label': tanks.includes('work'),
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
  };

  // Dry-tank countdown: an empty tank is a blank rectangle, and the only
  // thing worth saying over it is when it comes back.
  const ringsOn = findAllIn(els.accounts, '.dryring')
    .filter(r => String(r.className).includes('on'));
  const ringsOff = findAllIn(els.accounts, '.dryring')
    .filter(r => !String(r.className).includes('on'));
  ok['countdown ring shown on the spent window'] = ringsOn.length === 1;
  // Only the spent one: a ring over a tank that is 80% full says nothing.
  ok['countdown ring hidden on healthy windows'] = ringsOff.length >= 2;
  ok['countdown ring states the time left'] = ringsOn.length === 1 &&
    /^\dh(\d+m)?$|^\d+m$/.test(findIn(ringsOn[0], '.left')?.textContent || '');
  // No arc any more, and its absence is the assertion: a progress arc needs
  // a start as well as an end, the API reports only the reset time, and the
  // length was guessed from the window's name. That is right for "5h" and
  // wrong for "7d", whose reset is when the oldest usage ages out — hours
  // away, not days — so the arc sat above 90% full permanently.
  ok['no progress arc is drawn'] =
    findAllIn(els.accounts, '.arc').length === 0 &&
    findAllIn(els.accounts, '.track').length === 0;
  // One number in the glass, not a boxed label duplicating the caption below
  // it. The line under the tank already reads "refills 12h58m"; a plaque
  // saying "REFILLS IN 13h" over it looked like the two disagreed.
  ok['countdown is one bare figure'] = ringsOn.length === 1 &&
    findAllIn(ringsOn[0], '.plaque').length === 0 &&
    findAllIn(ringsOn[0], '.cap').length === 0;

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
  ok['extra usage is not an input'] =
    findAllIn(els.settings, '.setrow')
      .every(r => !findAllIn(r, 'input').some(i => i.dataset && i.dataset.field === 'allowOverage'));

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
  pinState = { pinned: 'you@example-one.com' };
  if (pollFn) { await pollFn(); await new Promise(r => setTimeout(r, 150)); }
  ok['a pin set by another client appears after the next poll'] =
    String(oneBtn.btn.className).includes('active') &&
    String(oneBtn.card.className).includes('pinned');
  ok['the account that lost the pin updates too'] =
    !String(twoBtn.btn.className).includes('active') &&
    !String(twoBtn.card.className).includes('pinned');

  let fail = 0;
  for (const [k, v] of Object.entries(ok)) { console.log((v ? 'PASS' : 'FAIL') + ': ' + k); if (!v) fail++; }
  console.log('fetches:', JSON.stringify(fetchCount));
  process.exit(fail ? 1 : 0);
})();
