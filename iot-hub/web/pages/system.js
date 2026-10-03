// System: audit log, backups, configuration export/import, edge
// forwarding and shift reports.
import {
  h, api, post, enc, pageHeader, card, badge, empty, table, field, opt, button, icon, tabs, linkButton, confirmDialog,
  session, when, ago, bytes, num, duration, toast,
} from '../core.js';

// Go durations (24h0m0s) as people write them (24h).
const goDur = (d) => String(d).replace(/(\d+h)0m0s$/, '$1').replace(/(\d+m)0s$/, '$1');
const withTenant = (path) => (session.tenant ? path + (path.includes('?') ? '&' : '?') + 'tenant=' + enc(session.tenant) : path);

export async function render(el, ctx) {
  const tab = ['general', 'audit', 'backups'].includes(ctx.params[0]) ? ctx.params[0] : 'general';
  el.append(pageHeader('System', { sub: 'Audit trail, backups, configuration and data forwarding.' }),
    tabs([['general', 'General'], ['audit', 'Audit log'], ['backups', 'Backups & config']], tab, (k) => { location.hash = '#/system/' + k; }));
  const body = h('div');
  el.append(body);
  if (tab === 'audit') return audit(body);
  if (tab === 'backups') return backups(body, ctx);
  return general(body);
}

async function general(body) {
  const [health, fwd, report] = await Promise.all([
    api('/api/health').catch(() => null),
    api('/api/forward').catch(() => null),
    api('/api/reports').catch(() => null),
  ]);
  body.append(h('div', { class: 'grid-2' },
    card('About this hub', {}, h('dl', { class: 'dl' },
      h('dt', { text: 'Status' }), h('dd', {}, health?.status === 'ok' ? badge('Healthy', 'good') : badge(health?.status || 'Unknown', 'warning')),
      h('dt', { text: 'Mode' }), h('dd', { text: health?.mode || (session.me.multiTenant ? 'multi-tenant' : 'single') }),
      h('dt', { text: 'Up for' }), h('dd', { text: health?.uptimeSec != null ? duration(health.uptimeSec * 1000) : '—' }),
      h('dt', { text: 'Signed in as' }), h('dd', { text: session.me.identity ? `${session.me.identity.id} (${session.me.identity.role})` : 'Authentication off' }))),
    card('Edge forwarding', { sub: 'Store-and-forward from this hub to a cloud tenant.' }, fwd
      ? h('dl', { class: 'dl' },
        h('dt', { text: 'Upstream' }), h('dd', { class: 'mono', text: fwd.url }),
        h('dt', { text: 'Link' }), h('dd', {}, fwd.connected ? badge('Connected', 'good') : badge('Disconnected', 'critical')),
        h('dt', { text: 'Queued' }), h('dd', { text: bytes(fwd.queuedBytes) }),
        h('dt', { text: 'Sent' }), h('dd', { text: num(fwd.sent) + (fwd.lastSentAt ? ` · last ${ago(fwd.lastSentAt)}` : '') }),
        h('dt', { text: 'Rejected / dropped' }), h('dd', { text: `${num(fwd.rejected)} / ${num(fwd.dropped)}` }),
        fwd.lastError && h('dt', { text: 'Last error' }), fwd.lastError && h('dd', { class: 'err', text: fwd.lastError }))
      : h('p', { class: 'hint', text: 'Off. Start the hub with -forward <cloud URL> to queue readings on disk and send them upstream.' }))),
  report && card(report.title || 'Shift report', {
    sub: `${report.period} · generated ${ago(report.generated)}`,
    actions: [button('Send now', async () => {
      if (!await confirmDialog('Send the shift report now?', 'It goes to the report targets set under Notifications.', { ok: 'Send' })) return;
      await post('/api/reports/send'); toast('Report sent');
    }, { kind: 'small' })],
  }, h('dl', { class: 'dl' },
    h('dt', { text: 'Alarms' }), h('dd', { text: `${report.alarms.total} (${report.alarms.unacknowledged} unacknowledged, ${report.alarms.shelved} shelved)` }),
    h('dt', { text: 'Machines (OEE)' }), h('dd', { text: report.machines.length ? report.machines.map((m) => `${m.name || m.sensor}${m.result?.oee != null ? ' ' + Math.round(m.result.oee * 100) + '%' : ''}`).join(', ') : 'None tracked' }),
    h('dt', { text: 'Energy meters' }), h('dd', { text: report.energy.length ? String(report.energy.length) : 'None' }),
    h('dt', { text: 'Silent sensors' }), h('dd', { text: report.silentSensors.length ? report.silentSensors.join(', ') : 'None' }))));
}

const auditFilter = { q: '' };
async function audit(body) {
  const list = await api('/api/audit?limit=1000');
  const q = h('input', { type: 'search', placeholder: 'Filter by who, action or target', 'aria-label': 'Filter audit log', value: auditFilter.q });
  const box = h('div');
  const draw = () => {
    const t = q.value.trim().toLowerCase();
    const rows = list.filter((e) => !t || `${e.actor} ${e.action} ${e.target} ${e.detail || ''}`.toLowerCase().includes(t)).slice(0, 500)
      .map((e) => h('tr', {}, h('td', { text: when(e.ts) }), h('td', {}, h('b', { text: e.actor })), h('td', {}, h('span', { class: 'chip mono', text: e.action })),
        h('td', { class: 'mono', text: e.target }), h('td', { class: 'muted', text: e.detail || '' })));
    box.replaceChildren(card(null, {}, list.length ? table(['When', 'Who', 'Action', 'Target', 'Detail'], rows, { empty: 'Nothing matches.' })
      : empty('No entries yet', 'Changes to sensors, devices, settings and alarms are recorded here.')));
  };
  q.oninput = () => { auditFilter.q = q.value; draw(); };
  body.append(h('div', { class: 'toolbar' }, h('div', { class: 'search' }, icon('search'), q), h('span', { class: 'hint', text: `Most recent ${Math.min(list.length, 1000)} entries` })), box);
  draw();
}

async function backups(body, ctx) {
  let st = null, off = null;
  try { st = await api('/api/backups'); } catch (e) { off = e.message; }
  const file = h('input', { type: 'file', accept: 'application/json,.json' });
  const mode = h('select', {}, opt('merge', 'Merge into the current configuration'), opt('replace', 'Replace the current configuration'));
  const importCfg = async () => {
    const f = file.files[0];
    if (!f) throw new Error('Choose a configuration file first.');
    const text = await f.text();
    try { JSON.parse(text); } catch { throw new Error('That file is not JSON.'); }
    // Dry run first: show exactly what would change, then apply.
    const path = withTenant('/api/config?mode=' + mode.value);
    const plan = await api(path + '&dryRun=1', { method: 'POST', body: text });
    const line = (label, xs) => xs?.length ? h('li', {}, h('b', { text: `${label} (${xs.length}): ` }), xs.slice(0, 30).join(', ') + (xs.length > 30 ? ' …' : '')) : null;
    const items = [line('Create', plan.created), line('Update', plan.updated), line('Delete', plan.deleted),
      plan.dashboard ? h('li', { text: 'Replace the dashboard layout' }) : null,
      plan.devices ? h('li', { text: `${plan.devices} device credential(s)` }) : null, line('Disconnect', plan.revokedDevices)].filter(Boolean);
    const ok = await confirmDialog(mode.value === 'replace' ? 'Replace the configuration?' : 'Import this configuration?',
      items.length ? '' : 'The file matches the current configuration; nothing would change.',
      { ok: 'Import', danger: !!(plan.deleted?.length || plan.revokedDevices?.length), details: items.length ? h('ul', { class: 'plan' }, items) : null });
    if (!ok || !items.length) return;
    await api(path, { method: 'POST', body: text });
    toast('Configuration imported');
  };
  body.append(card('Configuration', { sub: 'Sensor definitions (fields, limits, calculated fields, OEE) and the dashboard layout as one JSON file. Readings are not included. Importing shows what would change before anything is applied.' },
    h('div', { class: 'stack' },
      h('div', { class: 'row' }, linkButton('Export configuration', withTenant('/api/config'), { ico: 'download' })),
      h('div', { class: 'form-grid' }, field('Import a file', file), field('How', mode)),
      h('div', { class: 'row' }, button('Import', importCfg, { kind: 'primary' })))));
  body.append(card('Backups', { sub: off ? null : `Every ${goDur(st.every)}, keeping ${st.keep}${st.database ? ', including readings' : ' (configuration only)'}.`,
    actions: st && button('Back up now', async () => { const r = await post('/api/backups'); toast(`Backed up ${r.files.length} file(s)`); ctx.reload(); }, { kind: 'small' }) },
  off ? h('p', { class: 'hint', text: 'Scheduled backups are off. Start the hub with -backup-dir to keep regular snapshots of the configuration and readings.' })
    : h('div', {},
      st.lastError ? h('p', { class: 'err', text: 'Last backup failed: ' + st.lastError }) : st.lastAt ? h('p', { class: 'hint', text: 'Last backup ' + ago(st.lastAt) }) : null,
      table(['File', { text: 'Size', cls: 'n' }, 'Taken', ''], (st.files || []).map((f) => h('tr', {},
        h('td', { class: 'mono', text: f.name }), h('td', { class: 'n', text: bytes(f.size) }), h('td', { text: when(f.modified) }),
        h('td', { class: 'actions' }, linkButton('Download', withTenant('/api/backups/' + enc(f.name)), { kind: 'small', ico: 'download' })))),
      { empty: 'No backups yet.' }))));
}
