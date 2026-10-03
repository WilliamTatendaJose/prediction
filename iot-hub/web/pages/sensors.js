// Sensors: the list, and one page per sensor (live values, history, alarms,
// settings: fields, limits, detection, calculated fields, OEE).
import {
  h, api, post, put, del, enc, pageHeader, card, badge, empty, table, tabs, field, opt, button, icon, linkButton,
  modal, confirmDialog, live, canManage, ago, plural, toast,
} from '../core.js';
import { freshness, fieldSummary } from './overview.js';
import { liveTiles } from './dashboard.js';
import { alarmRow } from './alarms.js';

const ID_RE = /^[A-Za-z0-9_.-]{1,64}$/;
let draft = null; // a sensor being created (not on the server yet)

export async function render(el, ctx) {
  if (ctx.params[0]) return detail(el, ctx, ctx.params[0], ctx.params[1]);
  return list(el, ctx);
}

const listFilter = { q: '', status: '' };
async function list(el, ctx) {
  const [sensors, active] = await Promise.all([api('/api/sensors'), api('/api/anomalies?active=1&limit=1000').catch(() => [])]);
  const alarms = {};
  for (const a of active) if (!a.shelved) alarms[a.sensor] = (alarms[a.sensor] || 0) + 1;
  const q = h('input', { type: 'search', placeholder: 'Search by id, name, kind or location', 'aria-label': 'Search sensors', value: listFilter.q });
  const status = h('select', { 'aria-label': 'Status' }, opt('', 'Any status'), opt('good', 'Reporting'), opt('warning', 'Quiet'), opt('critical', 'Silent'), opt('alarm', 'In alarm'));
  status.value = listFilter.status;
  const box = h('div');
  const byId = new Map(sensors.map((s) => [s.id, s]));

  function draw() {
    const t = q.value.trim().toLowerCase(), now = Date.now();
    const rows = sensors
      .filter((s) => !t || `${s.id} ${s.name || ''} ${s.kind || ''} ${s.location || ''}`.toLowerCase().includes(t))
      .filter((s) => !status.value || (status.value === 'alarm' ? alarms[s.id] : freshness(s.lastSeen, now)[1] === status.value))
      .sort((a, b) => a.id.localeCompare(b.id))
      .map((s) => {
        const [label, tone] = freshness(s.lastSeen, now);
        const nf = Object.keys(s.fields || {}).length;
        const calcs = Object.values(s.fields || {}).filter((f) => f.calc).length;
        return h('tr', { class: 'link', onclick: (ev) => { if (!ev.target.closest('a')) location.hash = '#/sensors/' + enc(s.id); } },
          h('td', {}, h('div', { class: 'primary-cell' }, h('a', { href: '#/sensors/' + enc(s.id), text: s.name || s.id }),
            h('small', { text: [s.name && s.name !== s.id ? s.id : null, s.kind, s.location].filter(Boolean).join(' · ') || ' ' }))),
          h('td', {}, fieldSummary(s, 3)),
          h('td', {}, h('span', { class: 'chip', text: plural(nf, 'field') }), calcs ? h('span', { class: 'chip', text: `${calcs} calculated` }) : null, s.oee ? h('span', { class: 'chip', text: 'OEE' }) : null),
          h('td', {}, badge(label, tone), alarms[s.id] ? ' ' : null, alarms[s.id] ? badge(plural(alarms[s.id], 'alarm'), 'critical') : null),
          h('td', { class: 'n', text: ago(s.lastSeen) }));
      });
    box.replaceChildren(card(null, {}, sensors.length
      ? table(['Sensor', 'Latest values', 'Fields', 'Status', { text: 'Last seen', cls: 'n' }], rows, { empty: 'No sensors match.' })
      : empty('No sensors yet', 'Sensors appear by themselves when a device sends its first reading, over MQTT or HTTP. You can also define one ahead of time.')));
  }
  q.oninput = () => { listFilter.q = q.value; draw(); };
  status.onchange = () => { listFilter.status = status.value; draw(); };

  el.append(pageHeader('Sensors', {
    sub: `${plural(sensors.length, 'sensor')} in this tenant`,
    actions: canManage() && button('New sensor', newSensor, { kind: 'primary', ico: 'plus' }),
  }), h('div', { class: 'toolbar' }, h('div', { class: 'search' }, icon('search'), q), status), box);
  draw();

  let dirty = false;
  ctx.onLeave(live.on('reading', (r) => {
    const s = byId.get(r.s);
    if (!s) return;
    Object.assign(s.last ||= {}, r.v); s.lastSeen = r.t; dirty = true;
  }));
  const timer = setInterval(() => { if (dirty && document.activeElement !== q) { dirty = false; draw(); } }, 3000);
  ctx.onLeave(() => clearInterval(timer));
}

async function newSensor() {
  const id = h('input', { placeholder: 'tank-2', pattern: ID_RE.source, spellcheck: 'false' });
  const ok = await modal('New sensor', field('Sensor id', id, { hint: 'Letters, digits, _ . - (up to 64). Devices publish to this id.' }), {
    sub: 'Define a sensor before it reports, to set its units, limits and calculated fields up front.',
    actions: [['Cancel'], ['Continue', () => { if (!ID_RE.test(id.value.trim())) throw new Error('Use letters, digits, _ . - (up to 64).'); }, 'primary']],
  });
  if (!ok) return;
  draft = { id: id.value.trim(), fields: {}, isNew: true };
  location.hash = '#/sensors/' + enc(draft.id) + '/settings';
}

async function detail(el, ctx, id, tab) {
  let s;
  try { s = await api('/api/sensors/' + enc(id)); } catch (e) {
    if (e.status !== 404) throw e;
    if (draft?.id === id && canManage()) s = draft;
    else {
      el.append(pageHeader(id, { back: ['#/sensors', 'Sensors'] }), empty('Sensor not found', `There is no sensor "${id}" in this tenant. It may have been deleted.`, linkButton('All sensors', '#/sensors')));
      return;
    }
  }
  if (!s.isNew) draft = null;
  tab = ['live', 'history', 'alarms', 'settings'].includes(tab) ? tab : s.isNew ? 'settings' : 'live';
  if (tab === 'settings' && !canManage()) tab = 'live';
  ctx.crumbs([[s.name || s.id]]);

  const [label, tone] = s.isNew ? ['Not reporting yet', 'neutral'] : freshness(s.lastSeen);
  const sub = h('span', { class: 'page-sub' }, badge(label, tone),
    h('span', { text: s.isNew ? 'new' : `last reading ${ago(s.lastSeen)}` }),
    s.name && s.name !== s.id && h('span', { class: 'chip mono', text: s.id }),
    s.kind && h('span', { class: 'chip', text: s.kind }), s.location && h('span', { class: 'chip', text: s.location }));
  el.append(pageHeader(s.name || s.id, { back: ['#/sensors', 'Sensors'], sub }));
  el.append(tabs([['live', 'Live'], ['history', 'History'], ['alarms', 'Alarms'], ...(canManage() ? [['settings', 'Settings']] : [])],
    tab, (k) => { location.hash = `#/sensors/${enc(id)}/${k}`; }));
  const body = h('div');
  el.append(body);
  if (tab === 'live') return liveTab(body, ctx, s);
  if (tab === 'history') return historyTab(body, ctx, s);
  if (tab === 'alarms') return alarmsTab(body, ctx, s);
  return settingsTab(body, ctx, s);
}

function liveTab(body, ctx, s) {
  const fields = Object.keys(s.fields || {}).sort();
  if (!fields.length) { body.append(empty('No fields yet', 'Fields appear with the first reading. Define them ahead of time under Settings.')); return; }
  const sensors = { [s.id]: s };
  const tiles = liveTiles(ctx, sensors);
  const grid = h('div', { class: 'fieldcards' });
  for (const f of fields) {
    const def = s.fields[f];
    const isNum = !def.type || def.type === 'number';
    const opts = def.detect?.high != null ? { crit: def.detect.high } : {};
    const t = tiles.add({ id: 'f-' + f, type: isNum ? 'stat' : 'state', sensor: s.id, field: f, title: (def.label || f) + (def.calc ? ' (calculated)' : ''), options: opts });
    grid.append(t.el);
  }
  body.append(grid);
  const numeric = fields.filter((f) => !s.fields[f].type || s.fields[f].type === 'number');
  if (numeric.length) {
    const chart = h('div');
    const pick = h('div', { class: 'toggles', role: 'group', 'aria-label': 'Fields to chart' },
      numeric.map((f, i) => h('label', {}, h('input', { type: 'checkbox', value: f, checked: i === 0 }), s.fields[f].label || f)));
    const draw = () => {
      const boxes = [...pick.querySelectorAll('input')];
      const on = boxes.filter((b) => b.checked);
      for (const b of boxes) b.disabled = !b.checked && on.length >= 4; // at most four series
      const fs = on.map((b) => b.value);
      chart.replaceChildren(tiles.add({ id: 'chart', type: 'line', sensor: s.id, fields: fs.length ? fs : [numeric[0]], options: { points: 600 } }).el);
    };
    pick.onchange = draw;
    body.append(card('Live chart', { sub: 'Recent readings, updating live. Pick up to four fields.', actions: numeric.length > 1 ? pick : null, cls: 'chartcard' }, chart));
    draw();
  }
  if (s.oee) {
    const grid2 = h('div', { class: 'dash', style: 'grid-auto-rows:170px' });
    grid2.append(tiles.add({ id: 'oee', type: 'oee', sensor: s.id, w: 2, h: 2, options: { range: 'shift' } }).el);
    body.append(h('h3', { class: 'section-gap', text: 'OEE' }), grid2);
  }
}

function historyTab(body, ctx, s) {
  const numeric = Object.keys(s.fields || {}).filter((f) => !s.fields[f].type || s.fields[f].type === 'number').sort();
  if (!numeric.length) { body.append(empty('Nothing to chart', 'This sensor has no numeric fields yet.')); return; }
  const tiles = liveTiles(ctx, { [s.id]: s });
  const fieldSel = h('select', { 'aria-label': 'Field' }, numeric.map((f) => opt(f, s.fields[f].label || f)));
  const range = h('select', { 'aria-label': 'Range' }, [['1h', 'Last hour'], ['6h', 'Last 6 hours'], ['24h', 'Last 24 hours'], ['7d', 'Last 7 days'], ['30d', 'Last 30 days']].map(([v, t]) => opt(v, t, v === '24h')));
  const fc = h('select', { 'aria-label': 'Forecast' }, opt('', 'No forecast'), opt('1h', 'Forecast 1 h'), opt('6h', 'Forecast 6 h'), opt('24h', 'Forecast 24 h'));
  const chart = h('div'), stats = h('div', { class: 'dash', style: 'grid-auto-rows:160px' });
  const draw = () => {
    tiles.clear();
    chart.replaceChildren(tiles.add({ id: 'hist', type: 'line', sensor: s.id, fields: [fieldSel.value], options: { range: range.value, forecast: fc.value || undefined } }).el);
    stats.replaceChildren(tiles.add({ id: 'stats', type: 'stats', sensor: s.id, field: fieldSel.value, w: 2, options: { range: range.value } }).el,
      tiles.add({ id: 'eta', type: 'eta', sensor: s.id, field: fieldSel.value, w: 1,
        options: { side: s.fields[fieldSel.value].detect?.high != null ? 'above' : 'below' } }).el);
  };
  fieldSel.onchange = range.onchange = fc.onchange = draw;
  body.append(h('div', { class: 'toolbar' }, fieldSel, range, fc),
    card(null, { cls: 'chartcard' }, chart), stats,
    h('p', { class: 'hint', text: 'Longer ranges come from rollups (min / mean / max per bucket). Export raw data with GET /api/sensors/{id}/history.' }));
  draw();
}

async function alarmsTab(body, ctx, s) {
  const [evs, shelves] = await Promise.all([api(`/api/anomalies?sensor=${enc(s.id)}&from=-30d&limit=300`), api('/api/shelves').catch(() => [])]);
  const mine = shelves.filter((x) => x.sensor === s.id);
  evs.sort((a, b) => (!!a.end - !!b.end) || b.start - a.start);
  body.append(
    mine.length ? card('Shelved', {}, mine.map((x) => h('div', { class: 'shelf' }, h('span', { text: `${x.field || 'all fields'}${x.kind ? ' · ' + x.kind : ''} until ${new Date(x.until).toLocaleString()} — ${x.reason} (${x.by})` })))) : null,
    card('Last 30 days', { sub: 'Range limits and detection are set under Settings.' },
      evs.length ? h('ul', { class: 'list' }, evs.map((a) => alarmRow(a, () => ctx.reload()))) : empty('No alarms', 'This sensor has not raised an alarm in the last 30 days.')));
  const later = () => { if (!document.querySelector('dialog[open]')) ctx.reload(); };
  ctx.onLeave(live.on('anomaly', (a) => { if (a.sensor === s.id) later(); }));
}

const numOrUndef = (input) => { const v = input.value.trim(); return v === '' ? undefined : Number(v); };

function settingsTab(body, ctx, s) {
  const name = h('input', { value: s.name && s.name !== s.id ? s.name : '', placeholder: s.id });
  const kind = h('input', { value: s.kind || '', placeholder: 'tank, pump, meter…' });
  const loc = h('input', { value: s.location || '', placeholder: 'Line 3, roof…' });
  const rows = h('tbody');
  const fieldNames = () => [...rows.querySelectorAll('tr')].map((r) => r.dataset.name).filter(Boolean);

  function fieldRow(fname, f = {}) {
    const isNew = !fname;
    const nameIn = isNew ? h('input', { placeholder: 'field', 'aria-label': 'Field name', spellcheck: 'false', style: 'min-width:110px' }) : h('span', { class: 'fname', text: fname });
    const label = h('input', { value: f.label || '', 'aria-label': 'Label', placeholder: fname || '', style: 'min-width:120px' });
    const unit = h('input', { value: f.unit || '', 'aria-label': 'Unit', style: 'width:64px;min-width:56px' });
    const min = h('input', { type: 'number', step: 'any', value: f.min ?? '', 'aria-label': 'Display min' });
    const max = h('input', { type: 'number', step: 'any', value: f.max ?? '', 'aria-label': 'Display max' });
    const low = h('input', { type: 'number', step: 'any', value: f.detect?.low ?? '', 'aria-label': 'Alarm below' });
    const high = h('input', { type: 'number', step: 'any', value: f.detect?.high ?? '', 'aria-label': 'Alarm above' });
    const on = h('input', { type: 'checkbox', checked: !f.detect?.off, 'aria-label': 'Detection on', title: 'Anomaly detection (spikes, stale data)' });
    const type = h('select', { 'aria-label': 'Source' },
      opt('measured', 'Measured', !f.calc), opt('formula', 'Formula', f.calc?.formula), opt('integral', 'Integral', f.calc?.integrate));
    const formula = h('input', { value: f.calc?.formula || '', placeholder: 'voltage * current / 1000', 'aria-label': 'Formula', class: 'mono', spellcheck: 'false' });
    const integ = h('input', { value: f.calc?.integrate || '', placeholder: 'power_kw', 'aria-label': 'Field to integrate' });
    const per = h('select', { 'aria-label': 'Per' }, ['1h', '1m', '1s'].map((p) => opt(p, 'per ' + p, (f.calc?.per || '1h') === p)));
    const out = h('span', { class: 'hint' });
    const test = button('Test', async () => {
      if (s.isNew) { out.textContent = 'Save the sensor first.'; return; }
      const r = await post('/api/calc/test', { sensor: s.id, formula: formula.value });
      out.textContent = r.error ? r.error : '= ' + r.value;
      out.className = r.error ? 'err' : 'ok';
    }, { kind: 'small' });
    const src = h('div', { class: 'row', style: 'flex-wrap:nowrap' });
    const sync = () => {
      src.replaceChildren(type);
      if (type.value === 'formula') src.append(formula, test);
      if (type.value === 'integral') src.append(integ, per);
    };
    type.onchange = sync; sync();
    const tr = h('tr', {}, h('td', {}, nameIn), h('td', {}, label), h('td', {}, unit), h('td', {}, min), h('td', {}, max),
      h('td', {}, low), h('td', {}, high), h('td', { style: 'text-align:center' }, on), h('td', { style: 'min-width:280px' }, src, out),
      h('td', {}, h('button', { class: 'btn ghost icon small danger', type: 'button', title: 'Remove field', 'aria-label': 'Remove field ' + (fname || ''), onclick: () => tr.remove() }, icon('close'))));
    tr.dataset.name = fname || '';
    if (isNew) nameIn.oninput = () => { tr.dataset.name = nameIn.value.trim(); };
    tr.read = () => {
      const fld = { label: label.value || undefined, unit: unit.value || undefined, min: numOrUndef(min), max: numOrUndef(max), type: f.type };
      const det = { low: numOrUndef(low), high: numOrUndef(high), off: !on.checked || undefined };
      if (det.low !== undefined || det.high !== undefined || det.off) fld.detect = { ...(f.detect || {}), ...det };
      else if (f.detect) fld.detect = { ...f.detect, low: undefined, high: undefined, off: undefined };
      if (type.value === 'formula') fld.calc = { formula: formula.value.trim() };
      if (type.value === 'integral') fld.calc = { integrate: integ.value.trim(), per: per.value, maxGap: f.calc?.maxGap };
      return [tr.dataset.name, fld];
    };
    return tr;
  }
  for (const [fname, f] of Object.entries(s.fields || {}).sort()) rows.append(fieldRow(fname, f));

  const o = s.oee || {};
  const oeeOn = h('input', { type: 'checkbox', checked: !!s.oee });
  const sel = (cur, optional) => {
    const e = h('select', {}, optional ? opt('', '—') : null, fieldNames().map((n) => opt(n, n, n === cur)));
    if (cur && !fieldNames().includes(cur)) e.append(opt(cur, cur, true));
    return e;
  };
  const running = sel(o.running), total = sel(o.total), good = sel(o.good, true), reject = sel(o.reject, true), planned = sel(o.plannedStop, true);
  const ideal = h('input', { type: 'number', step: 'any', min: '0', value: o.idealCycleSec ?? '' });
  const hold = h('input', { value: o.maxHold || '', placeholder: '5m' });
  const oeeBox = h('div', { class: 'form-grid' }, field('Running (on/off field)', running), field('Total parts (counter)', total), field('Good parts', good),
    field('Rejects', reject), field('Planned stop (on/off)', planned), field('Ideal cycle (seconds)', ideal), field('Hold last value for', hold, { hint: 'e.g. 5m' }));
  const syncOee = () => { oeeBox.hidden = !oeeOn.checked; };
  oeeOn.onchange = syncOee; syncOee();
  const err = h('p', { class: 'err', role: 'alert' });

  const save = async () => {
    err.textContent = '';
    const def = { name: name.value.trim() || undefined, kind: kind.value.trim() || undefined, location: loc.value.trim() || undefined, fields: {} };
    for (const tr of rows.querySelectorAll('tr')) {
      const [n, f] = tr.read();
      if (!n) { err.textContent = 'Every field needs a name.'; return; }
      if (def.fields[n]) { err.textContent = `Field ${n} appears twice.`; return; }
      def.fields[n] = f;
    }
    if (oeeOn.checked) {
      if (good.value && reject.value) { err.textContent = 'OEE: give good parts or rejects, not both.'; return; }
      def.oee = { running: running.value, total: total.value, good: good.value || undefined, reject: reject.value || undefined,
        plannedStop: planned.value || undefined, idealCycleSec: Number(ideal.value), maxHold: hold.value || undefined };
    }
    try {
      await put('/api/sensors/' + enc(s.id), def);
      draft = null;
      toast('Saved ' + s.id);
      ctx.reload(); // a new sensor is now on the server: show it as saved
    } catch (e) { err.textContent = e.message; }
  };
  const remove = async () => {
    if (!await confirmDialog(`Delete ${s.id}?`, 'Its definition, limits and live buffers are removed. Stored history stays in the database, and the sensor comes back if a device sends to it again.', { ok: 'Delete sensor', danger: true })) return;
    await del('/api/sensors/' + enc(s.id));
    toast('Deleted ' + s.id);
    location.hash = '#/sensors';
  };

  body.append(
    card('General', {}, h('div', { class: 'form-grid' }, field('Display name', name), field('Kind', kind), field('Location', loc))),
    card('Fields', {
      sub: 'Min and max set the display range. "Alarm below / above" raise range alarms. Detection finds spikes and stale data. A formula uses other fields of this sensor (voltage * current / 1000; comparisons and and/or work too). An integral turns a rate into a total (kW → kWh).',
      actions: button('Add field', () => { const r = fieldRow('', {}); rows.append(r); r.querySelector('input').focus(); }, { kind: 'small', ico: 'plus' }),
    }, h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl fields' },
      h('thead', {}, h('tr', {}, ['Field', 'Label', 'Unit', 'Min', 'Max', 'Alarm below', 'Alarm above', 'Detect', 'Source', ''].map((t) => h('th', { text: t })))),
      rows))),
    card('OEE (machine efficiency)', { sub: 'Availability × performance × quality, from a running signal and part counters. Field lists refresh after saving.' },
      h('div', { class: 'stack' }, field('Track OEE for this sensor', oeeOn, { cls: 'check' }), oeeBox)),
    err,
    h('div', { class: 'row' }, button(s.isNew ? 'Create sensor' : 'Save changes', save, { kind: 'primary' }),
      h('span', { class: 'spacer' }), !s.isNew && button('Delete sensor', remove, { kind: 'danger' })));
}

