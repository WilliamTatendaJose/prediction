// Dashboard: the tenant's tile layout (tiles.js), live.
import { TILE_TYPES } from '../tiles.js';
import {
  h, api, put, enc, pageHeader, button, modal, field, opt, empty, live, canManage, canOperate, toast,
} from '../core.js';

const tileFields = (cfg) => cfg.fields || [cfg.field];

// mountTile is shared with the sensor page: it builds one tile and returns
// {el, inst, cfg}. ctx.sensors maps id → sensor.
export function mountTile(cfg, sensors, invalidate) {
  const el = h('section', { class: 'tile', dataset: { w: cfg.w || 1, h: cfg.h || 1 } });
  const stale = h('span', { class: 'stale' });
  const type = TILE_TYPES[cfg.type];
  const sensor = sensors[cfg.sensor];
  let inst = { update() {}, render() {} };
  if (!type || (!sensor && !(type.sensorOptional && !cfg.sensor))) {
    el.append(h('h2', { text: cfg.title || cfg.sensor }), h('p', { class: 'sub', text: !type ? `Unknown tile type "${cfg.type}"` : `Waiting for sensor "${cfg.sensor}"` }));
  } else {
    const tctx = {
      sensor,
      can: (a) => (a === 'operate' ? canOperate() : a === 'manage' ? canManage() : true),
      api,
      unit: (f) => sensor?.fields?.[f]?.unit || '',
      history: (f, limit) => api(`/api/sensors/${enc(cfg.sensor)}/history?field=${enc(f)}&limit=${limit}`).catch(() => ({ t: [], v: [] })),
      invalidate: () => invalidate(cfg.id),
    };
    inst = type.create(el, cfg, tctx);
    if (sensor?.last && tileFields(cfg).some((k) => k in sensor.last)) {
      inst.update(sensor.lastSeen, type.multi ? {} : sensor.last[cfg.field]);
    }
  }
  el.append(stale);
  return { el, inst, cfg, stale };
}

// A set of mounted tiles fed by the live stream.
export function liveTiles(ctx, sensors) {
  const live_ = new Map();
  const dirty = new Set();
  const flush = () => { for (const id of dirty) live_.get(id)?.inst.render(); dirty.clear(); };
  const invalidate = (id) => { if (!dirty.size) requestAnimationFrame(flush); dirty.add(id); };
  const markStale = () => {
    const now = Date.now();
    for (const { cfg, stale } of live_.values()) {
      const seen = sensors[cfg.sensor]?.lastSeen;
      const age = seen ? (now - seen) / 1000 : null;
      stale.textContent = age == null || age <= 120 ? '' : `${age > 3600 ? Math.round(age / 3600) + 'h' : Math.round(age / 60) + 'm'} ago`;
    }
  };
  const timer = setInterval(markStale, 15000);
  ctx.onLeave(() => { clearInterval(timer); for (const { inst } of live_.values()) inst.destroy?.(); });
  ctx.onLeave(live.on('reading', (r) => {
    const s = sensors[r.s];
    if (!s) { tiles.onNewSensor?.(r.s); return; }
    Object.assign(s.last ||= {}, r.v);
    s.lastSeen = r.t;
    for (const [id, { cfg, inst }] of live_) {
      if (cfg.sensor !== r.s) continue;
      if (TILE_TYPES[cfg.type]?.multi) {
        if (tileFields(cfg).some((f) => f in r.v)) { inst.update(r.t, r.v); invalidate(id); }
      } else if (cfg.field in r.v) { inst.update(r.t, r.v[cfg.field]); invalidate(id); }
    }
  }));
  ctx.onLeave(live.on('anomaly', (a) => {
    for (const [id, { cfg, inst }] of live_) if (inst.anomaly && (!cfg.sensor || cfg.sensor === a.sensor)) { inst.anomaly(a); invalidate(id); }
  }));
  ctx.onLeave(live.on('alarms', () => { for (const [id, { inst }] of live_) if (inst.alarmsChanged) { inst.alarmsChanged(); invalidate(id); } }));
  ctx.onLeave(live.on('theme', () => { for (const id of live_.keys()) invalidate(id); }));
  const tiles = {
    add(cfg) { live_.get(cfg.id)?.inst.destroy?.(); const t = mountTile(cfg, sensors, invalidate); live_.set(cfg.id, t); invalidate(cfg.id); return t; },
    clear() { for (const { inst } of live_.values()) inst.destroy?.(); live_.clear(); },
    markStale,
  };
  return tiles;
}

export async function render(el, ctx) {
  let sensors = {};
  const loadSensors = async () => { sensors = Object.fromEntries((await api('/api/sensors')).map((s) => [s.id, s])); };
  const [, dash] = await Promise.all([loadSensors(), api('/api/dashboard')]);
  let layout = dash && Array.isArray(dash.tiles) ? dash : { tiles: [] };
  let editing = false, dirtyLayout = false;

  const grid = h('div', { class: 'dash' });
  const actions = h('div', { class: 'page-actions' });
  const sensorsProxy = new Proxy({}, { get: (_, k) => sensors[k] });
  const tiles = liveTiles(ctx, sensorsProxy);
  let refreshTimer = null;
  tiles.onNewSensor = (id) => {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(async () => { await loadSensors(); if (layout.tiles.some((t) => t.sensor === id)) build(); }, 500);
  };

  function build() {
    tiles.clear();
    grid.replaceChildren();
    grid.classList.toggle('editing', editing);
    if (!layout.tiles.length) {
      const n = Object.keys(sensors).length;
      grid.append(h('div', { style: 'grid-column:1/-1;grid-row:span 2' }, empty('This dashboard is empty',
        n ? `${n} sensor${n > 1 ? 's are' : ' is'} reporting. Add tiles to show their values, charts, alarms and OEE.`
          : 'No sensors are reporting yet. Once devices send data, add tiles here.',
        canManage() && n ? button('Add a tile', () => { setEditing(true); addTile(); }, { kind: 'primary', ico: 'plus' }) : null)));
    }
    for (const cfg of layout.tiles) {
      const t = tiles.add(cfg);
      const bar = h('div', { class: 'edit-bar' },
        h('button', { type: 'button', title: 'Move earlier', 'aria-label': 'Move earlier', onclick: () => move(cfg.id, -1) }, '◀'),
        h('button', { type: 'button', title: 'Move later', 'aria-label': 'Move later', onclick: () => move(cfg.id, 1) }, '▶'),
        h('button', { type: 'button', title: 'Remove', 'aria-label': 'Remove tile', onclick: () => remove(cfg.id) }, '✕'));
      t.el.prepend(bar);
      grid.append(t.el);
    }
    tiles.markStale();
    renderActions();
  }
  function renderActions() {
    actions.replaceChildren();
    if (!canManage()) return;
    if (!editing) { actions.append(button('Edit layout', () => setEditing(true))); return; }
    actions.append(button('Add tile', addTile, { ico: 'plus' }));
    if (dirtyLayout) {
      actions.append(button('Discard', async () => { const d = await api('/api/dashboard'); layout = d?.tiles ? d : { tiles: [] }; dirtyLayout = false; setEditing(false); }));
      actions.append(button('Save layout', async () => { await put('/api/dashboard', layout); dirtyLayout = false; setEditing(false); toast('Layout saved'); }, { kind: 'primary' }));
    } else actions.append(button('Done', () => setEditing(false), { kind: 'primary' }));
  }
  function setEditing(on) { editing = on; grid.classList.toggle('editing', on); renderActions(); }
  function changed() { dirtyLayout = true; build(); }
  function move(id, d) {
    const i = layout.tiles.findIndex((t) => t.id === id), j = i + d;
    if (j < 0 || j >= layout.tiles.length) return;
    [layout.tiles[i], layout.tiles[j]] = [layout.tiles[j], layout.tiles[i]];
    changed();
  }
  function remove(id) { layout.tiles = layout.tiles.filter((t) => t.id !== id); changed(); }

  async function addTile() {
    await loadSensors().catch(() => {});
    const type = h('select', {}, Object.entries(TILE_TYPES).map(([k, t]) => opt(k, t.label)));
    const sensor = h('select');
    const fieldSel = h('select');
    const fieldWrap = field('Field', fieldSel);
    const title = h('input', { maxlength: 80, placeholder: 'Defaults to sensor · field' });
    const w = h('select', {}, opt('1', 'Narrow'), opt('2', 'Wide'));
    const ht = h('select', {}, opt('1', 'Short'), opt('2', 'Tall'));
    const opts = h('div', { class: 'form-grid' });
    const fillFields = () => {
      const t = TILE_TYPES[type.value], s = sensors[sensor.value];
      fieldWrap.hidden = !!t?.noField;
      fieldSel.multiple = !!t?.multi;
      fieldSel.size = t?.multi ? 4 : 0;
      fieldSel.replaceChildren(...Object.keys(s?.fields || {}).sort().map((f) => opt(f, s.fields[f].unit ? `${f} (${s.fields[f].unit})` : f)));
      fieldWrap.querySelector('.field-label').textContent = t?.multi ? 'Fields (pick one or more)' : 'Field';
      if (t?.multi && fieldSel.options[0]) fieldSel.options[0].selected = true;
    };
    const fillOptions = () => {
      const t = TILE_TYPES[type.value];
      opts.replaceChildren(...(t.options || []).map((o) => {
        const i = o.type === 'select' ? h('select', { name: o.key }, o.choices.map(([v, x]) => opt(v, x)))
          : h('input', { name: o.key, type: o.type === 'number' ? 'number' : 'text', step: o.type === 'number' ? 'any' : null });
        return field(o.label, i);
      }));
      const cur = sensor.value;
      sensor.replaceChildren(...(t.sensorOptional ? [opt('', 'All sensors')] : []),
        ...Object.values(sensors).sort((a, b) => a.id.localeCompare(b.id)).map((s) => opt(s.id, s.name && s.name !== s.id ? `${s.name} (${s.id})` : s.id)));
      if ([...sensor.options].some((x) => x.value === cur)) sensor.value = cur;
      const d = t.defaultSize || {};
      w.value = d.w || 1; ht.value = d.h || 1;
      fillFields();
    };
    type.onchange = fillOptions;
    sensor.onchange = fillFields;
    fillOptions();
    await modal('Add tile', h('div', { class: 'stack' },
      h('div', { class: 'form-grid' }, field('Tile type', type, { cls: 'full' }), field('Sensor', sensor), fieldWrap),
      h('div', { class: 'form-grid' }, field('Title (optional)', title, { cls: 'span2' }), field('Width', w), field('Height', ht)),
      opts), {
      actions: [['Cancel'], ['Add tile', () => {
        const t = TILE_TYPES[type.value];
        if (!sensor.value && !t.sensorOptional) throw new Error('Choose a sensor.');
        const fields = [...fieldSel.selectedOptions].map((o) => o.value);
        if (!fields.length && !t.noField) throw new Error('Choose a field.');
        const options = {};
        for (const o of t.options || []) {
          const v = opts.querySelector(`[name="${o.key}"]`).value;
          if (v !== '') options[o.key] = o.type === 'number' ? +v : v;
        }
        const cfg = { id: Math.random().toString(36).slice(2, 10), type: type.value, sensor: sensor.value,
          title: title.value.trim() || undefined, w: +w.value, h: +ht.value, options };
        if (t.multi) cfg.fields = fields; else if (!t.noField) cfg.field = fields[0];
        layout.tiles.push(cfg);
        changed();
      }, 'primary']],
    });
  }

  const head = pageHeader('Dashboard', { sub: 'Live tiles for this tenant. Everyone in the tenant sees the same layout.' });
  head.querySelector('.page-actions')?.remove();
  head.append(actions);
  el.append(head, grid);
  build();
  ctx.onLeave(() => clearTimeout(refreshTimer));
  // Leaving with unsaved edits: say so (the layout stays as it was saved).
  ctx.onLeave(() => { if (dirtyLayout) toast('Layout changes were not saved'); });
}
