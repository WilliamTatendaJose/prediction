// Devices & access: every device, service and person with a credential;
// one page per identity (credentials, twin, direct methods, messages).
import {
  h, api, post, put, patch, del, enc, pageHeader, card, badge, empty, table, tabs, field, opt, button, icon, linkButton,
  modal, confirmDialog, showSecrets, session, isSuper, multiTenant, tenantId, when, ago, toast,
} from '../core.js';
import { deviceFreshness, every } from './overview.js';
import { commandButtons, historyTable } from './commands.js';

const THINGS = ['device', 'service'];
const ROLE_INFO = {
  device: 'Sends data for its sensors, receives commands and twin updates',
  service: 'An application or gateway sending data for many sensors',
  viewer: 'A person who can see dashboards and alarms',
  operator: 'A person who can also acknowledge and shelve alarms',
  admin: 'A person who manages this tenant: sensors, devices, settings',
};
// Without self-service the platform operator issues credentials.
const locked = () => !isSuper() && multiTenant() && !session.me.deviceSelfService;

// A device's state is its last accepted reading (any transport), against
// its expected interval when one is set (see deviceFreshness).
function status(d, twin) {
  if (d.disabled) return ['Disabled', 'neutral'];
  if (d.expires && d.expires < Date.now()) return ['Expired', 'critical'];
  if (!THINGS.includes(d.role)) return ['Active', 'good'];
  return deviceFreshness(twin?.lastDataTime, twin?.expectedIntervalSec);
}
const mqttChip = (twin) => (twin?.connectionState === 'Connected' ? h('span', { class: 'chip', title: 'Has an MQTT session now', text: 'MQTT connected' }) : null);

export async function render(el, ctx) {
  if (ctx.params[0]) return detail(el, ctx, ctx.params[0], ctx.params[1]);
  let seg = 'things';
  try { seg = sessionStorage.getItem('iothub.devseg') || 'things'; } catch { /* unavailable */ }
  return list(el, ctx, seg);
}

async function list(el, ctx, seg) {
  const [devices, twins] = await Promise.all([api('/api/devices'), api('/api/twins').catch(() => [])]);
  const tw = Object.fromEntries(twins.map((t) => [t.deviceId, t]));
  const things = devices.filter((d) => THINGS.includes(d.role)), people = devices.filter((d) => !THINGS.includes(d.role));
  const q = h('input', { type: 'search', placeholder: 'Search by id, sensor or note', 'aria-label': 'Search' });
  const box = h('div');
  const draw = () => {
    const t = q.value.trim().toLowerCase();
    const src = (seg === 'people' ? people : things).filter((d) => !t || `${d.id} ${(d.sensors || []).join(' ')} ${d.note || ''}`.toLowerCase().includes(t));
    const rows = src.sort((a, b) => a.id.localeCompare(b.id)).map((d) => {
      const [label, tone] = status(d, tw[d.id]);
      return h('tr', { class: 'link', onclick: (ev) => { if (!ev.target.closest('a,button')) location.hash = '#/devices/' + enc(d.id); } },
        h('td', {}, h('div', { class: 'primary-cell' }, h('a', { href: '#/devices/' + enc(d.id), text: d.id }), h('small', { text: d.note || ROLE_INFO[d.role] || '' }))),
        h('td', {}, h('span', { class: 'chip', text: d.role })),
        h('td', {}, badge(label, tone), seg === 'things' && mqttChip(tw[d.id])),
        seg === 'things' && h('td', {}, (d.sensors || []).map((p) => h('span', { class: 'chip mono', text: p }))),
        h('td', {}, d.keys && h('span', { class: 'chip', text: 'keys / SAS' }), d.token && h('span', { class: 'chip', text: 'token' })),
        h('td', { class: 'n' }, seg === 'things' ? [tw[d.id]?.lastDataTime ? ago(tw[d.id].lastDataTime) : 'never',
          tw[d.id]?.expectedIntervalSec ? h('div', { class: 'hint', text: 'every ' + every(tw[d.id].expectedIntervalSec) }) : null] : d.expires ? when(d.expires) : 'never'));
    });
    const heads = seg === 'things'
      ? ['Device', 'Role', 'Status', 'Sensors', 'Credential', { text: 'Last data', cls: 'n' }]
      : ['User', 'Role', 'Status', 'Credential', { text: 'Expires', cls: 'n' }];
    box.replaceChildren(card(null, {}, src.length || t ? table(heads, rows, { empty: 'Nothing matches.' })
      : seg === 'things' ? empty('No devices yet', 'Add a device to get a connection string it can use over MQTT or HTTP.', !locked() && button('Add device', () => addIdentity('device'), { kind: 'primary', ico: 'plus' }))
        : empty('No users yet', 'Give each person their own token with the right role, so access can be revoked one at a time.', !locked() && button('Add user', () => addIdentity('viewer'), { kind: 'primary', ico: 'plus' }))));
  };
  q.oninput = draw;
  el.append(
    pageHeader('Devices & access', {
      sub: locked() ? 'Credentials for this tenant are issued by the platform operator. Ask them for new connection strings.' : 'Every device, application and person gets its own credential, so each can be rotated or revoked alone.',
      actions: !locked() && [button('Add user', () => addIdentity('viewer')), button('Add device', () => addIdentity('device'), { kind: 'primary', ico: 'plus' })],
    }),
    tabs([['things', 'Devices & services', things.length], ['people', 'People', people.length]], seg, (k) => { try { sessionStorage.setItem('iothub.devseg', k); } catch { /* unavailable */ } seg = k; el.replaceChildren(); list(el, ctx, k); }),
    h('div', { class: 'toolbar' }, h('div', { class: 'search' }, icon('search'), q)),
    box);
  draw();
}

async function addIdentity(defRole) {
  const isThing = THINGS.includes(defRole);
  const id = h('input', { placeholder: isThing ? 'pump-1' : 'ana', spellcheck: 'false' });
  const role = h('select', {}, (isThing ? THINGS : ['viewer', 'operator', 'admin']).map((r) => opt(r, r[0].toUpperCase() + r.slice(1), r === defRole)));
  const roleHint = h('span', { class: 'field-hint', text: ROLE_INFO[defRole] });
  role.onchange = () => { roleHint.textContent = ROLE_INFO[role.value]; };
  const sensors = h('input', { placeholder: 'pump-1, line3-*', spellcheck: 'false' });
  const auth = h('select', {}, opt('keys', 'Keys + SAS tokens (rotatable)'), opt('token', 'Static token'), opt('both', 'Both'));
  if (!isThing) auth.value = 'token';
  const expires = h('input', { type: 'date' });
  const note = h('input', { placeholder: isThing ? 'Roof tank, line 3…' : 'Name, team…' });
  const roleField = field('Role', role); roleField.append(roleHint);
  const r = await modal(isThing ? 'Add device' : 'Add user', h('div', { class: 'stack' },
    h('div', { class: 'form-grid' }, field(isThing ? 'Device id' : 'User id', id), roleField),
    isThing && field('Sensors it may send for', sensors, { hint: 'Comma-separated ids or patterns. It can only touch these sensors.' }),
    h('div', { class: 'form-grid' }, isThing && field('Credential', auth), field('Expires (optional)', expires)),
    field('Note', note)), {
    wide: true,
    actions: [['Cancel'], ['Create', async () => {
      const body = { id: id.value.trim(), role: role.value, auth: isThing ? auth.value : 'token', note: note.value.trim() || undefined,
        sensors: isThing ? sensors.value.split(',').map((x) => x.trim()).filter(Boolean) : [] };
      if (!body.id) throw new Error('Give it an id.');
      if (isThing && !body.sensors.length) throw new Error('List the sensors it may send for (use * for all).');
      if (expires.value) body.expires = new Date(expires.value + 'T23:59:59').getTime();
      return post('/api/devices', body);
    }, 'primary']],
  });
  if (!r?.id) return;
  await showSecrets(`Credentials for ${r.id}`, r);
  location.hash = '#/devices/' + enc(r.id);
}

async function detail(el, ctx, id, tab) {
  const devices = await api('/api/devices');
  const d = devices.find((x) => x.id === id);
  if (!d) { el.append(pageHeader(id, { back: ['#/devices', 'Devices & access'] }), empty('Not found', `"${id}" has no credential in this tenant.`, linkButton('All devices', '#/devices'))); return; }
  const thing = THINGS.includes(d.role);
  const twin = thing ? await api('/api/twins/' + enc(id)).catch(() => null) : null;
  tab = (thing ? ['overview', 'commands', 'twin', 'methods', 'messages'] : ['overview']).includes(tab) ? tab : 'overview';
  ctx.crumbs([[d.id]]);
  const [label, tone] = status(d, twin);
  const p = '/api/devices/' + enc(id);
  const reload = () => ctx.reload();
  el.append(pageHeader(d.id, {
    back: ['#/devices', 'Devices & access'],
    sub: h('span', { class: 'page-sub' }, badge(label, tone), thing && h('span', { text: twin?.lastDataTime ? 'last data ' + ago(twin.lastDataTime) : 'no data yet' }),
      mqttChip(twin), h('span', { class: 'chip', text: d.role }), d.note && h('span', { text: d.note })),
    actions: !locked() && [
      button(d.disabled ? 'Enable' : 'Disable', async () => {
        if (!d.disabled && !await confirmDialog(`Disable ${d.id}?`, 'It is disconnected now and refused until enabled again. Its credentials are kept.', { ok: 'Disable' })) return;
        await patch(p, { disabled: !d.disabled }); toast(d.disabled ? 'Enabled' : 'Disabled'); reload();
      }),
      button('Delete', async () => {
        if (!await confirmDialog(`Delete ${d.id}?`, 'Its credentials are revoked and it is disconnected at once. This cannot be undone.', { ok: 'Delete', danger: true, typed: d.id })) return;
        await del(p); toast('Deleted ' + d.id); location.hash = '#/devices';
      }, { kind: 'danger' }),
    ],
  }));
  if (thing) el.append(tabs([['overview', 'Overview'], ['commands', 'Commands'], ['twin', 'Twin'], ['methods', 'Direct methods'], ['messages', 'Messages']], tab, (k) => { location.hash = `#/devices/${enc(id)}/${k}`; }));
  const body = h('div');
  el.append(body);
  if (tab === 'commands') return commandsTab(body, ctx, id);
  if (tab === 'twin') return twinTab(body, ctx, id, twin);
  if (tab === 'methods') return methodsTab(body, id, twin);
  if (tab === 'messages') return messagesTab(body, id);

  const act = (labelText, fn, kind) => (locked() ? null : button(labelText, fn, { kind: 'small ' + (kind || '') }));
  const host = location.hostname;
  body.append(h('div', { class: 'grid-2' },
    card('Details', {}, h('dl', { class: 'dl' },
      h('dt', { text: 'Id' }), h('dd', { class: 'mono', text: d.id }),
      multiTenant() && h('dt', { text: 'Tenant' }), multiTenant() && h('dd', { class: 'mono', text: d.tenant || tenantId() }),
      h('dt', { text: 'Role' }), h('dd', { text: `${d.role} — ${ROLE_INFO[d.role] || ''}` }),
      thing && h('dt', { text: 'Sensors' }), thing && h('dd', {}, (d.sensors || []).map((s) => h('span', { class: 'chip mono', text: s }))),
      h('dt', { text: 'Created' }), h('dd', { text: when(d.created) }),
      h('dt', { text: 'Expires' }), h('dd', { text: d.expires ? when(d.expires) : 'Never' }),
      thing && h('dt', { text: 'Expected every' }), thing && h('dd', {}, twin?.expectedIntervalSec ? every(twin.expectedIntervalSec) : 'Not set',
        ' ', button('Change', () => expectedDialog(id, twin?.expectedIntervalSec, reload), { kind: 'small ghost' })),
      thing && h('dt', { text: 'Last data' }), thing && h('dd', { text: twin?.lastDataTime ? `${ago(twin.lastDataTime)} (${when(twin.lastDataTime)})` : 'Never' }),
      thing && h('dt', { text: 'MQTT' }), thing && h('dd', { text: twin?.connectionState === 'Connected' ? 'Connected now' : 'No session now (HTTP devices never have one)' }),
      thing && h('dt', { text: 'Last activity' }), thing && h('dd', { text: twin?.lastActivityTime ? ago(twin.lastActivityTime) : '—' }))),
    card('Credentials', { sub: locked() ? 'Issued by the platform operator.' : 'Rotate one key while the device keeps using the other; tokens are replaced at once.' },
      h('dl', { class: 'dl' },
        h('dt', { text: 'Keys' }), h('dd', {}, d.keys ? badge('Primary and secondary', 'good') : badge('None', 'neutral')),
        h('dt', { text: 'Token' }), h('dd', {}, d.token ? badge('Set', 'good') : badge('None', 'neutral'))),
      h('div', { class: 'row section-gap' },
        d.keys && act('Show keys', async () => showSecrets('Keys for ' + d.id, await api(p + '/keys'))),
        d.keys && act('SAS token (24 h)', async () => showSecrets('SAS token for ' + d.id, await post(p + '/sas', { ttl: '24h' }), 'Valid for 24 hours. Devices usually sign their own from a key.')),
        act(d.keys ? 'Rotate primary key' : 'Add keys', async () => {
          if (d.keys && !await confirmDialog('Rotate the primary key?', 'The current primary key stops working now; the secondary keeps working.', { ok: 'Rotate' })) return;
          await showSecrets('New primary key for ' + d.id, await post(p + '/rotate', { which: 'primary' })); reload();
        }),
        d.token && act('Replace token', async () => {
          if (!await confirmDialog('Replace the token?', 'The current token stops working now. Anything using it must be updated.', { ok: 'Replace', danger: true })) return;
          await showSecrets('New token for ' + d.id, await post(p + '/rotate', { which: 'token' })); reload();
        }, 'danger'))),
  ));
  if (thing) {
    body.append(card('Connect', { sub: 'Where this device sends its data.' }, h('dl', { class: 'dl' },
      h('dt', { text: 'MQTT username' }), h('dd', { class: 'mono', text: multiTenant() ? `${d.tenant || tenantId()}/${d.id}` : d.id }),
      h('dt', { text: 'MQTT topic' }), h('dd', { class: 'mono', text: `${multiTenant() ? (d.tenant || tenantId()) + '/' : ''}iot/{sensor}` }),
      h('dt', { text: 'HTTP' }), h('dd', { class: 'mono', text: `POST ${location.protocol}//${host}${location.port ? ':' + location.port : ''}/api/sensors/{sensor}/data` }),
      h('dt', { text: 'Twin, methods, messages' }), h('dd', { class: 'mono', text: `${multiTenant() ? (d.tenant || tenantId()) + '/' : ''}devices/${d.id}/#` }))));
  }
}

async function expectedDialog(id, cur, reload) {
  const presets = [['', 'Not set (Quiet after 2 min, Silent after 1 h)'], ['10s', '10 seconds'], ['30s', '30 seconds'], ['1m', '1 minute'],
    ['5m', '5 minutes'], ['15m', '15 minutes'], ['1h', '1 hour'], ['6h', '6 hours'], ['24h', '24 hours'], ['custom', 'Other…']];
  const curText = cur ? (cur % 3600 === 0 ? cur / 3600 + 'h' : cur % 60 === 0 ? cur / 60 + 'm' : cur + 's') : '';
  const sel = h('select', {}, presets.map(([v, t]) => opt(v, t, v === curText)));
  const custom = h('input', { placeholder: 'e.g. 90s, 2m, 12h', value: curText });
  const customField = field('Interval', custom, { hint: 'A duration from 1s to 7 days' });
  if (curText && !presets.some(([v]) => v === curText)) sel.value = 'custom';
  const sync = () => { customField.hidden = sel.value !== 'custom'; };
  sel.onchange = sync; sync();
  const ok = await modal('How often does ' + id + ' send data?', h('div', { class: 'stack' },
    h('p', { class: 'modal-text', text: 'Once twice this interval passes without data (at least the interval plus 30 seconds), the device shows as Overdue and raises an alarm, which notifies and escalates like any other. Data clears it.' }),
    field('Expected interval', sel), customField), {
    actions: [['Cancel'], ['Save', () => put(`/api/devices/${enc(id)}/expected-interval`, { interval: sel.value === 'custom' ? custom.value.trim() : sel.value }), 'primary']],
  });
  if (ok) { toast('Expected interval saved'); reload(); }
}

async function commandsTab(body, ctx, id) {
  const r = await api(`/api/devices/${enc(id)}/commands`);
  body.append(
    card('Send a command', { sub: 'Commands come from the catalog on the Commands page.' },
      r.commands.length ? commandButtons(id, r.commands, () => ctx.reload())
        : empty('No commands for this device', 'Define one under Commands, with a device pattern that matches it.', h('a', { class: 'btn', href: '#/commands/catalog' }, 'Open the catalog'))),
    card('History', { sub: 'The last 50 commands sent to this device.' }, historyTable(r.history)));
}

const pretty = (o) => JSON.stringify(o, null, 2);
const strip = (o) => Object.fromEntries(Object.entries(o || {}).filter(([k]) => !k.startsWith('$')));

function twinTab(body, ctx, id, tw) {
  if (!tw) { body.append(empty('No twin', 'This identity has no device twin.')); return; }
  const p = '/api/twins/' + enc(id);
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
    const gone = (before, after) => Object.fromEntries(Object.keys(before).filter((k) => !(k in after)).map((k) => [k, null]));
    const patchBody = { tags: { ...gone(tw.tags || {}, t), ...t }, properties: { desired: { ...gone(strip(tw.properties.desired), d), ...d } } };
    try {
      await api(p, { method: 'PATCH', body: JSON.stringify(patchBody), headers: { 'If-Match': tw.etag } });
      toast('Twin saved; desired changes sent to ' + id);
      ctx.reload();
    } catch (e) { err.textContent = e.message + (/etag|precondition/i.test(e.message) ? ' — someone changed it meanwhile; reload to see the latest.' : ''); }
  };
  body.append(
    card('Device twin', { sub: `Version ${tw.version} · desired v${tw.properties.desired.$version} · reported v${tw.properties.reported.$version}` },
      h('div', { class: 'grid-2' },
        field('Tags — cloud only, for grouping and queries', tags),
        field('Desired properties — sent to the device', desired)),
      err,
      h('div', { class: 'row section-gap' }, button('Save twin', save, { kind: 'primary' }), button('Reload', () => ctx.reload()))),
    card('Reported by the device', { sub: tw.properties.reported.$lastUpdated ? 'Last reported ' + ago(tw.properties.reported.$lastUpdated) : 'Nothing reported yet.' },
      h('pre', { class: 'code', text: pretty(strip(tw.properties.reported)) })));
}

function methodsTab(body, id, tw) {
  const name = h('input', { placeholder: 'reboot', spellcheck: 'false' });
  const payload = h('textarea', { placeholder: '{"delay": 5}', spellcheck: 'false', style: 'min-height:80px' });
  const timeout = h('select', {}, ['10s', '30s', '60s', '300s'].map((x) => opt(x, 'Wait up to ' + x, x === '30s')));
  const out = h('pre', { class: 'code' });
  body.append(card('Invoke a direct method', {
    sub: `A request the device answers right away. It must be connected over MQTT and subscribed to its devices/${id}/methods/# topic.`,
    actions: tw && badge(tw.connectionState === 'Connected' ? 'Connected' : 'Not connected', tw.connectionState === 'Connected' ? 'good' : 'neutral'),
  },
  h('div', { class: 'stack' },
    h('div', { class: 'form-grid' }, field('Method', name), field('Timeout', timeout)),
    field('Payload (JSON, optional)', payload),
    h('div', { class: 'row' }, button('Invoke', async () => {
      if (!name.value.trim()) throw new Error('Enter a method name.');
      let pl = null;
      if (payload.value.trim()) { try { pl = JSON.parse(payload.value); } catch (e) { throw new Error('Payload is not valid JSON: ' + e.message); } }
      out.textContent = 'Calling…';
      try {
        const r = await post(`/api/devices/${enc(id)}/methods/${enc(name.value.trim())}?timeout=${timeout.value}`, pl);
        out.textContent = `status ${r.status}\n` + pretty(r.payload);
      } catch (e) { out.textContent = e.message; }
    }, { kind: 'primary' })),
    out)));
}

async function messagesTab(body, id) {
  const text = h('textarea', { placeholder: '{"cmd": "close-valve"}', spellcheck: 'false', style: 'min-height:80px' });
  const ttl = h('select', {}, ['10m', '1h', '24h', '48h'].map((x) => opt(x, 'Keep ' + x, x === '1h')));
  const listBox = h('div');
  const tone = { completed: 'good', deadlettered: 'critical', rejected: 'critical', expired: 'warning', queued: 'info', delivered: 'info' };
  const load = async () => {
    const list = await api(`/api/devices/${enc(id)}/messages`);
    listBox.replaceChildren(table(['Sent', 'Body', 'Status', { text: 'Deliveries', cls: 'n' }, 'Expires'],
      list.slice(0, 50).map((m) => h('tr', {}, h('td', { text: when(m.created) }), h('td', { class: 'mono', text: JSON.stringify(m.body) }),
        h('td', {}, badge(m.status, tone[m.status] || 'neutral')), h('td', { class: 'n', text: m.deliveries }), h('td', { text: when(m.expires) }))),
      { empty: 'No messages sent yet.' }));
  };
  body.append(card('Send a cloud-to-device message', {
    sub: `Queued until the device listens (MQTT devices/${id}/messages/#, or HTTP polling) and redelivered until it completes it.`,
  }, h('div', { class: 'stack' }, field('Body (JSON or text)', text), h('div', { class: 'row' }, ttl, button('Send', async () => {
    let b = text.value.trim();
    if (!b) throw new Error('Write the message first.');
    try { JSON.parse(b); } catch { b = JSON.stringify(b); } // plain text is sent as a JSON string
    await api(`/api/devices/${enc(id)}/messages?ttl=${ttl.value}`, { method: 'POST', body: b });
    toast('Queued for ' + id); text.value = ''; load();
  }, { kind: 'primary' })))),
  card('Recent messages', { actions: button('Refresh', load, { kind: 'small' }) }, listBox));
  await load();
}
