// Commands: send named commands to devices (operators and admins), see
// what was sent, and (admins) define the catalog.
import {
  h, api, put, del, enc, pageHeader, card, badge, empty, table, tabs, field, opt, button, icon, modal, confirmDialog,
  ago, when, toast, meter,
} from '../core.js';
import { deviceFreshness } from './overview.js';

const RUN_TONE = { ok: 'good', completed: 'good', queued: 'info', delivered: 'info', failed: 'critical', offline: 'warning',
  timeout: 'warning', interrupted: 'warning', retrying: 'info', cancelled: 'neutral', rejected: 'critical', deadlettered: 'critical', expired: 'warning', untracked: 'neutral' };
const RUN_LABEL = { ok: 'Done', failed: 'Device refused', offline: 'Device offline', timeout: 'No answer', interrupted: 'Interrupted', retrying: 'Will retry', cancelled: 'Retries cancelled', queued: 'Queued',
  delivered: 'Delivered', completed: 'Completed', rejected: 'Rejected', deadlettered: 'Undeliverable', expired: 'Expired', untracked: 'Sent' };
export const runBadge = (r, text) => badge(text || RUN_LABEL[r.status] || r.status, RUN_TONE[r.status] || 'neutral');

const cmdLabel = (c) => c.label || c.name;

// Ask for parameters (and confirmation), run, report. Resolves to the run.
// The form for a command's parameters: {els, read()}.
function paramForm(c) {
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
  return { els: inputs.map((x) => x.el), read, any: inputs.length > 0 };
}

export async function runCommand(device, c) {
  const form = paramForm(c);
  const go = () => api(`/api/devices/${enc(device)}/commands/${enc(c.name)}`, { method: 'POST', body: JSON.stringify({ params: form.read() }) });
  let run;
  if (form.any || c.confirm) {
    run = await modal(`${cmdLabel(c)} on ${device}`, h('div', { class: 'stack' },
      c.confirm && h('p', { class: 'modal-text', text: `This sends "${cmdLabel(c)}" to ${device}${c.kind === 'message' ? ' (it is delivered when the device next listens)' : ''}.` }),
      form.els), {
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
  if (ctx.params[0] === 'batch' && ctx.params[1]) return batchView(el, ctx, ctx.params[1]);
  const data = await api('/api/commands');
  data.commands ||= []; data.devices ||= []; data.history ||= []; data.batches ||= [];
  const tab = ['send', 'history', ...(data.canEdit ? ['catalog'] : [])].includes(ctx.params[0]) ? ctx.params[0] : 'send';
  const byName = Object.fromEntries(data.commands.map((c) => [c.name, c]));
  el.append(pageHeader('Commands', {
    sub: 'Send named commands to devices. Admins decide which commands exist and who may run them.',
    actions: data.canEdit && button('New command', () => editCommand(null, ctx), { kind: 'primary', ico: 'plus' }),
  }), tabs([['send', 'Send'], ['history', 'History', data.history.length], ...(data.canEdit ? [['catalog', 'Catalog', data.commands.length]] : [])],
    tab, (k) => { location.hash = '#/commands/' + k; }));

  if (tab === 'history') {
    if (data.batches.length) el.append(card('Sent to many devices', { sub: 'The last 20 batches.' }, batchTable(data.batches)));
    el.append(card(data.batches.length ? 'Every command' : null, {}, historyTable(data.history, { showDevice: true })));
    return;
  }
  if (tab === 'catalog') return catalog(el, ctx, data);

  const q = h('input', { type: 'search', placeholder: 'Find a device', 'aria-label': 'Find a device' });
  const box = h('div');
  const bar = h('div');
  const selected = new Set();
  let shown = [];
  const drawBar = () => {
    bar.replaceChildren();
    if (!selected.size) return;
    const sel = data.devices.filter((d) => selected.has(d.id));
    // Commands offered on at least one selected device; the rest are skipped.
    const offered = data.commands.map((c) => [c, sel.filter((d) => d.commands.includes(c.name)).length]).filter(([, n]) => n > 0);
    const pick = h('select', { 'aria-label': 'Command' }, offered.map(([c, n]) => opt(c.name, `${cmdLabel(c)}${n < sel.length ? ` (applies to ${n} of ${sel.length})` : ''}`)));
    bar.append(h('div', { class: 'bulkbar', role: 'region', 'aria-label': 'Send to selected devices' },
      h('b', { text: `${sel.length} selected` }),
      offered.length ? [pick, button('Send to selected…', () => sendMany(byName[pick.value], sel), { kind: 'primary' })]
        : h('span', { class: 'hint', text: 'No command applies to all of these.' }),
      h('span', { class: 'spacer' }),
      button('Clear selection', () => { selected.clear(); draw(); }, { kind: 'small ghost' })));
  };
  const draw = () => {
    const t = q.value.trim().toLowerCase();
    shown = data.devices.filter((d) => d.commands.length && (!t || d.id.toLowerCase().includes(t))).sort((a, b) => a.id.localeCompare(b.id));
    const all = h('input', { type: 'checkbox', 'aria-label': 'Select all shown', checked: shown.length > 0 && shown.every((d) => selected.has(d.id)),
      onchange: (ev) => { for (const d of shown) { if (ev.target.checked) selected.add(d.id); else selected.delete(d.id); } draw(); } });
    box.replaceChildren(card(null, {}, !data.commands.length
      ? empty('No commands yet', data.canEdit ? 'Define commands such as "Reboot" or "Close valve" once, for the devices they apply to.' : 'An admin has not defined any commands yet.',
        data.canEdit && button('New command', () => editCommand(null, ctx), { kind: 'primary' }))
      : shown.length ? table([{ text: '', cls: 'sel' }, 'Device', 'Status', 'Commands'], shown.map((d) => {
        const [label, tone] = deviceFreshness(d.lastDataTime, d.expectedIntervalSec);
        return h('tr', { class: selected.has(d.id) ? 'selected' : null },
          h('td', { class: 'sel' }, h('input', { type: 'checkbox', 'aria-label': 'Select ' + d.id, checked: selected.has(d.id),
            onchange: (ev) => { if (ev.target.checked) selected.add(d.id); else selected.delete(d.id); draw(); } })),
          h('td', {}, h('div', { class: 'primary-cell' }, h('b', { text: d.id }), h('small', { text: d.lastDataTime ? 'last data ' + ago(d.lastDataTime) : 'no data yet' }))),
          h('td', {}, badge(label, tone), d.connectionState === 'Connected' ? h('span', { class: 'chip', text: 'MQTT connected' }) : null),
          h('td', {}, commandButtons(d.id, d.commands.map((n) => byName[n]).filter(Boolean), () => ctx.reload())));
      })) : empty('No matches', t ? 'No device with commands matches.' : 'No device has a command you may run.')));
    box.querySelector('th.sel')?.append(all);
    drawBar();
  };
  q.oninput = draw;
  el.append(h('div', { class: 'toolbar' }, h('div', { class: 'search' }, icon('search'), q),
    h('span', { class: 'hint', text: 'Tick devices to send one command to all of them. Methods need the device connected over MQTT; messages wait until it listens.' })), bar, box);
  draw();
}

// Send c to many devices: confirm (always: it touches many), then follow
// the batch on its own page.
// Retry controls: {el, read() → {attempts, every}}; attempts 0 = off.
const EVERY = [['30s', '30 seconds'], ['1m', '1 minute'], ['5m', '5 minutes'], ['15m', '15 minutes'], ['1h', '1 hour']];
// Go-style durations, as the server reads them (10s, 5m, 1h30m).
const dur = (t) => { let ms = 0; for (const [, n, u] of String(t).matchAll(/(\d+(?:\.\d+)?)(ms|s|m|h)/g)) ms += n * { ms: 1, s: 1e3, m: 6e4, h: 36e5 }[u]; return ms; };
const fmtDur = (ms) => (ms >= 36e5 ? `${+(ms / 36e5).toFixed(1)} h` : ms >= 6e4 ? `${+(ms / 6e4).toFixed(1)} min` : `${Math.round(ms / 1e3)} s`);
// The wait before retry n, as twin.RetryPolicy.Wait: every × backoff^(n-1), capped.
export function retryWait(p, n) {
  const cap = p.maxEvery ? dur(p.maxEvery) : 24 * 36e5;
  return Math.min(dur(p.every) * Math.pow(p.backoff > 1 ? p.backoff : 1, n - 1), cap);
}
// With jitter, wait n falls in [w(1-j), min(cap, w(1+j))] (twin.RetryPolicy.Jittered).
export function retryRange(p, n) {
  const w = retryWait(p, n), j = p.jitter || 0, cap = p.maxEvery ? dur(p.maxEvery) : 24 * 36e5;
  return [w * (1 - j), Math.min(cap, w * (1 + j))];
}
const retryText = (p) => `every ${p.every}${p.backoff > 1 ? ` ×${p.backoff}` : ''}${p.maxEvery ? ` (max ${p.maxEvery})` : ''}${p.jitter ? ` ±${Math.round(p.jitter * 100)}%` : ''}`;

// Retry controls: {el, read() → policy}; attempts 0 = off.
function retryControls(p) {
  const on = h('input', { type: 'checkbox', checked: !!p?.attempts });
  const attempts = h('select', { 'aria-label': 'Retry attempts' }, [1, 2, 3, 5, 10].map((n) => opt(String(n), `${n} more time${n === 1 ? '' : 's'}`, n === (p?.attempts || 3))));
  const every = h('select', { 'aria-label': 'First wait' }, EVERY.map(([v, t]) => opt(v, 'first after ' + t, v === (p?.every || '5m'))));
  if (p?.every && !EVERY.some(([v]) => v === p.every)) every.append(opt(p.every, 'first after ' + p.every, true));
  const backoff = h('select', { 'aria-label': 'Backoff' }, [[1, 'same wait each time'], [2, 'doubling the wait'], [3, 'tripling the wait']].map(([v, t]) => opt(String(v), t, v === (p?.backoff || 1))));
  if (p?.backoff > 1 && ![2, 3].includes(p.backoff)) backoff.append(opt(String(p.backoff), `×${p.backoff} each time`, true));
  const cap = h('select', { 'aria-label': 'Longest wait' }, [['', 'no longer than 24 h'], ['5m', 'no longer than 5 min'], ['15m', 'no longer than 15 min'], ['1h', 'no longer than 1 h'], ['6h', 'no longer than 6 h']]
    .map(([v, t]) => opt(v, t, v === (p?.maxEvery || ''))));
  if (p?.maxEvery && !['5m', '15m', '1h', '6h'].includes(p.maxEvery)) cap.append(opt(p.maxEvery, 'no longer than ' + p.maxEvery, true));
  const jitter = h('select', { 'aria-label': 'Randomize waits' }, [[0, 'exact waits'], [0.1, 'randomized ±10%'], [0.2, 'randomized ±20%'], [0.5, 'randomized ±50%']]
    .map(([v, t]) => opt(String(v), t, v === (p?.jitter || 0))));
  if (p?.jitter && ![0.1, 0.2, 0.5].includes(p.jitter)) jitter.append(opt(String(p.jitter), `randomized ±${Math.round(p.jitter * 100)}%`, true));
  const preview = h('span', { class: 'hint' });
  const read = () => {
    if (!on.checked) return { attempts: 0 };
    const r = { attempts: Number(attempts.value), every: every.value };
    if (Number(backoff.value) > 1) { r.backoff = Number(backoff.value); if (cap.value) r.maxEvery = cap.value; }
    if (Number(jitter.value) > 0) r.jitter = Number(jitter.value);
    return r;
  };
  const more = h('div', { class: 'stack', style: 'gap:6px' }, h('div', { class: 'row' }, attempts, every, backoff, cap, jitter), preview);
  const sync = () => {
    more.hidden = !on.checked;
    cap.hidden = Number(backoff.value) <= 1;
    const r = read();
    if (!r.attempts) return;
    const ranges = Array.from({ length: r.attempts }, (_, i) => retryRange(r, i + 1));
    const one = ([a, b]) => (a === b ? fmtDur(a) : `${fmtDur(a)}–${fmtDur(b)}`);
    const total = [ranges.reduce((x, [a]) => x + a, 0), ranges.reduce((x, [, b]) => x + b, 0)];
    preview.textContent = `Waits ${ranges.map(one).join(', ')} between tries; the last retry comes about ${one(total)} after the first try (plus each round's own time).`
      + (r.jitter ? ' Randomizing keeps batches started together from retrying in lockstep.' : '');
  };
  for (const x of [on, attempts, every, backoff, cap, jitter]) x.onchange = sync;
  sync();
  return {
    el: h('div', { class: 'stack', style: 'gap:6px' }, field('Retry devices that are offline or don\'t answer', on, { cls: 'check' }), more,
      h('span', { class: 'hint', text: 'Only offline, no answer, errors and restarts are retried; a device that refuses is not.' })),
    read,
  };
}

async function sendMany(c, devices) {
  const run = devices.filter((d) => d.commands.includes(c.name)).map((d) => d.id);
  const skipped = devices.length - run.length;
  const form = paramForm(c);
  const retry = c.kind === 'method' ? retryControls(c.retry) : null;
  const list = run.slice(0, 12).join(', ') + (run.length > 12 ? ` and ${run.length - 12} more` : '');
  const b = await modal(`${cmdLabel(c)} on ${run.length} device${run.length === 1 ? '' : 's'}`, h('div', { class: 'stack' },
    h('p', { class: 'modal-text', text: `This sends "${cmdLabel(c)}" to ${list}.${c.kind === 'message' ? ' Each is delivered when the device next listens.' : ''}` }),
    skipped ? h('p', { class: 'hint', text: `${skipped} selected device${skipped === 1 ? ' does' : 's do'} not offer this command and will be skipped.` }) : null,
    form.els, retry?.el), {
    actions: [['Cancel'], [`Send to ${run.length}`, () => api(`/api/commands/${enc(c.name)}/run`, { method: 'POST',
      body: JSON.stringify({ devices: run, params: form.read(), ...(retry ? { retry: retry.read() } : {}) }) }),
      c.confirm ? 'danger' : 'primary']],
  });
  if (!b?.id) return;
  location.hash = '#/commands/batch/' + enc(b.id);
}

function batchTable(batches) {
  return table(['When', 'Command', 'By', 'Progress', 'Results'], batches.map((b) => h('tr', { class: 'link', onclick: () => { location.hash = '#/commands/batch/' + enc(b.id); } },
    h('td', { title: when(b.at), text: ago(b.at) }),
    h('td', {}, h('a', { href: '#/commands/batch/' + enc(b.id), text: b.label || b.command })),
    h('td', { text: b.by }),
    h('td', { text: b.finished ? `${b.cancelled ? 'cancelled' : b.interrupted ? 'interrupted' : 'done'}, ${b.total} devices`
      : b.nextRetry ? `${b.done} of ${b.total}, retrying ${new Date(b.nextRetry).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}` : `${b.done} of ${b.total}` }),
    h('td', {}, countBadges(b.counts)))));
}

function countBadges(counts) {
  return Object.entries(counts || {}).sort().map(([k, n]) => [runBadge({ status: k === 'skipped' ? 'untracked' : k }, `${n} ${RUN_LABEL[k] || k}`), ' ']);
}

// Definite failures, resent by "Resend to failed" (twin.Retryable).
const RETRYABLE = new Set(['offline', 'timeout', 'failed', 'error', 'interrupted', 'cancelled', 'rejected', 'deadlettered', 'expired']);

async function resend(b) {
  const failed = b.results.filter((r) => RETRYABLE.has(r.status));
  const ok = await confirmDialog(`Resend ${b.label || b.command} to ${failed.length} device${failed.length === 1 ? '' : 's'}?`, '', {
    ok: `Resend to ${failed.length}`,
    details: h('div', { class: 'stack' },
      h('p', { class: 'modal-text', text: 'The same command and parameters go again to the devices that failed. Devices that succeeded, are still waiting, or were skipped are left alone.' }),
      b.params && Object.keys(b.params).length ? h('p', {}, 'Parameters: ', h('span', { class: 'chip mono', text: JSON.stringify(b.params) })) : null,
      h('ul', { class: 'plan' }, failed.slice(0, 15).map((r) => h('li', {}, h('b', { text: r.device }), ' — ', RUN_LABEL[r.status] || r.status)),
        failed.length > 15 ? h('li', { text: `and ${failed.length - 15} more` }) : null)),
  });
  if (!ok) return;
  try {
    const nb = await api(`/api/commands/batches/${enc(b.id)}/resend`, { method: 'POST' });
    location.hash = '#/commands/batch/' + enc(nb.id);
  } catch (e) { toast(e.message, 'bad'); }
}

async function batchView(el, ctx, id) {
  ctx.crumbs([['Batch']]);
  const head = h('div'), body = h('div');
  el.append(head, body);
  const draw = (b) => {
    const failed = b.results.filter((r) => RETRYABLE.has(r.status)).length;
    const link = (x, text) => h('a', { href: '#/commands/batch/' + enc(x), text });
    head.replaceChildren(pageHeader(`${b.label || b.command} on ${b.total} device${b.total === 1 ? '' : 's'}`, {
      back: ['#/commands/history', 'Commands'],
      actions: b.finished && failed ? button(`Resend to ${failed} failed`, () => resend(b), { kind: 'primary' })
        : !b.finished && b.retry && !b.cancelled ? button('Cancel retries', async () => {
          if (!await confirmDialog('Cancel the remaining retries?', 'Devices waiting for another attempt are left as they are. Calls already in flight finish.', { ok: 'Cancel retries', danger: true })) return;
          await api(`/api/commands/batches/${enc(b.id)}/cancel`, { method: 'POST' }); toast('Retries cancelled'); ctx.reload();
        }) : null,
      sub: h('span', { class: 'page-sub' }, b.cancelled ? badge('Retries cancelled', 'neutral') : b.finished ? (b.interrupted ? badge('Interrupted by a restart', 'warning') : badge('Finished', 'good'))
          : b.nextRetry ? badge(`Retrying at ${new Date(b.nextRetry).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })}`, 'info') : badge('Running', 'info'),
        b.retry && h('span', { class: 'chip', text: `round ${b.round} of ${b.retry.attempts + 1} · ${retryText(b.retry)}` }),
        h('span', { text: `sent by ${b.by} ${ago(b.at)}` }), b.params && Object.keys(b.params).length ? h('span', { class: 'chip mono', text: JSON.stringify(b.params) }) : null,
        b.retryOf ? h('span', {}, 'resend of ', link(b.retryOf, 'an earlier batch')) : null,
        b.retries?.length ? h('span', {}, 'resent: ', b.retries.map((x, i) => [i ? ', ' : '', link(x, `#${i + 1}`)])) : null),
    }));
    const rank = { pending: 0, retrying: 0, error: 1, failed: 2, offline: 3, timeout: 4, skipped: 6 };
    const rows = [...b.results].sort((x, y) => (rank[x.status] ?? 5) - (rank[y.status] ?? 5) || x.device.localeCompare(y.device));
    body.replaceChildren(
      card(null, {}, h('div', { class: 'stack' },
        h('div', { class: 'row' }, h('b', { text: `${b.done} of ${b.total} done` }), h('span', { class: 'spacer' }), countBadges(b.counts)),
        meter(b.done, b.total, { tone: 'progress' }))), // a progress bar: never warning-coloured
      card(null, {}, table(['Device', 'Result', ...(b.retry ? [{ text: 'Attempts', cls: 'n' }] : []), 'Details'], rows.map((r) => h('tr', {},
        h('td', {}, h('b', { text: r.device })),
        h('td', {}, r.status === 'pending' ? badge('Waiting…', 'info') : r.status === 'skipped' ? badge('Skipped', 'neutral') : runBadge(r)),
        b.retry && h('td', { class: 'n', text: r.attempts || '' }),
        h('td', { class: 'mono', text: [r.code || '', r.error || ''].filter(Boolean).join(' ') }))))));
  };
  let b = await api('/api/commands/batches/' + enc(id));
  draw(b);
  // Follow it until every device has answered.
  while (!b.finished && ctx.current()) {
    await new Promise((r) => setTimeout(r, b.nextRetry ? 5000 : 1000)); // slower while waiting for a retry
    if (!ctx.current()) return;
    b = await api('/api/commands/batches/' + enc(id)).catch(() => b);
    draw(b);
  }
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
  const retry = retryControls(c.retry);
  const retryField = h('div', {}, h('span', { class: 'field-label', text: 'Default for batches' }), retry.el);
  const methodField = field('Method name', method), timeoutField = field('Wait for the answer', timeout), ttlField = field('Delivery', ttl);
  const syncKind = () => { methodField.hidden = timeoutField.hidden = retryField.hidden = kind.value !== 'method'; ttlField.hidden = kind.value !== 'message'; };
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
    h('div', { class: 'form-grid' }, field('Who may run it', role), h('div', { class: 'field', style: 'justify-content:end' }, field('Ask for confirmation', confirm, { cls: 'check' }))),
    retryField), {
    wide: true,
    actions: [['Cancel'], ['Save command', async () => {
      let pl;
      if (payload.value.trim()) { try { pl = JSON.parse(payload.value); } catch (e) { throw new Error('Payload is not valid JSON: ' + e.message); } }
      const body = { name: name.value.trim(), label: label.value.trim() || undefined, kind: kind.value,
        devices: devices.value.split(',').map((x) => x.trim()).filter(Boolean), payload: pl,
        params: [...params.children].map((r) => r.read()).filter((p) => p.name), role: role.value, confirm: confirm.checked || undefined };
      if (kind.value === 'method') {
        body.method = method.value.trim() || undefined; body.timeout = timeout.value;
        const r = retry.read(); if (r.attempts) body.retry = r;
      } else body.ttl = ttl.value;
      if (!body.name) throw new Error('Give the command an id.');
      return put('/api/commands/' + enc(body.name), body);
    }, 'primary']],
  });
  if (!ok) return;
  toast('Command saved');
  if (location.hash === '#/commands/catalog') ctx.reload(); else location.hash = '#/commands/catalog';
}

