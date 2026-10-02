import { TILE_TYPES } from './tiles.js';

const $ = (id) => document.getElementById(id);
const grid = $('grid');
let sensors = {};             // id -> sensor view from /api/sensors
let layout = { tiles: [] };   // persisted dashboard
let live = new Map();         // tile id -> { cfg, inst, el }
const dirty = new Set();
let es = null;
let dirtyLayout = false;

// ---- API ------------------------------------------------------------------
async function api(path, opts = {}) {
  const tok = safeGet('iothub.token');
  const headers = { ...(opts.body ? { 'Content-Type': 'application/json' } : {}), ...(tok ? { Authorization: 'Bearer ' + tok } : {}) };
  const res = await fetch(path, { ...opts, headers });
  if (res.status === 401) {
    const t = prompt('This hub requires an access token for changes:');
    if (t) { safeSet('iothub.token', t); return api(path, opts); }
  }
  if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
  return res.status === 204 ? null : res.json();
}
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
  if (!type || !sensor) {
    const h = document.createElement('h2'); h.textContent = cfg.title || cfg.sensor;
    const p = document.createElement('p'); p.className = 'sub';
    p.textContent = !type ? `Unknown tile type "${cfg.type}"` : `Waiting for sensor "${cfg.sensor}"`;
    el.append(h, p);
  } else {
    const ctx = {
      sensor,
      unit: (f) => sensor.fields?.[f]?.unit || '',
      history: (f, limit) => api(`/api/sensors/${encodeURIComponent(cfg.sensor)}/history?field=${encodeURIComponent(f)}&limit=${limit}`).catch(() => ({ t: [], v: [] })),
      invalidate: () => invalidate(cfg.id),
    };
    inst = type.create(el, cfg, ctx);
    const f = tileFields(cfg);
    if (sensor.last && f.some((k) => k in sensor.last)) {
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
  const s = sensors[form.sensor.value], multi = TILE_TYPES[form.type.value]?.multi;
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
    const i = document.createElement('input'); i.id = 'o-' + o.key; i.name = 'o-' + o.key;
    i.type = o.type === 'number' ? 'number' : 'text'; if (o.type === 'number') i.step = 'any';
    box.append(l, i);
  }
  const d = t.defaultSize || {};
  form.w.value = d.w || 1; form.h.value = d.h || 1;
  fillFields();
}
$('add').onclick = async () => {
  await loadSensors().catch(() => {});
  form.type.replaceChildren(); form.sensor.replaceChildren();
  for (const [k, t] of Object.entries(TILE_TYPES)) opt(form.type, k, t.label);
  for (const s of Object.values(sensors)) opt(form.sensor, s.id, s.name && s.name !== s.id ? `${s.name} (${s.id})` : s.id);
  form.title.value = '';
  fillOptions();
  dlg.showModal();
};
form.type.onchange = fillOptions;
form.sensor.onchange = fillFields;
dlg.addEventListener('close', () => {
  if (dlg.returnValue !== 'ok' || !form.sensor.value) return;
  const t = TILE_TYPES[form.type.value];
  const fields = [...form.field.selectedOptions].map((o) => o.value);
  if (!fields.length) return;
  const options = {};
  for (const o of t.options || []) { const v = form['o-' + o.key].value; if (v !== '') options[o.key] = o.type === 'number' ? +v : v; }
  const cfg = {
    id: Math.random().toString(36).slice(2, 10),
    type: form.type.value, sensor: form.sensor.value,
    title: form.title.value.trim() || undefined,
    w: +form.w.value, h: +form.h.value, options,
  };
  if (t.multi) cfg.fields = fields; else cfg.field = fields[0];
  layout.tiles.push(cfg);
  changed();
});

// ---- boot -------------------------------------------------------------------
const savedTheme = safeGet('iothub.theme');
if (savedTheme) document.documentElement.dataset.theme = savedTheme;
try {
  const [, dash] = await Promise.all([loadSensors(), api('/api/dashboard')]);
  layout = dash && Array.isArray(dash.tiles) ? dash : { tiles: [] };
} catch (e) {
  $('conn').dataset.state = 'down'; $('conn').textContent = 'API unreachable';
}
build();
connect();
