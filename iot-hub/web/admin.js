// Settings page: sensors (fields, limits, detection, calculated fields,
// OEE), devices and their credentials, alert targets and escalation,
// stream jobs, and tenants (superadmin). Everything from the server is
// rendered as text, never as HTML.

const $ = (id) => document.getElementById(id);
const view = $('view');

function h(tag, attrs = {}, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === false || v == null) continue;
    if (k === 'class') e.className = v;
    else if (k === 'text') e.textContent = v;
    else if (k.startsWith('on')) e[k] = v;
    else if (k === 'value') e.value = v;
    else if (k === 'checked') e.checked = !!v;
    else e.setAttribute(k, v === true ? '' : v);
  }
  for (const c of kids.flat()) if (c != null && c !== false) e.append(c.nodeType ? c : String(c));
  return e;
}
const field = (label, input, cls) => h('label', { class: cls }, label, input);
const opt = (value, text, selected) => h('option', { value, selected: selected ? 'selected' : false }, text ?? value);
function safeGet(k) { try { return localStorage.getItem(k); } catch { return null; } }
function safeSet(k, v) { try { localStorage.setItem(k, v); } catch { /* unavailable */ } }

let me = {};
let tenant = null;

async function api(path, opts = {}) {
  const headers = { 'X-Requested-With': 'iothub', ...(opts.body ? { 'Content-Type': 'application/json' } : {}), ...(opts.headers || {}) };
  if (tenant && !path.startsWith('/api/admin/')) headers['X-Tenant'] = tenant;
  const res = await fetch(path, { ...opts, headers, credentials: 'same-origin' });
  if (res.status === 401) { location.href = './'; throw new Error('sign in on the dashboard first'); }
  const body = res.status === 204 ? null : await res.json().catch(() => null);
  if (!res.ok) throw new Error(body?.error || res.statusText);
  return body;
}
const put = (path, obj) => api(path, { method: 'PUT', body: JSON.stringify(obj) });
const post = (path, obj) => api(path, { method: 'POST', body: JSON.stringify(obj ?? {}) });
const patch = (path, obj) => api(path, { method: 'PATCH', body: JSON.stringify(obj) });
const del = (path) => api(path, { method: 'DELETE' });

let toastTimer;
function toast(msg, bad) {
  const t = $('toast');
  t.textContent = msg;
  t.className = bad ? 'bad' : '';
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, bad ? 6000 : 2500);
}
const fail = (e) => toast(e.message || String(e), true);
const isSuper = () => me.identity?.role === 'superadmin';
const enc = encodeURIComponent;
const when = (ms) => (ms ? new Date(ms).toLocaleString() : '—');

// ---- tabs -------------------------------------------------------------------
const tabs = { sensors: sensorsTab, devices: devicesTab, alerts: alertsTab, jobs: jobsTab, grafana: grafanaTab, tenants: tenantsTab };
function show(name) {
  if (!tabs[name]) name = 'sensors';
  for (const b of document.querySelectorAll('.tabs button')) b.setAttribute('aria-selected', b.dataset.tab === name);
  view.replaceChildren(h('p', { class: 'hint', text: 'Loading…' }));
  tabs[name]().catch((e) => view.replaceChildren(h('p', { class: 'err', text: e.message })));
}
for (const b of document.querySelectorAll('.tabs button')) b.onclick = () => { location.hash = b.dataset.tab; };
addEventListener('hashchange', () => show(location.hash.slice(1)));

// ---- sensors ---------------------------------------------------------------
async function sensorsTab() {
  const list = await api('/api/sensors');
  const tbody = h('tbody');
  for (const s of list.sort((a, b) => a.id.localeCompare(b.id))) {
    const fields = Object.entries(s.fields || {});
    const calcs = fields.filter(([, f]) => f.calc).length;
    tbody.append(h('tr', {},
      h('td', {}, h('b', { text: s.id })), h('td', { text: s.name || '' }),
      h('td', { class: 'n', text: fields.length }), h('td', { class: 'n', text: calcs || '' }),
      h('td', {}, s.oee ? h('span', { class: 'pill on', text: 'OEE' }) : ''),
      h('td', { text: when(s.lastSeen) }),
      h('td', {}, h('button', { class: 'small', onclick: () => editSensor(s) }, 'Edit'))));
  }
  const id = h('input', { placeholder: 'new-sensor-id', pattern: '[A-Za-z0-9_.-]{1,64}', 'aria-label': 'New sensor id' });
  view.replaceChildren(h('section', { class: 'card' },
    h('h2', { text: 'Sensors' }),
    h('p', { class: 'sub', text: 'Sensors also appear by themselves when data arrives. Edit one to set units, limits, anomaly detection, calculated fields and OEE.' }),
    h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' },
      h('thead', {}, h('tr', {}, ...['Id', 'Name', 'Fields', 'Calculated', '', 'Last seen', ''].map((t) => h('th', { text: t })))), tbody)),
    h('div', { class: 'row' }, id, h('button', { onclick: () => {
      if (!/^[A-Za-z0-9_.-]{1,64}$/.test(id.value)) return toast('Id: letters, digits, _ . - (up to 64)', true);
      editSensor({ id: id.value, fields: {}, isNew: true });
    } }, 'New sensor'))));
}

function num(input) { const v = input.value.trim(); return v === '' ? undefined : Number(v); }

function editSensor(s) {
  const err = h('p', { class: 'err', role: 'alert' });
  const name = h('input', { value: s.name || '' }), kind = h('input', { value: s.kind || '' }), loc = h('input', { value: s.location || '' });
  const rows = h('tbody');
  const fieldNames = () => [...rows.querySelectorAll('tr')].map((r) => r.dataset.name).filter(Boolean);

  function fieldRow(fname, f = {}) {
    const isNew = !fname;
    const nameIn = h('input', { value: fname || '', placeholder: 'field', 'aria-label': 'Field name', readonly: !isNew });
    const label = h('input', { value: f.label || '', 'aria-label': 'Label' });
    const unit = h('input', { value: f.unit || '', size: 4, 'aria-label': 'Unit' });
    const min = h('input', { type: 'number', step: 'any', value: f.min ?? '', 'aria-label': 'Display min' });
    const max = h('input', { type: 'number', step: 'any', value: f.max ?? '', 'aria-label': 'Display max' });
    const low = h('input', { type: 'number', step: 'any', value: f.detect?.low ?? '', 'aria-label': 'Alarm below' });
    const high = h('input', { type: 'number', step: 'any', value: f.detect?.high ?? '', 'aria-label': 'Alarm above' });
    const off = h('input', { type: 'checkbox', checked: f.detect?.off, 'aria-label': 'Detection off' });
    const type = h('select', { 'aria-label': 'Source' },
      opt('measured', 'measured', !f.calc), opt('formula', 'formula', f.calc?.formula), opt('integral', 'integral', f.calc?.integrate));
    const formula = h('input', { value: f.calc?.formula || '', placeholder: 'voltage * current / 1000', 'aria-label': 'Formula' });
    const integ = h('input', { value: f.calc?.integrate || '', placeholder: 'power_kw', 'aria-label': 'Integrate field', size: 8 });
    const per = h('select', { 'aria-label': 'Per' }, ...['1h', '1m', '1s'].map((p) => opt(p, 'per ' + p, (f.calc?.per || '1h') === p)));
    const out = h('span', { class: 'hint' });
    const testBtn = h('button', { class: 'small', type: 'button', onclick: async () => {
      if (s.isNew) { out.textContent = 'save the sensor first'; return; }
      try {
        const r = await post('/api/calc/test', { sensor: s.id, formula: formula.value });
        out.textContent = r.error ? r.error : '= ' + r.value;
        out.className = r.error ? 'err' : 'ok';
      } catch (e) { out.textContent = e.message; out.className = 'err'; }
    } }, 'Test');
    const src = h('div', { class: 'row' });
    const syncSrc = () => {
      src.replaceChildren(type);
      if (type.value === 'formula') src.append(formula, testBtn, out);
      if (type.value === 'integral') src.append(integ, per);
    };
    type.onchange = syncSrc;
    syncSrc();
    const tr = h('tr', {}, h('td', {}, nameIn), h('td', {}, label), h('td', {}, unit), h('td', {}, min), h('td', {}, max),
      h('td', {}, low), h('td', {}, high), h('td', {}, off), h('td', {}, src),
      h('td', {}, h('button', { class: 'small danger', type: 'button', title: 'Remove field', onclick: () => tr.remove() }, '✕')));
    tr.dataset.name = fname || '';
    nameIn.oninput = () => { tr.dataset.name = nameIn.value.trim(); };
    tr.read = () => {
      const fld = { label: label.value || undefined, unit: unit.value || undefined, min: num(min), max: num(max), type: f.type };
      const det = { low: num(low), high: num(high), off: off.checked || undefined };
      if (det.low !== undefined || det.high !== undefined || det.off) fld.detect = { ...(f.detect || {}), ...det };
      if (type.value === 'formula') fld.calc = { formula: formula.value.trim() };
      if (type.value === 'integral') fld.calc = { integrate: integ.value.trim(), per: per.value, maxGap: f.calc?.maxGap };
      return [tr.dataset.name, fld];
    };
    return tr;
  }
  for (const [fname, f] of Object.entries(s.fields || {}).sort()) rows.append(fieldRow(fname, f));

  // OEE
  const o = s.oee || {};
  const oeeOn = h('input', { type: 'checkbox', checked: !!s.oee });
  const sel = (cur, optional) => {
    const e = h('select', {}, optional ? opt('', '—') : null, ...fieldNames().map((n) => opt(n, n, n === cur)));
    if (cur && !fieldNames().includes(cur)) e.append(opt(cur, cur, true));
    return e;
  };
  const running = sel(o.running), total = sel(o.total), good = sel(o.good, true), reject = sel(o.reject, true), planned = sel(o.plannedStop, true);
  const ideal = h('input', { type: 'number', step: 'any', min: '0', value: o.idealCycleSec ?? '' });
  const hold = h('input', { value: o.maxHold || '', placeholder: '5m', size: 5 });

  const save = async () => {
    err.textContent = '';
    const def = { name: name.value || undefined, kind: kind.value || undefined, location: loc.value || undefined, fields: {} };
    for (const tr of rows.querySelectorAll('tr')) {
      const [n, f] = tr.read();
      if (!n) { err.textContent = 'Every field needs a name.'; return; }
      if (def.fields[n]) { err.textContent = `Field ${n} appears twice.`; return; }
      def.fields[n] = f;
    }
    if (oeeOn.checked) {
      def.oee = { running: running.value, total: total.value, good: good.value || undefined, reject: reject.value || undefined,
        plannedStop: planned.value || undefined, idealCycleSec: Number(ideal.value), maxHold: hold.value || undefined };
    }
    try {
      await put('/api/sensors/' + enc(s.id), def);
      toast('Saved ' + s.id);
      sensorsTab();
    } catch (e) { err.textContent = e.message; }
  };
  const remove = async () => {
    if (!confirm(`Delete sensor ${s.id}? Its definition and live buffers go; stored history stays in the database.`)) return;
    try { await del('/api/sensors/' + enc(s.id)); toast('Deleted'); sensorsTab(); } catch (e) { fail(e); }
  };

  view.replaceChildren(h('section', { class: 'card' },
    h('h2', { text: (s.isNew ? 'New sensor ' : 'Sensor ') + s.id }),
    h('div', { class: 'row' }, field('Name', name), field('Kind', kind), field('Location', loc)),
    h('h3', { text: 'Fields' }),
    h('p', { class: 'hint', text: 'Min/max set the display range. "Alarm below/above" raise range alarms. A formula uses other fields of this sensor, e.g. voltage * current / 1000; comparisons and and/or work too. An integral turns a rate into a total (kW → kWh per 1h).' }),
    h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' },
      h('thead', {}, h('tr', {}, ...['Field', 'Label', 'Unit', 'Min', 'Max', 'Alarm below', 'Alarm above', 'Detection off', 'Source', ''].map((t) => h('th', { text: t })))),
      rows)),
    h('div', { class: 'row' }, h('button', { type: 'button', onclick: () => rows.append(fieldRow('', {})) }, 'Add field')),
    h('h3', { text: 'OEE (machine efficiency)' }),
    h('div', { class: 'row' }, field('Track OEE', oeeOn, 'check')),
    h('div', { class: 'row' }, field('Running (bool)', running), field('Total parts (counter)', total), field('Good parts', good),
      field('Rejects', reject), field('Planned stop (bool)', planned), field('Ideal cycle (s)', ideal), field('Max hold', hold)),
    h('p', { class: 'hint', text: 'Give either good or rejects, not both. Field lists refresh when you save.' }),
    err,
    h('div', { class: 'row' }, h('button', { class: 'primary', onclick: save }, 'Save'),
      h('button', { onclick: sensorsTab }, 'Cancel'), !s.isNew && h('button', { class: 'danger', onclick: remove }, 'Delete sensor'))));
}

// ---- devices ----------------------------------------------------------------
function showSecrets(title, r) {
  const body = $('secret-body');
  body.replaceChildren(h('p', { class: 'sub', text: title }));
  for (const [k, label] of [['token', 'Token'], ['connectionString', 'Connection string'], ['primaryKey', 'Primary key'],
    ['secondaryKey', 'Secondary key'], ['sas', 'SAS token'], ['login', 'Grafana login'], ['password', 'Password'], ['url', 'Grafana']]) {
    if (!r[k]) continue;
    const input = h('input', { value: r[k], readonly: true, 'aria-label': label });
    body.append(h('div', { class: 'secret-row' }, h('label', { text: label }), h('div', { class: 'copy' }, input,
      h('button', { type: 'button', onclick: () => { input.select(); navigator.clipboard?.writeText(r[k]).then(() => toast('Copied')); } }, 'Copy'))));
  }
  $('secret').showModal();
}

async function devicesTab() {
  const list = await api('/api/devices');
  const locked = !isSuper() && me.multiTenant && !me.deviceSelfService;
  const tbody = h('tbody');
  const act = (label, fn, cls) => h('button', { class: 'small ' + (cls || ''), disabled: locked, onclick: async () => { try { await fn(); } catch (e) { fail(e); } } }, label);
  for (const d of list) {
    const p = '/api/devices/' + enc(d.id);
    tbody.append(h('tr', {},
      h('td', {}, h('b', { text: d.id }), d.note ? h('div', { class: 'hint', text: d.note }) : null),
      h('td', { text: d.role }), h('td', { text: (d.sensors || []).join(', ') }),
      h('td', {}, d.token ? h('span', { class: 'pill on', text: 'token' }) : null, d.keys ? h('span', { class: 'pill on', text: 'keys' }) : null,
        d.disabled ? h('span', { class: 'pill bad', text: 'disabled' }) : null),
      h('td', { text: d.expires ? when(d.expires) : '' }),
      h('td', {}, h('div', { class: 'row' },
        (d.role === 'device' || d.role === 'service') && h('button', { class: 'small', onclick: () => twinPanel(d.id).catch(fail) }, 'Twin'),
        d.keys && act('Keys', async () => showSecrets('Keys for ' + d.id, await api(p + '/keys'))),
        d.keys && act('SAS 24h', async () => showSecrets('SAS token for ' + d.id + ' (24 h)', await post(p + '/sas', { ttl: '24h' }))),
        act(d.keys ? 'Rotate primary' : 'Add keys', async () => {
          if (!confirm('The current primary key stops working now (the secondary keeps working).')) return;
          showSecrets('New primary key for ' + d.id, await post(p + '/rotate', { which: 'primary' }));
        }),
        d.token && act('New token', async () => {
          if (!confirm('The current token stops working now.')) return;
          showSecrets('New token for ' + d.id, await post(p + '/rotate', { which: 'token' }));
        }),
        act(d.disabled ? 'Enable' : 'Disable', async () => { await patch(p, { disabled: !d.disabled }); devicesTab(); }),
        act('Delete', async () => {
          if (!confirm(`Revoke and delete ${d.id}? It is disconnected at once.`)) return;
          await del(p); devicesTab();
        }, 'danger')))));
  }
  const id = h('input', { placeholder: 'pump-1', 'aria-label': 'Id' });
  const role = h('select', { 'aria-label': 'Role' }, ...['device', 'service', 'viewer', 'operator', 'admin'].map((r) => opt(r)));
  const sensors = h('input', { placeholder: 'pump-*, line3-*', 'aria-label': 'Sensors' });
  const authSel = h('select', { 'aria-label': 'Credential' }, opt('keys', 'keys (SAS, rotatable)'), opt('token', 'token'), opt('both', 'both'));
  const expires = h('input', { type: 'date', 'aria-label': 'Token expires' });
  const note = h('input', { placeholder: 'roof, line 3…', 'aria-label': 'Note' });
  const err = h('p', { class: 'err', role: 'alert' });
  const create = async () => {
    err.textContent = '';
    const body = { id: id.value.trim(), role: role.value, auth: authSel.value, note: note.value || undefined,
      sensors: sensors.value.split(',').map((x) => x.trim()).filter(Boolean) };
    if (expires.value) body.expires = new Date(expires.value + 'T23:59:59').getTime();
    try {
      const r = await post('/api/devices', body);
      showSecrets(`Credentials for ${r.id} in tenant ${r.tenant}`, r);
      devicesTab();
    } catch (e) { err.textContent = e.message; }
  };
  view.replaceChildren(...[
    h('section', { class: 'card' },
      h('h2', { text: 'Devices and access' }),
      h('p', { class: 'sub', text: locked
        ? 'Device credentials for this tenant are issued by the platform operator. Ask them for a connection string.'
        : 'Every device, service and person gets its own credential. Keys sign short-lived SAS tokens (as in Azure IoT Hub); rotate one key while devices use the other.' }),
      h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' },
        h('thead', {}, h('tr', {}, ...['Id', 'Role', 'Sensors', 'Credential', 'Token expires', ''].map((t) => h('th', { text: t })))), tbody))),
    !locked && h('section', { class: 'card' },
      h('h2', { text: 'Add' }),
      h('div', { class: 'row' }, field('Id', id), field('Role', role), field('Sensors (globs)', sensors), field('Credential', authSel),
        field('Token expires (optional)', expires), field('Note', note)),
      h('p', { class: 'hint', text: 'Devices and services need sensor patterns; they can only touch those sensors.' }),
      err, h('button', { class: 'primary', onclick: create }, 'Create')),
  ].filter(Boolean));
}

// ---- device twin, direct methods, cloud-to-device messages -------------------
const pretty = (o) => JSON.stringify(o, null, 2);
const strip = (o) => Object.fromEntries(Object.entries(o || {}).filter(([k]) => !k.startsWith('$')));

async function twinPanel(id) {
  const p = '/api/twins/' + enc(id);
  const tw = await api(p);
  const tags = h('textarea', { 'aria-label': 'Tags (JSON)', spellcheck: 'false' });
  const desired = h('textarea', { 'aria-label': 'Desired properties (JSON)', spellcheck: 'false' });
  tags.value = pretty(tw.tags || {});
  desired.value = pretty(strip(tw.properties.desired));
  const err = h('p', { class: 'err', role: 'alert' });
  const save = async () => {
    err.textContent = '';
    let t, d;
    try { t = JSON.parse(tags.value || '{}'); d = JSON.parse(desired.value || '{}'); } catch (e) { err.textContent = 'Not valid JSON: ' + e.message; return; }
    // A merge patch: keys removed in the editor become null (deleted).
    const del = (before, after) => Object.fromEntries(Object.keys(before).filter((k) => !(k in after)).map((k) => [k, null]));
    const body = { tags: { ...del(tw.tags || {}, t), ...t }, properties: { desired: { ...del(strip(tw.properties.desired), d), ...d } } };
    try {
      await api(p, { method: 'PATCH', body: JSON.stringify(body), headers: { 'If-Match': tw.etag } });
      toast('Twin saved; desired changes sent to ' + id);
      twinPanel(id);
    } catch (e) { err.textContent = e.message + (e.message.includes('etag') ? ' — reopen to see the latest.' : ''); }
  };

  const mname = h('input', { placeholder: 'reboot', 'aria-label': 'Method name' });
  const mpay = h('input', { placeholder: '{"delay": 5}', 'aria-label': 'Method payload (JSON)', class: 'wide' });
  const mto = h('select', { 'aria-label': 'Timeout' }, ...['10s', '30s', '60s', '300s'].map((x) => opt(x, 'wait ' + x, x === '30s')));
  const mout = h('pre', { class: 'hint' });
  const invoke = async () => {
    if (!mname.value.trim()) { mout.textContent = 'Enter a method name.'; return; }
    mout.textContent = 'Calling…';
    try {
      const r = await post(`/api/devices/${enc(id)}/methods/${enc(mname.value.trim())}?timeout=${mto.value}`, mpay.value.trim() ? JSON.parse(mpay.value) : null);
      mout.textContent = `status ${r.status}\n` + pretty(r.payload);
    } catch (e) { mout.textContent = e.message; }
  };

  const body = h('input', { placeholder: '{"cmd": "close-valve"}', 'aria-label': 'Message body', class: 'wide' });
  const ttl = h('select', { 'aria-label': 'Time to live' }, ...['10m', '1h', '24h', '48h'].map((x) => opt(x, 'keep ' + x, x === '1h')));
  const msgs = h('tbody');
  const loadMsgs = async () => {
    const list = await api(`/api/devices/${enc(id)}/messages`);
    msgs.replaceChildren(...list.slice(0, 20).map((m) => h('tr', {},
      h('td', { text: when(m.created) }), h('td', { text: JSON.stringify(m.body) }),
      h('td', {}, h('span', { class: 'pill ' + ({ completed: 'on', deadlettered: 'bad', rejected: 'bad', expired: 'bad' }[m.status] || ''), text: m.status })),
      h('td', { class: 'n', text: m.deliveries }), h('td', { text: when(m.expires) }))));
  };
  const send = async () => {
    let b = body.value.trim();
    try { JSON.parse(b); } catch { b = JSON.stringify(b); } // plain text is sent as a JSON string
    try { await api(`/api/devices/${enc(id)}/messages?ttl=${ttl.value}`, { method: 'POST', body: b }); toast('Queued for ' + id); loadMsgs(); } catch (e) { fail(e); }
  };

  view.replaceChildren(
    h('section', { class: 'card' },
      h('h2', { text: 'Device twin: ' + id }),
      h('p', { class: 'sub' }, h('span', { class: 'pill ' + (tw.connectionState === 'Connected' ? 'on' : ''), text: tw.connectionState }),
        ` version ${tw.version} · desired v${tw.properties.desired.$version} · reported v${tw.properties.reported.$version} · last activity ${when(tw.lastActivityTime)}`),
      h('div', { class: 'grid2' },
        h('div', {}, h('h3', { text: 'Tags (cloud only: grouping and queries)' }), tags),
        h('div', {}, h('h3', { text: 'Desired properties (sent to the device)' }), desired)),
      h('h3', { text: 'Reported by the device' }), h('pre', { class: 'hint', text: pretty(strip(tw.properties.reported)) }),
      err,
      h('div', { class: 'row' }, h('button', { class: 'primary', onclick: save }, 'Save twin'), h('button', { onclick: devicesTab }, 'Back to devices'))),
    h('section', { class: 'card' },
      h('h2', { text: 'Direct method' }),
      h('p', { class: 'sub', text: 'A call the device answers now. It must be connected and subscribed to devices/' + id + '/methods/#.' }),
      h('div', { class: 'row' }, field('Method', mname), field('Payload', mpay), field('Timeout', mto), h('button', { onclick: invoke }, 'Invoke')),
      mout),
    h('section', { class: 'card' },
      h('h2', { text: 'Cloud-to-device messages' }),
      h('p', { class: 'sub', text: 'Queued until the device listens (MQTT devices/' + id + '/messages/#, or HTTP polling), redelivered until it completes them.' }),
      h('div', { class: 'row' }, field('Body', body), field('Keep', ttl), h('button', { onclick: send }, 'Send')),
      h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' },
        h('thead', {}, h('tr', {}, ...['Sent', 'Body', 'Status', 'Deliveries', 'Expires'].map((t) => h('th', { text: t })))), msgs))));
  loadMsgs().catch(fail);
}

// ---- alerts & reports -------------------------------------------------------
async function alertsTab() {
  const { settings: st, recentEscalations } = await api('/api/settings');
  st.notify.targets ||= []; st.notify.kinds ||= []; st.reports.targets ||= []; st.escalation.levels ||= []; st.shifts ||= []; st.targets ||= [];
  const ids = st.targets.map((t) => t.id);
  const tbody = h('tbody');
  for (const t of st.targets) {
    tbody.append(h('tr', {}, h('td', {}, h('b', { text: t.id })), h('td', { text: t.display }),
      h('td', { class: 'n', text: t.sent }), h('td', { class: 'n', text: t.failed }), h('td', { class: 'err', text: t.lastError || '' }),
      h('td', {}, h('div', { class: 'row' },
        h('button', { class: 'small', onclick: async () => {
          try { await post('/api/settings/targets/' + enc(t.id) + '/test'); toast('Test sent to ' + t.id); } catch (e) { fail(e); }
        } }, 'Test'),
        h('button', { class: 'small danger', onclick: async () => {
          try { await del('/api/settings/targets/' + enc(t.id)); alertsTab(); } catch (e) { fail(e); }
        } }, 'Remove')))));
  }
  const tid = h('input', { placeholder: 'ops-teams', 'aria-label': 'Target id' });
  const tkind = h('select', { 'aria-label': 'Kind' }, ...['teams', 'slack', 'email', 'webhook', 'discord', 'telegram'].map((k) => opt(k)));
  const turl = h('input', { placeholder: 'https://… or smtp://user:pass@host:587?from=…&to=…', 'aria-label': 'URL', class: 'wide' });
  const terr = h('p', { class: 'err', role: 'alert' });

  const picks = (selected) => {
    const box = h('div', { class: 'row' });
    for (const id of ids) box.append(h('label', { class: 'check' }, h('input', { type: 'checkbox', value: id, checked: selected?.includes(id) }), id));
    if (!ids.length) box.append(h('span', { class: 'hint', text: 'Add a target first.' }));
    box.read = () => [...box.querySelectorAll('input:checked')].map((c) => c.value);
    return box;
  };
  const nTargets = picks(st.notify.targets);
  const kinds = h('div', { class: 'row' });
  for (const k of ['range', 'spike', 'stale', 'rule']) kinds.append(h('label', { class: 'check' }, h('input', { type: 'checkbox', value: k, checked: !st.notify.kinds.length || st.notify.kinds.includes(k) }), k));
  const resolved = h('input', { type: 'checkbox', checked: st.notify.resolved });
  const cooldown = h('input', { type: 'number', min: '0', value: st.notify.cooldownMin || 10, size: 4 });
  const levels = h('div');
  const levelRow = (l = { afterMin: '', targets: [] }) => {
    const after = h('input', { type: 'number', min: '1', value: l.afterMin, 'aria-label': 'After minutes', size: 4 });
    const tg = picks(l.targets);
    const row = h('div', { class: 'row' }, field('After (min)', after), tg, h('button', { class: 'small danger', type: 'button', onclick: () => row.remove() }, '✕'));
    row.read = () => ({ afterMin: Number(after.value), targets: tg.read() });
    return row;
  };
  for (const l of st.escalation.levels) levels.append(levelRow(l));
  const repeat = h('input', { type: 'number', min: '0', value: st.escalation.repeatMin || 0, size: 4 });
  const rTargets = picks(st.reports.targets);
  const shifts = h('input', { value: st.shifts.join(', '), placeholder: '06:00, 14:00, 22:00' });
  const tz = h('input', { value: st.timeZone || '', placeholder: 'Africa/Harare' });
  const secret = h('input', { type: 'password', placeholder: st.webhookSecretSet ? '(set — leave empty to keep)' : '(none)', autocomplete: 'new-password' });
  const err = h('p', { class: 'err', role: 'alert' });

  const saveAll = async () => {
    err.textContent = '';
    const allKinds = [...kinds.querySelectorAll('input:checked')].map((c) => c.value);
    const body = {
      notify: { targets: nTargets.read(), kinds: allKinds.length === 4 ? [] : allKinds, resolved: resolved.checked, cooldownMin: Number(cooldown.value) || 0 },
      escalation: { levels: [...levels.children].map((r) => r.read()), repeatMin: Number(repeat.value) || 0 },
      reports: { targets: rTargets.read() },
      shifts: shifts.value.split(',').map((x) => x.trim()).filter(Boolean),
      timeZone: tz.value.trim(),
    };
    if (secret.value) body.webhookSecret = secret.value;
    try { await put('/api/settings', body); toast('Settings saved'); alertsTab(); } catch (e) { err.textContent = e.message; }
  };

  view.replaceChildren(
    h('section', { class: 'card' },
      h('h2', { text: 'Targets' }),
      h('p', { class: 'sub', text: 'Name each destination once; its URL holds a secret and is never shown again.' }),
      h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' },
        h('thead', {}, h('tr', {}, ...['Id', 'Destination', 'Sent', 'Failed', 'Last error', ''].map((t) => h('th', { text: t })))), tbody)),
      h('div', { class: 'row' }, field('Id', tid), field('Kind', tkind), field('URL', turl),
        h('button', { onclick: async () => {
          terr.textContent = '';
          try { await put('/api/settings/targets/' + enc(tid.value.trim()), { spec: tkind.value + '=' + turl.value.trim() }); alertsTab(); } catch (e) { terr.textContent = e.message; }
        } }, 'Add target')), terr),
    h('section', { class: 'card' },
      h('h2', { text: 'Alarms' }),
      h('h3', { text: 'Notify when an alarm opens' }), nTargets,
      h('div', { class: 'row' }, h('span', { class: 'hint', text: 'Kinds:' }), kinds),
      h('div', { class: 'row' }, field('Also when it clears', resolved, 'check'), field('Cooldown (min)', cooldown)),
      h('h3', { text: 'Escalate while nobody acknowledges' }),
      h('p', { class: 'hint', text: 'Each level notifies its targets once its delay passes; acknowledging or shelving stops it.' }),
      levels, h('div', { class: 'row' }, h('button', { type: 'button', onclick: () => levels.append(levelRow()) }, 'Add level'), field('Repeat last level every (min, 0 = no)', repeat)),
      recentEscalations?.length ? h('p', { class: 'hint', text: `${recentEscalations.length} escalation(s) sent recently; latest ${when(recentEscalations.at(-1).at)}.` }) : null),
    h('section', { class: 'card' },
      h('h2', { text: 'Shift reports and plant time' }),
      h('h3', { text: 'Send the report at each shift change to' }), rTargets,
      h('div', { class: 'row' }, field('Shift starts', shifts), field('Time zone', tz), field('Webhook signing secret', secret))),
    h('section', { class: 'card' }, err, h('button', { class: 'primary', onclick: saveAll }, 'Save alarms and reports')));
}

// ---- stream jobs ------------------------------------------------------------
const exampleQuery = 'SELECT avg(level) AS level_avg\nINTO [avg-{sensor}]\nFROM [tank-*]\nGROUP BY sensor, TumblingWindow(minute, 5)';

async function jobsTab(edit) {
  const list = await api('/api/jobs');
  const tbody = h('tbody');
  for (const j of list) {
    tbody.append(h('tr', {},
      h('td', {}, h('b', { text: j.id }), j.error ? h('div', { class: 'err', text: j.error }) : null),
      h('td', {}, h('span', { class: 'pill ' + (j.enabled ? 'on' : ''), text: j.enabled ? 'running' : 'stopped' })),
      ...['in', 'filtered', 'out', 'late', 'dropped', 'errors'].map((k) => h('td', { class: 'n', text: j[k] })),
      h('td', { text: j.lastOutput ? `${j.lastOutput.group ?? ''} ${j.lastOutput.value}` : '' }),
      h('td', { class: 'err', text: j.lastError || '' }),
      h('td', {}, h('button', { class: 'small', onclick: () => jobsTab(j) }, 'Edit'))));
  }
  const cur = edit || { id: '', query: exampleQuery, enabled: true, lateness: '' };
  const id = h('input', { value: cur.id, placeholder: 'tank-avg', readonly: !!edit, 'aria-label': 'Job id' });
  const q = h('textarea', { 'aria-label': 'Query', spellcheck: 'false' });
  q.value = cur.query;
  const late = h('input', { value: cur.lateness || '', placeholder: '5s', size: 5 });
  const on = h('input', { type: 'checkbox', checked: cur.enabled });
  const from = h('select', {}, ...['-1h', '-6h', '-24h', '-7d'].map((f) => opt(f, 'last ' + f.slice(1), f === '-6h')));
  const err = h('p', { class: 'err', role: 'alert' });
  const result = h('div');
  const test = async () => {
    err.textContent = '';
    result.replaceChildren(h('p', { class: 'hint', text: 'Running over history…' }));
    try {
      const r = await post('/api/jobs/test?from=' + enc(from.value), { query: q.value, lateness: late.value || undefined });
      const rows = h('tbody');
      for (const x of r.rows.slice(0, 200)) {
        rows.append(h('tr', {}, h('td', { text: when(x.windowEnd) }), h('td', { text: x.group || '' }), h('td', { text: x.field }), h('td', { class: 'n', text: +x.value.toFixed(4) }), h('td', { class: 'n', text: x.count || '' })));
      }
      result.replaceChildren(...[
        h('p', { class: 'hint', text: `${r.readings} readings from ${r.sensors.length} sensor(s) → ${r.rows.length}${r.truncated ? '+' : ''} result(s); ${r.status.late} late, ${r.status.filtered} filtered. The last, partial window is included.` }),
        r.alerts?.length ? h('ul', {}, ...r.alerts.map((a) => h('li', { text: a }))) : null,
        r.rows.length ? h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ...['Window end', 'Output', 'Field', 'Value', 'Count'].map((t) => h('th', { text: t })))), rows)) : null,
      ].filter(Boolean));
    } catch (e) { result.replaceChildren(); err.textContent = e.message; }
  };
  const save = async () => {
    err.textContent = '';
    try {
      await put('/api/jobs/' + enc(id.value.trim()), { query: q.value, enabled: on.checked, lateness: late.value || undefined });
      toast('Job saved'); jobsTab();
    } catch (e) { err.textContent = e.message; }
  };
  view.replaceChildren(
    h('section', { class: 'card' },
      h('h2', { text: 'Stream jobs' }),
      h('p', { class: 'sub', text: 'Windowed queries over live readings, as in Azure Stream Analytics. Results become sensors, alarms, or webhook posts.' }),
      h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' },
        h('thead', {}, h('tr', {}, ...['Job', '', 'In', 'Filtered', 'Out', 'Late', 'Dropped', 'Errors', 'Last output', 'Last error', ''].map((t) => h('th', { text: t })))), tbody))),
    h('section', { class: 'card' },
      h('h2', { text: edit ? 'Edit ' + edit.id : 'New job' }),
      h('div', { class: 'row' }, field('Id', id), field('Lateness', late), field('Running', on, 'check')),
      q,
      h('p', { class: 'hint', text: 'SELECT avg|min|max|sum|count|stddev|first|last|delta|increase(field) [AS name] INTO [sensor] | [prefix-{sensor}] | alert | webhook:<target> FROM [glob] [WHERE value …] GROUP BY [sensor,] TumblingWindow(minute, 5) | HoppingWindow(minute, 60, 15) | SlidingWindow(minute, 10) [HAVING value > 80]' }),
      err,
      h('div', { class: 'row' }, h('button', { onclick: test }, 'Test on history'), from,
        h('button', { class: 'primary', onclick: save }, 'Save'),
        edit && h('button', { class: 'danger', onclick: async () => {
          if (!confirm('Delete job ' + edit.id + '? Its open alarms clear.')) return;
          try { await del('/api/jobs/' + enc(edit.id)); jobsTab(); } catch (e) { fail(e); }
        } }, 'Delete'), edit && h('button', { onclick: () => jobsTab() }, 'New job')),
      result));
}

// ---- Grafana (per tenant) ----------------------------------------------------
async function grafanaTab() {
  const g = await api('/api/grafana');
  if (!g.grafana || g.grafana.error) {
    view.replaceChildren(h('section', { class: 'card' }, h('h2', { text: 'Grafana' }),
      h('p', { class: 'sub', text: g.grafana?.error ? 'Setting up Grafana for this tenant failed: ' + g.grafana.error + ' The platform operator can retry it.' : 'Grafana has not been set up for this tenant yet; the platform operator provisions it.' })));
    return;
  }
  const tbody = h('tbody');
  for (const u of g.users || []) {
    tbody.append(h('tr', {}, h('td', {}, h('b', { text: tenantPrefix() + u.login })), h('td', { text: u.role }),
      h('td', {}, h('div', { class: 'row' },
        h('button', { class: 'small', onclick: async () => {
          try { await put('/api/grafana/users/' + enc(u.login), { role: u.role === 'Viewer' ? 'Editor' : 'Viewer' }); grafanaTab(); } catch (e) { fail(e); }
        } }, u.role === 'Viewer' ? 'Make editor' : 'Make viewer'),
        h('button', { class: 'small danger', onclick: async () => {
          if (!confirm('Remove Grafana user ' + u.login + '?')) return;
          try { await del('/api/grafana/users/' + enc(u.login)); grafanaTab(); } catch (e) { fail(e); }
        } }, 'Remove')))));
  }
  const login = h('input', { placeholder: 'ana', 'aria-label': 'Grafana login' });
  const email = h('input', { placeholder: 'ana@plant.co (optional)', 'aria-label': 'Email' });
  const role = h('select', { 'aria-label': 'Grafana role' }, opt('Viewer'), opt('Editor'));
  const err = h('p', { class: 'err', role: 'alert' });
  view.replaceChildren(
    h('section', { class: 'card' },
      h('h2', { text: 'Grafana' }),
      h('p', { class: 'sub' }, 'This tenant has its own Grafana organization with the IoT Hub dashboards: ',
        h('a', { href: g.grafana.url, target: '_blank', rel: 'noopener', text: g.grafana.url }),
        '. Its data source can only read this tenant\'s data, so editors may write their own SQL.'),
      h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' },
        h('thead', {}, h('tr', {}, ...['Login', 'Role', ''].map((t) => h('th', { text: t })))), tbody))),
    h('section', { class: 'card' },
      h('h2', { text: 'Add a Grafana user' }),
      h('div', { class: 'row' }, field('Login', login), field('Email', email), field('Role', role),
        h('button', { class: 'primary', onclick: async () => {
          err.textContent = '';
          try {
            const r = await put('/api/grafana/users/' + enc(login.value.trim()), { role: role.value, email: email.value.trim() || undefined });
            if (r.password) showSecrets('Grafana sign-in for ' + r.login, r); else toast('Role updated');
            grafanaTab();
          } catch (e) { err.textContent = e.message; }
        } }, 'Add')),
      h('p', { class: 'hint', text: 'Editors can build dashboards and write queries; viewers only look. Organization admin stays with the platform.' }),
      err));
}
const tenantPrefix = () => (tenant || me.tenant?.id || '') + '.';

// ---- tenants (superadmin) ---------------------------------------------------
async function tenantsTab() {
  const list = await api('/api/admin/tenants');
  const tbody = h('tbody');
  for (const t of list) {
    const q = t.effectiveQuota;
    tbody.append(h('tr', {},
      h('td', {}, h('b', { text: t.id }), t.name ? h('div', { class: 'hint', text: t.name }) : null),
      h('td', {}, h('span', { class: 'pill ' + (t.status === 'active' ? 'on' : 'bad'), text: t.status })),
      h('td', { class: 'n', text: `${t.sensors} / ${q.maxSensors}` }), h('td', { class: 'n', text: `${t.devices} / ${q.maxDevices || '∞'}` }),
      h('td', { class: 'n', text: `${t.messagesToday} / ${q.messagesPerDay || '∞'}` }), h('td', { class: 'n', text: t.messagesRejected }),
      h('td', { text: t.deviceSelfService ? 'tenant' : 'platform' }),
      h('td', {}, t.grafana?.orgId && !t.grafana.error ? h('a', { href: t.grafana.url, target: '_blank', rel: 'noopener', text: 'org ' + t.grafana.orgId })
        : t.grafana?.error ? h('span', { class: 'err', title: t.grafana.error, text: 'failed' }) : '—'),
      h('td', {}, h('div', { class: 'row' },
        me.grafana && h('button', { class: 'small', onclick: async () => {
          try { await post('/api/admin/tenants/' + enc(t.id) + '/grafana'); toast('Grafana provisioned for ' + t.id); tenantsTab(); } catch (e) { fail(e); }
        } }, t.grafana?.orgId && !t.grafana.error ? 'Repair Grafana' : 'Provision Grafana'),
        t.status === 'active' && h('button', { class: 'small', onclick: () => { safeSet('iothub.tenant', t.id); location.href = './'; } }, 'Open'),
        h('button', { class: 'small', onclick: () => tenantForm(t) }, 'Edit'),
        h('button', { class: 'small', onclick: async () => {
          try { await patch('/api/admin/tenants/' + enc(t.id), { status: t.status === 'active' ? 'suspended' : 'active' }); tenantsTab(); } catch (e) { fail(e); }
        } }, t.status === 'active' ? 'Suspend' : 'Resume'),
        h('button', { class: 'small danger', onclick: async () => {
          const typed = prompt(`Deleting erases all of ${t.id}'s data, credentials and backups. Type ${t.id} to confirm.`);
          if (typed !== t.id) return;
          try { await del('/api/admin/tenants/' + enc(t.id) + '?confirm=' + enc(t.id)); tenantsTab(); } catch (e) { fail(e); }
        } }, 'Delete')))));
  }
  view.replaceChildren(
    h('section', { class: 'card' },
      h('h2', { text: 'Tenants' }),
      h('p', { class: 'sub', text: 'Each tenant is an isolated hub with its own data, devices and settings.' }),
      h('div', { class: 'tbl-wrap' }, h('table', { class: 'tbl' },
        h('thead', {}, h('tr', {}, ...['Tenant', 'Status', 'Sensors', 'Devices', 'Messages today', 'Rejected', 'Credentials by', 'Grafana', ''].map((x) => h('th', { text: x })))), tbody))),
    tenantForm());
}

function tenantForm(t) {
  const q = t?.quota || {};
  const id = h('input', { value: t?.id || '', readonly: !!t, placeholder: 'acme', 'aria-label': 'Tenant id' });
  const name = h('input', { value: t?.name || '', placeholder: 'Acme Mining' });
  const n = (v) => h('input', { type: 'number', value: v ?? '', placeholder: 'default', size: 7 });
  const rate = n(q.messagesPerSecond), daily = n(q.messagesPerDay), sensors = n(q.maxSensors), devices = n(q.maxDevices),
    raw = n(q.rawRetentionDays), rollup = n(q.rollupRetentionDays), jobs = n(q.maxJobs);
  const self = h('input', { type: 'checkbox', checked: t?.deviceSelfService });
  const err = h('p', { class: 'err', role: 'alert' });
  const val = (i) => (i.value === '' ? undefined : Number(i.value));
  const card = h('section', { class: 'card' },
    h('h2', { text: t ? 'Edit ' + t.id : 'New tenant' }),
    h('div', { class: 'row' }, field('Id', id), field('Name', name), field('Tenant admins issue device credentials', self, 'check')),
    h('h3', { text: 'Quota (empty = platform default, -1 = unlimited)' }),
    h('div', { class: 'row' }, field('Messages / s', rate), field('Messages / day', daily), field('Sensors', sensors),
      field('Devices', devices), field('Raw days', raw), field('Rollup days', rollup), field('Jobs', jobs)),
    err,
    h('div', { class: 'row' }, h('button', { class: 'primary', onclick: async () => {
      err.textContent = '';
      const quota = { messagesPerSecond: val(rate), messagesPerDay: val(daily), maxSensors: val(sensors), maxDevices: val(devices),
        rawRetentionDays: val(raw), rollupRetentionDays: val(rollup), maxJobs: val(jobs) };
      try {
        if (t) await patch('/api/admin/tenants/' + enc(t.id), { name: name.value, quota, deviceSelfService: self.checked });
        else await post('/api/admin/tenants', { id: id.value.trim(), name: name.value || undefined, quota, deviceSelfService: self.checked });
        toast('Saved'); tenantsTab();
      } catch (e) { err.textContent = e.message; }
    } }, t ? 'Save' : 'Create tenant'), t && h('button', { onclick: tenantsTab }, 'Cancel')));
  if (t) view.replaceChildren(card);
  return card;
}

// ---- boot ---------------------------------------------------------------------
const savedTheme = safeGet('iothub.theme');
if (savedTheme) document.documentElement.dataset.theme = savedTheme;
try {
  me = await (await fetch('/api/me', { credentials: 'same-origin' })).json();
} catch { me = {}; }
if (!me.identity) {
  view.replaceChildren(h('p', { class: 'err', text: 'Sign in on the dashboard first.' }), h('a', { class: 'button', href: './' }, 'Dashboard'));
} else if (!['admin', 'superadmin'].includes(me.identity.role) && me.authEnabled) {
  view.replaceChildren(h('p', { class: 'err', text: 'Settings are for administrators.' }));
} else {
  $('who').textContent = me.authEnabled ? `${me.identity.id} (${me.identity.role}${me.tenant ? ', ' + me.tenant.id : ''})` : 'authentication off';
  document.querySelector('[data-tab="grafana"]').hidden = !me.grafana;
  if (me.multiTenant && isSuper()) {
    document.querySelector('[data-tab="tenants"]').hidden = false;
    const ids = (me.tenants || []).filter((t) => t.status === 'active').map((t) => t.id);
    const sel = $('tenant');
    for (const i of ids) sel.append(opt(i));
    tenant = ids.includes(safeGet('iothub.tenant')) ? safeGet('iothub.tenant') : ids[0] || null;
    if (tenant) { sel.value = tenant; sel.hidden = false; }
    sel.onchange = () => { tenant = sel.value; safeSet('iothub.tenant', tenant); show(location.hash.slice(1)); };
    if (!tenant) location.hash = 'tenants';
  }
  show(location.hash.slice(1));
}
