// Commands: send named commands to devices (operators and admins), see
// what was sent, and (admins) define the catalog.
import {
  h, api, put, del, enc, pageHeader, card, badge, empty, table, tabs, field, opt, button, icon, modal, confirmDialog,
  ago, when, toast,
} from '../core.js';
import { deviceFreshness } from './overview.js';

const RUN_TONE = { ok: 'good', completed: 'good', queued: 'info', delivered: 'info', failed: 'critical', offline: 'warning',
  timeout: 'warning', rejected: 'critical', deadlettered: 'critical', expired: 'warning', untracked: 'neutral' };
const RUN_LABEL = { ok: 'Done', failed: 'Device refused', offline: 'Device offline', timeout: 'No answer', queued: 'Queued',
  delivered: 'Delivered', completed: 'Completed', rejected: 'Rejected', deadlettered: 'Undeliverable', expired: 'Expired', untracked: 'Sent' };
export const runBadge = (r) => badge(RUN_LABEL[r.status] || r.status, RUN_TONE[r.status] || 'neutral');

const cmdLabel = (c) => c.label || c.name;

// Ask for parameters (and confirmation), run, report. Resolves to the run.
export async function runCommand(device, c) {
  const inputs = (c.params || []).map((p) => {
    let input;
    if (p.type === 'bool') input = h('input', { type: 'checkbox', checked: p.default === true });
    else if (p.type === 'choice') input = h('select', {}, !p.required && opt('', '—'), p.choices.map((x) => opt(x, x, x === p.default)));
    else input = h('input', { type: p.type === 'number' ? 'number' : 'text', step: 'any', min: p.min ?? null, max: p.max ?? null, value: p.default ?? '' });
    const hint = p.type === 'number' && (p.min != null || p.max != null) ? `${p.min ?? '…'} to ${p.max ?? '…'}` : null;
    return { p, input, el: field((p.label || p.name) + (p.required ? '' : ' (optional)'), input, { hint, cls: p.type === 'bool' ? 'check' : '' }) };
  });
  const read = () => {
    const out = {};
    for (const { p, input } of inputs) {
      if (p.type === 'bool') out[p.name] = input.checked;
      else if (input.value === '') continue;
      else out[p.name] = p.type === 'number' ? Number(input.value) : input.value;
    }
    return out;
  };
  const go = () => api(`/api/devices/${enc(device)}/commands/${enc(c.name)}`, { method: 'POST', body: JSON.stringify({ params: read() }) });
  let run;
  if (inputs.length || c.confirm) {
    run = await modal(`${cmdLabel(c)} on ${device}`, h('div', { class: 'stack' },
      c.confirm && h('p', { class: 'modal-text', text: `This sends "${cmdLabel(c)}" to ${device}${c.kind === 'message' ? ' (it is delivered when the device next listens)' : ''}.` }),
      inputs.map((x) => x.el)), {
      actions: [['Cancel'], [c.kind === 'method' ? 'Run now' : 'Send', go, c.confirm ? 'danger' : 'primary']],
    });
  } else {
    run = await go().catch((e) => { toast(e.message, 'bad'); return null; });
  }
  if (!run || run === true) return null;
  const what = `${cmdLabel(c)} on ${device}`;
  if (run.status === 'ok') toast(`${what}: done${run.code ? ' (' + run.code + ')' : ''}`);
  else if (run.status === 'queued') toast(`${what}: queued for delivery`);
  else toast(`${what}: ${RUN_LABEL[run.status] || run.status}${run.error ? ' — ' + run.error : ''}`, 'bad');
  return run;
}

export function historyTable(runs, { showDevice } = {}) {
  return table(['When', ...(showDevice ? ['Device'] : []), 'Command', 'By', 'Result', 'Details'], runs.map((r) => h('tr', {},
    h('td', { title: when(r.at), text: ago(r.at) }),
    showDevice && h('td', {}, h('b', { text: r.device })),
    h('td', {}, h('div', { class: 'primary-cell' }, h('b', { text: r.label || r.command }), r.payload && h('small', { class: 'mono', text: JSON.stringify(r.payload) }))),
    h('td', { text: r.by }),
    h('td', {}, runBadge(r)),
    h('td', { class: 'mono', text: r.error || (r.result ? `${r.code} ${JSON.stringify(r.result)}` : r.messageId ? 'message ' + r.messageId : '') }))),
  { empty: 'Nothing sent yet.' });
}

// The buttons for one device.
export function commandButtons(device, cmds, after) {
  return h('div', { class: 'row' }, cmds.map((c) => button(cmdLabel(c), async () => { const r = await runCommand(device, c); if (r) after?.(r); },
    { kind: 'small', title: c.kind === 'method' ? `Direct method ${c.method || c.name}` : 'Queued message' })));
}

export async function render(el, ctx) {
  const data = await api('/api/commands');
  data.commands ||= []; data.devices ||= []; data.history ||= [];
  const tab = ['send', 'history', ...(data.canEdit ? ['catalog'] : [])].includes(ctx.params[0]) ? ctx.params[0] : 'send';
  const byName = Object.fromEntries(data.commands.map((c) => [c.name, c]));
  el.append(pageHeader('Commands', {
    sub: 'Send named commands to devices. Admins decide which commands exist and who may run them.',
    actions: data.canEdit && button('New command', () => editCommand(null, ctx), { kind: 'primary', ico: 'plus' }),
  }), tabs([['send', 'Send'], ['history', 'History', data.history.length], ...(data.canEdit ? [['catalog', 'Catalog', data.commands.length]] : [])],
    tab, (k) => { location.hash = '#/commands/' + k; }));

  if (tab === 'history') { el.append(card(null, {}, historyTable(data.history, { showDevice: true }))); return; }
  if (tab === 'catalog') return catalog(el, ctx, data);

  const q = h('input', { type: 'search', placeholder: 'Find a device', 'aria-label': 'Find a device' });
  const box = h('div');
  const draw = () => {
    const t = q.value.trim().toLowerCase();
    const devs = data.devices.filter((d) => d.commands.length && (!t || d.id.toLowerCase().includes(t))).sort((a, b) => a.id.localeCompare(b.id));
    box.replaceChildren(card(null, {}, !data.commands.length
      ? empty('No commands yet', data.canEdit ? 'Define commands such as "Reboot" or "Close valve" once, for the devices they apply to.' : 'An admin has not defined any commands yet.',
        data.canEdit && button('New command', () => editCommand(null, ctx), { kind: 'primary' }))
      : devs.length ? table(['Device', 'Status', 'Commands'], devs.map((d) => {
        const [label, tone] = deviceFreshness(d.lastDataTime, d.expectedIntervalSec);
        return h('tr', {},
          h('td', {}, h('div', { class: 'primary-cell' }, h('b', { text: d.id }), h('small', { text: d.lastDataTime ? 'last data ' + ago(d.lastDataTime) : 'no data yet' }))),
          h('td', {}, badge(label, tone), d.connectionState === 'Connected' ? h('span', { class: 'chip', text: 'MQTT connected' }) : null),
          h('td', {}, commandButtons(d.id, d.commands.map((n) => byName[n]).filter(Boolean), () => ctx.reload())));
      })) : empty('No matches', t ? 'No device with commands matches.' : 'No device has a command you may run.')));
  };
  q.oninput = draw;
  el.append(h('div', { class: 'toolbar' }, h('div', { class: 'search' }, icon('search'), q),
    h('span', { class: 'hint', text: 'Methods need the device connected over MQTT; messages wait until it listens.' })), box);
  draw();
}

function catalog(el, ctx, data) {
  el.append(card(null, {}, data.commands.length ? table(['Command', 'Kind', 'Devices', 'Parameters', 'Who', ''], data.commands.map((c) => h('tr', {},
    h('td', {}, h('div', { class: 'primary-cell' }, h('b', { text: cmdLabel(c) }), h('small', { class: 'mono', text: c.name }))),
    h('td', { text: c.kind === 'method' ? `method ${c.method || c.name}` : `message (keep ${c.ttl || '1h'})` }),
    h('td', {}, (c.devices?.length ? c.devices : ['all devices']).map((p) => h('span', { class: 'chip mono', text: p }))),
    h('td', { text: (c.params || []).map((p) => p.name).join(', ') || '—' }),
    h('td', {}, c.role === 'admin' ? badge('Admins', 'warning') : badge('Operators', 'info'), c.confirm ? h('span', { class: 'chip', text: 'confirm' }) : null),
    h('td', { class: 'actions' }, button('Edit', () => editCommand(c, ctx), { kind: 'small' }),
      button('Delete', async () => {
        if (!await confirmDialog(`Delete ${cmdLabel(c)}?`, 'It disappears from every device. Its past runs stay in the history.', { ok: 'Delete', danger: true })) return;
        await del('/api/commands/' + enc(c.name)); toast('Deleted'); ctx.reload();
      }, { kind: 'small danger' })))))
    : empty('No commands yet', 'Define each command once, with the devices it applies to.', button('New command', () => editCommand(null, ctx), { kind: 'primary' }))));
}

async function editCommand(c, ctx) {
  c = c || { kind: 'method', role: 'operator', params: [] };
  const name = h('input', { value: c.name || '', readonly: !!c.name, placeholder: 'reboot', spellcheck: 'false' });
  const label = h('input', { value: c.label || '', placeholder: 'Reboot' });
  const kind = h('select', {}, opt('method', 'Direct method — runs now, the device answers', c.kind === 'method'), opt('message', 'Message — queued until the device listens', c.kind === 'message'));
  const method = h('input', { value: c.method || '', placeholder: 'defaults to the command name', spellcheck: 'false' });
  const timeout = h('select', {}, ['5s', '10s', '30s', '60s', '300s'].map((x) => opt(x, 'wait ' + x, (c.timeout || '30s') === x)));
  const ttl = h('select', {}, ['10m', '1h', '8h', '24h', '48h'].map((x) => opt(x, 'keep ' + x, (c.ttl || '1h') === x)));
  const devices = h('input', { value: (c.devices || []).join(', '), placeholder: 'pump-*, valve-3 (empty: every device)', spellcheck: 'false' });
  const payload = h('textarea', { spellcheck: 'false', placeholder: '{"cmd": "reboot"}', style: 'min-height:70px' });
  payload.value = c.payload ? JSON.stringify(c.payload, null, 2) : '';
  const role = h('select', {}, opt('operator', 'Operators and admins', c.role !== 'admin'), opt('admin', 'Admins only', c.role === 'admin'));
  const confirm = h('input', { type: 'checkbox', checked: c.confirm });
  const methodField = field('Method name', method), timeoutField = field('Wait for the answer', timeout), ttlField = field('Delivery', ttl);
  const syncKind = () => { methodField.hidden = timeoutField.hidden = kind.value !== 'method'; ttlField.hidden = kind.value !== 'message'; };
  kind.onchange = syncKind; syncKind();

  const params = h('tbody');
  const paramRow = (p = { type: 'number' }) => {
    const pn = h('input', { value: p.name || '', placeholder: 'delay', spellcheck: 'false', 'aria-label': 'Parameter name', style: 'min-width:90px' });
    const pl = h('input', { value: p.label || '', placeholder: 'Delay (s)', 'aria-label': 'Label', style: 'min-width:100px' });
    const pt = h('select', { 'aria-label': 'Type' }, ['number', 'text', 'bool', 'choice'].map((x) => opt(x, x, p.type === x)));
    const extra = h('input', { 'aria-label': 'Range or choices', style: 'min-width:120px',
      value: p.type === 'choice' ? (p.choices || []).join(', ') : p.min != null || p.max != null ? `${p.min ?? ''}..${p.max ?? ''}` : '' });
    const syncX = () => { extra.placeholder = pt.value === 'choice' ? 'open, closed' : pt.value === 'number' ? '0..60' : ''; extra.disabled = pt.value === 'bool' || pt.value === 'text'; };
    pt.onchange = syncX; syncX();
    const def = h('input', { value: p.default ?? '', 'aria-label': 'Default', placeholder: 'default', style: 'min-width:70px' });
    const req = h('input', { type: 'checkbox', checked: p.required, 'aria-label': 'Required' });
    const tr = h('tr', {}, h('td', {}, pn), h('td', {}, pl), h('td', {}, pt), h('td', {}, extra), h('td', {}, def), h('td', { style: 'text-align:center' }, req),
      h('td', {}, h('button', { type: 'button', class: 'btn ghost icon small danger', 'aria-label': 'Remove parameter', onclick: () => tr.remove() }, icon('close'))));
    tr.read = () => {
      const out = { name: pn.value.trim(), label: pl.value.trim() || undefined, type: pt.value, required: req.checked || undefined };
      const x = extra.value.trim();
      if (out.type === 'choice') out.choices = x.split(',').map((s) => s.trim()).filter(Boolean);
      if (out.type === 'number' && x) {
        const [a, b] = x.split('..');
        if (a?.trim()) out.min = Number(a); if (b?.trim()) out.max = Number(b);
      }
      const d = def.value.trim();
      if (d !== '') out.default = out.type === 'number' ? Number(d) : out.type === 'bool' ? d === 'true' : d;
      return out;
    };
    return tr;
  };
  for (const p of c.params || []) params.append(paramRow(p));

  const ok = await modal(c.name ? 'Edit ' + cmdLabel(c) : 'New command', h('div', { class: 'stack' },
    h('div', { class: 'form-grid' }, field('Command id', name, { hint: 'a-z, 0-9, _ and -' }), field('Label on the button', label)),
    h('div', { class: 'form-grid' }, field('Kind', kind, { cls: 'span2' }), methodField, timeoutField, ttlField),
    field('Devices', devices, { hint: 'Device id patterns. The command is offered on every match.' }),
    field('Payload (JSON)', payload, { hint: 'Sent as is; parameters below are added as keys of this object.' }),
    h('div', {}, h('div', { class: 'row' }, h('span', { class: 'field-label', text: 'Parameters the person fills in' }), h('span', { class: 'spacer' }),
      button('Add parameter', () => params.append(paramRow()), { kind: 'small', ico: 'plus' })),
    h('div', { class: 'tbl-wrap', style: 'margin:0;padding:0' }, h('table', { class: 'tbl fields' },
      h('thead', {}, h('tr', {}, ['Name', 'Label', 'Type', 'Range / choices', 'Default', 'Required', ''].map((x) => h('th', { text: x })))), params))),
    h('div', { class: 'form-grid' }, field('Who may run it', role), h('div', { class: 'field', style: 'justify-content:end' }, field('Ask for confirmation', confirm, { cls: 'check' })))), {
    wide: true,
    actions: [['Cancel'], ['Save command', async () => {
      let pl;
      if (payload.value.trim()) { try { pl = JSON.parse(payload.value); } catch (e) { throw new Error('Payload is not valid JSON: ' + e.message); } }
      const body = { name: name.value.trim(), label: label.value.trim() || undefined, kind: kind.value,
        devices: devices.value.split(',').map((x) => x.trim()).filter(Boolean), payload: pl,
        params: [...params.children].map((r) => r.read()).filter((p) => p.name), role: role.value, confirm: confirm.checked || undefined };
      if (kind.value === 'method') { body.method = method.value.trim() || undefined; body.timeout = timeout.value; } else body.ttl = ttl.value;
      if (!body.name) throw new Error('Give the command an id.');
      return put('/api/commands/' + enc(body.name), body);
    }, 'primary']],
  });
  if (!ok) return;
  toast('Command saved');
  if (location.hash === '#/commands/catalog') ctx.reload(); else location.hash = '#/commands/catalog';
}

