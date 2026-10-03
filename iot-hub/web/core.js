// Shared by every page: API client, element builder, dialogs, toasts, the
// live event stream and who the user is. Everything from the server is
// rendered as text (textContent), never as HTML.

export const $ = (id) => document.getElementById(id);
export const enc = encodeURIComponent;

// h('div', {class, text, onclick, ...attrs}, ...children)
export function h(tag, attrs = {}, ...kids) {
  const e = tag.includes(':') ? document.createElementNS('http://www.w3.org/2000/svg', tag.split(':')[1]) : document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v == null) continue;
    if (k === 'class') e.setAttribute('class', v);
    else if (k === 'text') e.textContent = v;
    else if (k.startsWith('on')) e[k] = v;
    else if (k === 'value') e.value = v;
    else if (k === 'checked') e.checked = !!v;
    else if (k === 'dataset') Object.assign(e.dataset, v);
    else if (k === 'style') e.style.cssText = v; // CSSOM: allowed under style-src 'self'
    else e.setAttribute(k, v === true ? '' : v);
  }
  for (const c of kids.flat(Infinity)) if (c != null && c !== false) e.append(c.nodeType ? c : String(c));
  return e;
}

export function safeGet(k) { try { return localStorage.getItem(k); } catch { return null; } }
export function safeSet(k, v) { try { if (v == null) localStorage.removeItem(k); else localStorage.setItem(k, v); } catch { /* unavailable */ } }

// ---- who, which tenant -------------------------------------------------------
export const session = {
  me: { authEnabled: false, identity: { role: 'admin' } },
  tenant: null, // a multi-tenant superadmin's chosen tenant
};
export const role = () => session.me.identity?.role || (session.me.authEnabled ? null : 'admin');
export const isSuper = () => role() === 'superadmin';
export const canManage = () => !session.me.authEnabled || ['admin', 'superadmin'].includes(role());
export const canOperate = () => canManage() || role() === 'operator';
export const multiTenant = () => !!session.me.multiTenant;
// A superadmin with no tenant chosen has nothing tenant-scoped to show.
export const hasTenant = () => !multiTenant() || !isSuper() || !!session.tenant;
export function tenantLabel() {
  if (!multiTenant()) return '';
  if (isSuper()) { const t = (session.me.tenants || []).find((x) => x.id === session.tenant); return t ? (t.name || t.id) : ''; }
  return session.me.tenant?.name || session.me.tenant?.id || '';
}
export const tenantId = () => (isSuper() ? session.tenant : session.me.tenant?.id) || '';

// ---- API ---------------------------------------------------------------------
// The login cookie is HttpOnly; X-Requested-With is the server's CSRF guard.
export class Unauthorized extends Error {}
const onUnauthorized = new Set();
export function whenSignedOut(fn) { onUnauthorized.add(fn); }

export async function api(path, opts = {}) {
  const headers = { 'X-Requested-With': 'iothub', ...(opts.body != null ? { 'Content-Type': 'application/json' } : {}), ...(opts.headers || {}) };
  if (session.tenant && !path.startsWith('/api/admin/')) headers['X-Tenant'] = session.tenant;
  const res = await fetch(path, { ...opts, headers, credentials: 'same-origin' });
  if (res.status === 401) { for (const fn of onUnauthorized) fn(); throw new Unauthorized('Sign in to continue.'); }
  const body = res.status === 204 ? null : await res.json().catch(() => null);
  if (!res.ok) { const e = new Error(String(body?.error || res.statusText || 'Request failed').replace(/^invalid: /, '')); e.status = res.status; throw e; }
  return body;
}
const json = (method) => (path, obj) => api(path, { method, body: JSON.stringify(obj ?? {}) });
export const post = json('POST');
export const put = json('PUT');
export const patch = json('PATCH');
export const del = (path) => api(path, { method: 'DELETE' });

// ---- formatting --------------------------------------------------------------
const dtf = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' });
export const when = (ms) => (ms ? dtf.format(ms) : '—');
export function ago(ms) {
  if (!ms) return 'never';
  const s = (Date.now() - ms) / 1000;
  if (s < 0) return 'just now';
  if (s < 60) return `${Math.max(1, Math.round(s))}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}
export function duration(ms) {
  const s = ms / 1000;
  if (s < 1) return '<1s';
  if (s < 90) return `${Math.round(s)}s`;
  if (s < 5400) return `${Math.round(s / 60)} min`;
  if (s < 172800) return `${Math.round(s / 3600)} h`;
  return `${Math.round(s / 86400)} days`;
}
export function num(v) {
  if (typeof v === 'boolean') return v ? 'true' : 'false';
  if (typeof v !== 'number' || !isFinite(v)) return v == null ? '—' : String(v);
  const a = Math.abs(v);
  if (Number.isInteger(v) && a < 1e5) return v.toLocaleString();
  if (a >= 1e6) return (v / 1e6).toFixed(1) + 'M';
  if (a >= 1e5) return (v / 1e3).toFixed(0) + 'K';
  if (a >= 100) return v.toFixed(0);
  if (a >= 10) return v.toFixed(1);
  return v.toFixed(2);
}
export const bytes = (n) => (n < 1024 ? n + ' B' : n < 1048576 ? (n / 1024).toFixed(1) + ' KB' : n < 1073741824 ? (n / 1048576).toFixed(1) + ' MB' : (n / 1073741824).toFixed(2) + ' GB');
export const plural = (n, one, many = one + 's') => `${n.toLocaleString()} ${n === 1 ? one : many}`;

// ---- icons (24px stroke) -----------------------------------------------------
const PATHS = {
  overview: 'M3 13h8V3H3zM13 21h8V11h-8zM3 21h8v-6H3zM13 3v6h8V3z',
  dashboard: 'M4 19V9M10 19V5M16 19v-7M22 19H2',
  alarm: 'M6 8a6 6 0 1 1 12 0c0 7 3 9 3 9H3s3-2 3-9M10.3 21a1.94 1.94 0 0 0 3.4 0',
  sensor: 'M12 2v4M12 18v4M4.9 4.9l2.8 2.8M16.3 16.3l2.8 2.8M2 12h4M18 12h4M4.9 19.1l2.8-2.8M16.3 7.7l2.8-2.8M12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6',
  device: 'M5 4h14v10H5zM2 20h20M9 14v6M15 14v6',
  jobs: 'M4 6h16M4 12h10M4 18h6M17 15l3 3-3 3',
  bell: 'M22 6l-10 7L2 6M2 6h20v12H2z',
  grafana: 'M3 3v18h18M7 15l4-4 3 3 5-6',
  tenants: 'M3 21V7l9-4 9 4v14M9 21v-6h6v6M8 10h.01M12 10h.01M16 10h.01',
  system: 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z',
  menu: 'M3 6h18M3 12h18M3 18h18',
  command: 'M4 17l6-6-6-6M12 19h8',
  close: 'M18 6 6 18M6 6l12 12',
  sun: 'M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10zM12 1v2M12 21v2M4.2 4.2l1.4 1.4M18.4 18.4l1.4 1.4M1 12h2M21 12h2M4.2 19.8l1.4-1.4M18.4 5.6l1.4-1.4',
  logout: 'M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9',
  plus: 'M12 5v14M5 12h14',
  back: 'M19 12H5M12 19l-7-7 7-7',
  search: 'M11 19a8 8 0 1 0 0-16 8 8 0 0 0 0 16zM21 21l-4.35-4.35',
  external: 'M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6M15 3h6v6M10 14 21 3',
  download: 'M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3',
  key: 'M21 2l-2 2m-7.6 7.6a5.5 5.5 0 1 1-7.78 7.78 5.5 5.5 0 0 1 7.78-7.78zm0 0L15.5 7.5m0 0 3 3L22 7l-3-3m-3.5 3.5L19 4',
  check: 'M20 6 9 17l-5-5',
};
export function icon(name, cls = 'ico') {
  return h('svg:svg', { class: cls, viewBox: '0 0 24 24', 'aria-hidden': 'true', fill: 'none', stroke: 'currentColor', 'stroke-width': '2', 'stroke-linecap': 'round', 'stroke-linejoin': 'round' },
    h('svg:path', { d: PATHS[name] || '' }));
}

// ---- components --------------------------------------------------------------
// Status always carries a label next to its color (never color alone).
export function badge(text, tone = 'neutral') { return h('span', { class: 'badge ' + tone }, h('i'), text); }

export function pageHeader(title, { sub, actions, back } = {}) {
  return h('div', { class: 'page-head' },
    h('div', { class: 'page-title' },
      back && h('a', { class: 'back', href: back[0] }, icon('back'), back[1]),
      h('h1', { text: title }),
      sub && h('p', { class: 'page-sub' }, sub)),
    actions && h('div', { class: 'page-actions' }, actions));
}

export function card(title, { sub, actions, cls } = {}, ...body) {
  return h('section', { class: 'card' + (cls ? ' ' + cls : '') },
    (title || actions) && h('div', { class: 'card-head' },
      h('div', {}, title && h('h2', { text: title }), sub && h('p', { class: 'card-sub' }, sub)),
      actions && h('div', { class: 'card-actions' }, actions)),
    ...body);
}

export function empty(title, text, action) {
  return h('div', { class: 'empty-state' }, h('p', { class: 'empty-title', text: title }), text && h('p', { class: 'empty-text' }, text), action);
}

export function table(headers, rows, { empty: emptyText, cls } = {}) {
  const tb = h('tbody', {}, rows);
  const t = h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' + (cls ? ' ' + cls : '') },
    h('thead', {}, h('tr', {}, headers.map((x) => (typeof x === 'string' ? h('th', { text: x }) : h('th', { class: x.cls, text: x.text }))))), tb));
  if (!rows.length && emptyText) return h('div', {}, t, h('p', { class: 'tbl-empty', text: emptyText }));
  return t;
}

export function field(label, input, { hint, cls } = {}) {
  return h('label', { class: 'field' + (cls ? ' ' + cls : '') }, h('span', { class: 'field-label', text: label }), input, hint && h('span', { class: 'field-hint', text: hint }));
}
export const opt = (value, text, selected) => h('option', { value, selected: selected ? 'selected' : false }, text ?? value);

export function button(label, onclick, { kind = '', ico, title, disabled, type = 'button' } = {}) {
  const b = h('button', { type, class: 'btn ' + kind, title, disabled, 'aria-label': ico && !label ? title : null }, ico && icon(ico), label);
  if (onclick) {
    b.onclick = async (ev) => {
      if (b.dataset.busy) return;
      b.dataset.busy = '1'; b.disabled = true;
      try { await onclick(ev); } catch (e) { if (!(e instanceof Unauthorized)) toast(e.message || String(e), 'bad'); } finally { delete b.dataset.busy; b.disabled = !!disabled; }
    };
  }
  return b;
}
export const linkButton = (label, href, { kind = '', ico, target } = {}) => h('a', { class: 'btn ' + kind, href, target, rel: target ? 'noopener' : null }, ico && icon(ico), label);

export function tabs(items, current, onPick) {
  const bar = h('div', { class: 'seg', role: 'tablist' });
  for (const [k, label, count] of items) {
    bar.append(h('button', { type: 'button', role: 'tab', 'aria-selected': String(k === current), onclick: () => onPick(k) }, label, count != null && h('span', { class: 'count', text: count })));
  }
  return bar;
}

export function meter(value, max, { tone } = {}) {
  const pct = max > 0 ? Math.min(100, (value / max) * 100) : 0;
  const t = tone || (pct >= 90 ? 'critical' : pct >= 75 ? 'warning' : '');
  return h('div', { class: 'meter ' + t, role: 'meter', 'aria-valuenow': value, 'aria-valuemax': max }, h('div', { style: `width:${pct}%` }));
}

// ---- toasts and dialogs ------------------------------------------------------
let toastTimer;
export function toast(msg, tone = '') {
  const t = $('toast');
  t.textContent = msg;
  t.className = 'toast ' + tone;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, tone === 'bad' ? 6000 : 2600);
}
export const fail = (e) => { if (!(e instanceof Unauthorized)) toast(e.message || String(e), 'bad'); };

// modal(title, body, {actions: [[label, fn|null, kind]], wide}) → closes when
// fn resolves without throwing; a thrown error shows inside the dialog.
export function modal(title, body, { actions = [['Close']], wide, sub, ref } = {}) {
  return new Promise((resolve) => {
    const err = h('p', { class: 'err', role: 'alert' });
    const d = h('dialog', { class: 'modal' + (wide ? ' wide' : '') });
    const close = (v) => { d.close(); d.remove(); resolve(v); };
    if (ref) ref.close = close; // lets body buttons close the dialog
    const form = h('form', { method: 'dialog', onsubmit: (ev) => ev.preventDefault() },
      h('div', { class: 'modal-head' }, h('h2', { text: title }), h('button', { type: 'button', class: 'btn ghost icon', 'aria-label': 'Close', onclick: () => close(undefined) }, icon('close'))),
      sub && h('p', { class: 'modal-sub' }, sub),
      h('div', { class: 'modal-body' }, body),
      err,
      h('div', { class: 'modal-actions' }, actions.map(([label, fn, kind], i) => {
        const b = h('button', { type: i === actions.length - 1 && fn ? 'submit' : 'button', class: 'btn ' + (kind || '') }, label);
        b.onclick = async (ev) => {
          ev.preventDefault();
          if (!fn) return close(undefined);
          err.textContent = '';
          b.disabled = true;
          try { const v = await fn(); if (v !== false) close(v ?? true); } catch (e) { err.textContent = e.message || String(e); } finally { b.disabled = false; }
        };
        return b;
      })));
    d.append(form);
    d.addEventListener('cancel', (ev) => { ev.preventDefault(); close(undefined); });
    document.body.append(d);
    d.showModal();
    d.querySelector('input:not([readonly]), textarea, select')?.focus();
  });
}

export function confirmDialog(title, text, { ok = 'Confirm', danger, typed, details } = {}) {
  const input = typed ? h('input', { 'aria-label': 'Type to confirm', autocomplete: 'off', spellcheck: 'false' }) : null;
  const body = h('div', {}, text && h('p', { class: 'modal-text', text }), details, typed && field(`Type ${typed} to confirm`, input));
  return modal(title, body, { actions: [['Cancel'], [ok, () => { if (typed && input.value.trim() !== typed) throw new Error(`Type ${typed} exactly.`); return true; }, danger ? 'danger' : 'primary']] }).then((v) => v === true);
}

// Secrets are shown once, each with a copy button.
const SECRET_LABELS = [['connectionString', 'Connection string'], ['token', 'Token'], ['primaryKey', 'Primary key'], ['secondaryKey', 'Secondary key'],
  ['sas', 'SAS token'], ['login', 'Grafana login'], ['password', 'Password'], ['url', 'Grafana address']];
export function showSecrets(title, r, note = 'Shown only once. Anyone holding these can act as this identity, so store them in a secrets manager.') {
  const rows = [];
  for (const [k, label] of SECRET_LABELS) {
    if (!r[k]) continue;
    const input = h('input', { value: r[k], readonly: true, class: 'mono', 'aria-label': label });
    rows.push(field(label, h('div', { class: 'copy-row' }, input, button('Copy', async () => {
      input.select();
      await navigator.clipboard?.writeText(r[k]);
      toast(label + ' copied');
    }))));
  }
  return modal(title, h('div', { class: 'stack' }, rows), { sub: note, actions: [['I have saved them', () => true, 'primary']] });
}

// ---- live stream -------------------------------------------------------------
// One EventSource for the app; pages subscribe and unsubscribe as they
// mount. Events: 'reading' {s, t, v}, 'anomaly', 'alarms', 'state'.
const subs = new Map();
export const live = {
  state: 'idle',
  on(type, fn) {
    if (!subs.has(type)) subs.set(type, new Set());
    subs.get(type).add(fn);
    return () => subs.get(type)?.delete(fn);
  },
  emit(type, data) { for (const fn of subs.get(type) || []) { try { fn(data); } catch (e) { console.error(e); } } },
};
let es = null;
export function connectLive() {
  es?.close(); es = null;
  if (!hasTenant()) { live.state = 'idle'; live.emit('state', 'idle'); return; }
  es = new EventSource('/api/stream' + (session.tenant ? '?tenant=' + enc(session.tenant) : ''));
  es.onopen = () => { live.state = 'live'; live.emit('state', 'live'); };
  es.onerror = () => { live.state = 'down'; live.emit('state', 'down'); };
  es.onmessage = (ev) => live.emit('reading', JSON.parse(ev.data));
  es.addEventListener('alarms', () => live.emit('alarms'));
  es.addEventListener('anomaly', (ev) => live.emit('anomaly', JSON.parse(ev.data)));
}
export function disconnectLive() { es?.close(); es = null; live.state = 'idle'; live.emit('state', 'idle'); }

// ---- alarm verdicts (shared by the alarms page and tiles) ---------------------
// Where an alarm's subject lives: a device for "overdue" alarms (admins
// see its page, operators its commands), else the sensor.
export const alarmHref = (a) => (a.kind === 'overdue' ? (canManage() ? '#/devices/' + enc(a.sensor) : '#/commands') : '#/sensors/' + enc(a.sensor));

export const VERDICTS = [['confirmed', 'Confirmed problem'], ['false_alarm', 'False alarm'], ['expected', 'Expected (maintenance, changeover)']];
export const verdictLabel = Object.fromEntries(VERDICTS.map(([k, v]) => [k, v.split(' (')[0]]));
