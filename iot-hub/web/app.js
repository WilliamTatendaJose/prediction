import { TILE_TYPES } from './tiles.js';

const $ = (id) => document.getElementById(id);
const grid = $('grid');
let sensors = {};             // id -> sensor view from /api/sensors
let layout = { tiles: [] };   // persisted dashboard
let live = new Map();         // tile id -> { cfg, inst, el }
const dirty = new Set();
let es = null;
let dirtyLayout = false;
const active = new Map();     // open anomaly episodes by id

// ---- API ------------------------------------------------------------------
// Auth lives in an HttpOnly cookie set by /api/login: scripts never see the
// token. X-Requested-With is the server's CSRF guard for cookie writes.
class Unauthorized extends Error {}
async function api(path, opts = {}) {
  const headers = { 'X-Requested-With': 'iothub', ...(opts.body ? { 'Content-Type': 'application/json' } : {}) };
  const res = await fetch(path, { ...opts, headers, credentials: 'same-origin' });
  if (res.status === 401) { showLogin(); throw new Unauthorized('login required'); }
  if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
  return res.status === 204 ? null : res.json();
}

let me = { authEnabled: false, identity: { role: 'admin' } };
const canManage = () => !me.authEnabled || me.identity?.role === 'admin';
const canOperate = () => canManage() || me.identity?.role === 'operator';

function showLogin(msg) {
  const d = $('login');
  if (d.open) return;
  $('login-err').textContent = msg || '';
  $('login-token').value = '';
  d.showModal();
}
$('login-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const res = await fetch('/api/login', {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'iothub' },
    body: JSON.stringify({ token: $('login-token').value.trim() }),
  });
  if (!res.ok) { $('login-err').textContent = 'That token was not accepted.'; return; }
  location.reload();
});
$('logout').onclick = async () => {
  await fetch('/api/logout', { method: 'POST', credentials: 'same-origin', headers: { 'X-Requested-With': 'iothub' } });
  location.reload();
};
function safeGet(k) { try { return localStorage.getItem(k); } catch { return null; } }
function safeSet(k, v) { try { localStorage.setItem(k, v); } catch { /* storage unavailable */ } }

async function loadSensors() {
  const list = await api('/api/sensors');
  sensors = Object.fromEntries(list.map((s) => [s.id, s]));
}

// ---- rendering --------------------------------------------------------------
function invalidate(id) {
  if (!dirty.size) requestAnimationFrame(flush);
  dirty.add(id);
}
function flush() {
  for (const id of dirty) live.get(id)?.inst.render();
  dirty.clear();
}

function tileFields(cfg) { return cfg.fields || [cfg.field]; }

function mountTile(cfg) {
  const el = document.createElement('section');
  el.className = 'tile';
  el.dataset.w = cfg.w || 1;
  el.dataset.h = cfg.h || 1;
  const bar = document.createElement('div');
  bar.className = 'edit-bar';
  for (const [label, title, fn] of [['◀', 'Move left', () => move(cfg.id, -1)], ['▶', 'Move right', () => move(cfg.id, 1)], ['✕', 'Remove', () => remove(cfg.id)]]) {
    const b = document.createElement('button');
    b.textContent = label; b.title = title; b.setAttribute('aria-label', title); b.onclick = fn;
    bar.append(b);
  }
  el.append(bar);
  const stale = document.createElement('span'); stale.className = 'stale'; el.append(stale);

  const type = TILE_TYPES[cfg.type];
  const sensor = sensors[cfg.sensor];
  let inst = { update() {}, render() {} };
  if (!type || (!sensor && !(type.sensorOptional && !cfg.sensor))) {
    const h = document.createElement('h2'); h.textContent = cfg.title || cfg.sensor;
    const p = document.createElement('p'); p.className = 'sub';
    p.textContent = !type ? `Unknown tile type "${cfg.type}"` : `Waiting for sensor "${cfg.sensor}"`;
    el.append(h, p);
  } else {
    const ctx = {
      sensor,
      can: (a) => (a === 'operate' ? canOperate() : a === 'manage' ? canManage() : true),
      api,
      unit: (f) => sensor?.fields?.[f]?.unit || '',
      history: (f, limit) => api(`/api/sensors/${encodeURIComponent(cfg.sensor)}/history?field=${encodeURIComponent(f)}&limit=${limit}`).catch(() => ({ t: [], v: [] })),
      invalidate: () => invalidate(cfg.id),
    };
    inst = type.create(el, cfg, ctx);
    const f = tileFields(cfg);
    if (sensor?.last && f.some((k) => k in sensor.last)) {
      inst.update(sensor.lastSeen, type.multi ? {} : sensor.last[cfg.field]);
    }
  }
  live.set(cfg.id, { cfg, inst, el, stale });
  grid.append(el);
  invalidate(cfg.id);
}

function build() {
  for (const { inst } of live.values()) inst.destroy?.();
  live = new Map();
  grid.replaceChildren();
  if (!layout.tiles.length) {
    const p = document.createElement('p'); p.className = 'empty';
    const n = Object.keys(sensors).length;
    p.textContent = n
      ? `${n} sensor${n > 1 ? 's' : ''} reporting. Click Edit → Add tile to build your dashboard.`
      : 'No sensors yet. Publish to MQTT topic iot/<sensor> or POST /api/sensors/<sensor>/data.';
    grid.append(p);
  }
  layout.tiles.forEach(mountTile);
  markStale();
}

// ---- live stream ------------------------------------------------------------
let refreshTimer = null;
function connect() {
  es?.close();
  const conn = $('conn');
  es = new EventSource('/api/stream');
  es.onopen = () => { conn.dataset.state = 'live'; conn.textContent = 'Live'; };
  es.onerror = () => { conn.dataset.state = 'down'; conn.textContent = 'Reconnecting…'; };
  es.onmessage = (ev) => {
    const r = JSON.parse(ev.data); // { s: sensor, t: ms, v: { field: value } }
    const s = sensors[r.s];
    if (!s) { // new sensor: refresh the list once, then rebuild tiles waiting on it
      clearTimeout(refreshTimer);
      refreshTimer = setTimeout(async () => { await loadSensors(); if (layout.tiles.some((t) => t.sensor === r.s)) build(); }, 500);
      return;
    }
    Object.assign(s.last ||= {}, r.v);
    s.lastSeen = r.t;
    for (const [id, { cfg, inst }] of live) {
      if (cfg.sensor !== r.s) continue;
      if (TILE_TYPES[cfg.type]?.multi) {
        if (tileFields(cfg).some((f) => f in r.v)) { inst.update(r.t, r.v); invalidate(id); }
      } else if (cfg.field in r.v) { inst.update(r.t, r.v[cfg.field]); invalidate(id); }
    }
  };
  es.addEventListener('alarms', onAlarmsChanged);
  es.addEventListener('anomaly', (ev) => {
    const a = JSON.parse(ev.data);
    if (a.end || a.shelved) active.delete(a.id); else active.set(a.id, a);
    renderAlerts();
    for (const [id, { cfg, inst }] of live) {
      if (inst.anomaly && (!cfg.sensor || cfg.sensor === a.sensor)) { inst.anomaly(a); invalidate(id); }
    }
  });
}

// Someone acknowledged or shelved: refresh counts and alarm tiles.
function onAlarmsChanged() {
  loadActive();
  for (const [id, { inst }] of live) if (inst.alarmsChanged) { inst.alarmsChanged(); invalidate(id); }
}

async function loadActive() {
  const list = await api('/api/anomalies?active=1&limit=1000').catch(() => []);
  active.clear();
  for (const a of list) if (!a.shelved) active.set(a.id, a);
  renderAlerts();
}

// The badge counts what still needs a person: active, unshelved, unacknowledged.
function renderAlerts() {
  const el = $('alerts');
  const open = [...active.values()];
  const unacked = open.filter((a) => !a.ack);
  el.hidden = open.length === 0;
  el.dataset.level = unacked.length ? 'unacked' : 'acked';
  el.textContent = unacked.length
    ? `${unacked.length} unacknowledged alarm${unacked.length === 1 ? '' : 's'}`
    : `${open.length} active (acknowledged)`;
  el.title = open.slice(0, 10).map((a) => `${a.ack ? '✓ ' : ''}${a.sensor}${a.field ? '.' + a.field : ''}: ${a.message}`).join('\n');
}

function markStale() {
  const now = Date.now();
  for (const { cfg, stale } of live.values()) {
    const seen = sensors[cfg.sensor]?.lastSeen;
    const age = seen ? (now - seen) / 1000 : null;
    stale.textContent = age == null ? '' : age > 120 ? `${age > 3600 ? Math.round(age / 3600) + 'h' : Math.round(age / 60) + 'm'} ago` : '';
  }
}
setInterval(markStale, 15000);

// Hidden tabs hold no connection and do no work; on return, resync.
document.addEventListener('visibilitychange', async () => {
  if (document.hidden) { es?.close(); es = null; return; }
  await loadSensors().catch(() => {});
  build();
  connect();
  loadActive();
});

// ---- editing ------------------------------------------------------------------
function setEditing(on) {
  document.body.classList.toggle('editing', on);
  $('edit').textContent = on ? 'Done' : 'Edit';
  $('add').hidden = !on;
  $('save').hidden = !on || !dirtyLayout;
}
function changed() { dirtyLayout = true; $('save').hidden = false; build(); }
function move(id, d) {
  const i = layout.tiles.findIndex((t) => t.id === id), j = i + d;
  if (j < 0 || j >= layout.tiles.length) return;
  [layout.tiles[i], layout.tiles[j]] = [layout.tiles[j], layout.tiles[i]];
  changed();
}
function remove(id) { layout.tiles = layout.tiles.filter((t) => t.id !== id); changed(); }

$('edit').onclick = () => setEditing(!document.body.classList.contains('editing'));
$('save').onclick = async () => {
  try { await api('/api/dashboard', { method: 'PUT', body: JSON.stringify(layout) }); dirtyLayout = false; $('save').hidden = true; }
  catch (e) { alert('Save failed: ' + e.message); }
};
$('theme').onclick = () => {
  const dark = document.documentElement.dataset.theme
    ? document.documentElement.dataset.theme === 'dark'
    : matchMedia('(prefers-color-scheme: dark)').matches;
  document.documentElement.dataset.theme = dark ? 'light' : 'dark';
  safeSet('iothub.theme', document.documentElement.dataset.theme);
  for (const id of live.keys()) invalidate(id); // canvases read colors at draw time
};

// Add-tile dialog
const dlg = $('dlg'), form = $('form');
function opt(sel, value, text) { const o = document.createElement('option'); o.value = value; o.textContent = text; sel.append(o); }
function fillFields() {
  const t = TILE_TYPES[form.type.value];
  const s = sensors[form.sensor.value], multi = t?.multi;
  const noField = !!t?.noField;
  form.field.hidden = noField; form.field.required = !noField;
  form.field.previousElementSibling.hidden = noField;
  form.field.multiple = !!multi;
  form.field.replaceChildren();
  for (const f of Object.keys(s?.fields || {}).sort()) {
    const unit = s.fields[f].unit;
    opt(form.field, f, unit ? `${f} (${unit})` : f);
  }
  if (multi && form.field.options[0]) form.field.options[0].selected = true;
}
function fillOptions() {
  const t = TILE_TYPES[form.type.value], box = $('f-opts');
  box.replaceChildren();
  for (const o of t.options || []) {
    const l = document.createElement('label'); l.textContent = o.label; l.htmlFor = 'o-' + o.key;
    let i;
    if (o.type === 'select') {
      i = document.createElement('select');
      for (const [v, text] of o.choices) opt(i, v, text);
    } else {
      i = document.createElement('input');
      i.type = o.type === 'number' ? 'number' : 'text'; if (o.type === 'number') i.step = 'any';
    }
    i.id = 'o-' + o.key; i.name = 'o-' + o.key;
    box.append(l, i);
  }
  // Sensor list: "(all sensors)" only for tiles that support it.
  const cur = form.sensor.value;
  form.sensor.replaceChildren();
  if (t.sensorOptional) opt(form.sensor, '', '(all sensors)');
  for (const s of Object.values(sensors)) opt(form.sensor, s.id, s.name && s.name !== s.id ? `${s.name} (${s.id})` : s.id);
  if ([...form.sensor.options].some((x) => x.value === cur)) form.sensor.value = cur;
  const d = t.defaultSize || {};
  form.w.value = d.w || 1; form.h.value = d.h || 1;
  fillFields();
}
$('add').onclick = async () => {
  await loadSensors().catch(() => {});
  form.type.replaceChildren(); form.sensor.replaceChildren();
  for (const [k, t] of Object.entries(TILE_TYPES)) opt(form.type, k, t.label);
  form.title.value = '';
  fillOptions();
  dlg.showModal();
};
form.type.onchange = fillOptions;
form.sensor.onchange = fillFields;
dlg.addEventListener('close', () => {
  const t = TILE_TYPES[form.type.value];
  if (dlg.returnValue !== 'ok' || (!form.sensor.value && !t.sensorOptional)) return;
  const fields = [...form.field.selectedOptions].map((o) => o.value);
  if (!fields.length && !t.noField) return;
  const options = {};
  for (const o of t.options || []) { const v = form.elements['o-' + o.key].value; if (v !== '') options[o.key] = o.type === 'number' ? +v : v; }
  const cfg = {
    id: Math.random().toString(36).slice(2, 10),
    type: form.type.value, sensor: form.sensor.value,
    title: form.title.value.trim() || undefined,
    w: +form.w.value, h: +form.h.value, options,
  };
  if (t.multi) cfg.fields = fields; else if (!t.noField) cfg.field = fields[0];
  layout.tiles.push(cfg);
  changed();
});

// ---- boot -------------------------------------------------------------------
const savedTheme = safeGet('iothub.theme');
if (savedTheme) document.documentElement.dataset.theme = savedTheme;
// Boot retries until the hub answers: an installed dashboard is often
// opened before the network (or the hub) is back.
let bootDelay = 2000, bootTimer = null, booting = false, booted = false;
function offlineText() { return navigator.onLine ? 'Hub unreachable, retrying…' : 'Offline'; }
async function boot() {
  if (booting || booted) return;
  booting = true;
  clearTimeout(bootTimer);
  try {
    const r = await fetch('/api/me', { credentials: 'same-origin' });
    me = await r.json();
    if (r.status === 401 && !me.publicRead) showLogin();
    $('edit').hidden = !canManage();
    $('logout').hidden = !me.authEnabled || !me.identity;
    if (me.identity && me.authEnabled) $('logout').title = `Signed in as ${me.identity.id} (${me.identity.role})`;
    const [, dash] = await Promise.all([loadSensors(), api('/api/dashboard')]);
    layout = dash && Array.isArray(dash.tiles) ? dash : { tiles: [] };
    booted = true;
    build();
    connect();
    loadActive();
  } catch (e) {
    if (!(e instanceof Unauthorized)) {
      $('conn').dataset.state = 'down';
      $('conn').textContent = offlineText();
      const p = document.createElement('p');
      p.className = 'empty';
      p.textContent = navigator.onLine
        ? "Can't reach the hub. The dashboard will load by itself when it answers."
        : 'No network connection. The dashboard will load by itself when the connection returns.';
      grid.replaceChildren(p);
      bootTimer = setTimeout(boot, bootDelay);
      bootDelay = Math.min(bootDelay * 2, 60000);
    }
  } finally {
    booting = false;
  }
}
$('edit').hidden = true; // until /api/me says what this user may do
addEventListener('online', () => { bootDelay = 2000; boot(); });
addEventListener('offline', () => { $('conn').dataset.state = 'down'; $('conn').textContent = 'Offline'; });
boot();

// ---- install (PWA) ----------------------------------------------------------
// Service workers need a secure context: HTTPS (-tls-cert) or localhost.
if ('serviceWorker' in navigator && window.isSecureContext) {
  navigator.serviceWorker.register('/sw.js').catch(() => { /* still works as a page */ });
}
let installPrompt = null;
addEventListener('beforeinstallprompt', (e) => { e.preventDefault(); installPrompt = e; $('install').hidden = false; });
addEventListener('appinstalled', () => { installPrompt = null; $('install').hidden = true; });
$('install').addEventListener('click', async () => {
  if (!installPrompt) return;
  installPrompt.prompt();
  await installPrompt.userChoice.catch(() => {});
  installPrompt = null;
  $('install').hidden = true;
});
