// Tenants (platform operator): create, quotas, suspend, Grafana, delete.
import {
  h, api, post, patch, del, enc, pageHeader, card, badge, empty, table, field, button, modal, confirmDialog, meter,
  session, num, when, toast,
} from '../core.js';
import { switchTenant, refreshMe } from '../app.js';

export async function render(el, ctx) {
  const list = await api('/api/admin/tenants');
  const reload = async () => { await refreshMe().catch(() => {}); ctx.reload(); };
  const rows = list.map((t) => {
    const q = t.effectiveQuota;
    const g = t.grafana;
    const use = (n, max) => h('div', { style: 'min-width:110px' }, h('div', { class: 'hint', style: 'margin:0', text: `${num(n)} / ${max > 0 ? num(max) : '∞'}` }), max > 0 && meter(n, max));
    return h('tr', {},
      h('td', {}, h('div', { class: 'primary-cell' }, h('b', { text: t.name || t.id }), h('small', { class: 'mono', text: t.id }))),
      h('td', {}, t.status === 'active' ? badge('Active', 'good') : badge('Suspended', 'critical')),
      h('td', {}, use(t.sensors, q.maxSensors)),
      h('td', {}, use(t.devices, q.maxDevices)),
      h('td', {}, use(t.messagesToday, q.messagesPerDay), t.messagesRejected ? h('div', { class: 'hint', text: `${num(t.messagesRejected)} rejected` }) : null),
      h('td', {}, t.deviceSelfService ? 'Tenant' : 'Platform'),
      session.me.grafana && h('td', {}, g?.orgId && !g.error ? h('a', { href: g.url, target: '_blank', rel: 'noopener' }, 'Org ' + g.orgId)
        : g?.error ? badge('Failed', 'critical') : badge('None', 'neutral')),
      h('td', { class: 'actions' },
        t.status === 'active' && button('Open', () => { switchTenant(t.id); location.hash = '#/overview'; }, { kind: 'small' }),
        button('Manage', () => manage(t, reload), { kind: 'small' })));
  });
  el.append(
    pageHeader('Tenants', {
      sub: 'Each tenant is an isolated hub with its own data, devices, settings and quotas.',
      actions: button('New tenant', () => tenantForm(null, reload), { kind: 'primary', ico: 'plus' }),
    }),
    card(null, {}, list.length
      ? table(['Tenant', 'Status', 'Sensors', 'Devices', 'Messages today', 'Credentials by', ...(session.me.grafana ? ['Grafana'] : []), ''], rows)
      : empty('No tenants yet', 'Create the first tenant, then issue its admin a token and its devices their connection strings.',
        button('New tenant', () => tenantForm(null, reload), { kind: 'primary' }))));
}

async function manage(t, reload) {
  const g = t.grafana;
  const d = {}; // d.close() once the dialog is open
  const body = h('div', { class: 'stack' },
    h('dl', { class: 'dl' },
      h('dt', { text: 'Id' }), h('dd', { class: 'mono', text: t.id }),
      h('dt', { text: 'Created' }), h('dd', { text: when(t.created) }),
      h('dt', { text: 'Status' }), h('dd', {}, t.status === 'active' ? badge('Active', 'good') : badge('Suspended', 'critical')),
      t.note && h('dt', { text: 'Note' }), t.note && h('dd', { text: t.note }),
      session.me.grafana && h('dt', { text: 'Grafana' }), session.me.grafana && h('dd', { text: g?.orgId && !g.error ? `Organization ${g.orgId} (${g.org})` : g?.error ? 'Failed: ' + g.error : 'Not set up' })),
    h('div', { class: 'row' },
      button('Edit plan', async () => { d.close(); await tenantForm(t, reload); }),
      session.me.grafana && button(g?.orgId && !g.error ? 'Repair Grafana' : 'Set up Grafana', async () => {
        await post('/api/admin/tenants/' + enc(t.id) + '/grafana'); toast('Grafana ready for ' + t.id); d.close(); reload();
      }),
      button(t.status === 'active' ? 'Suspend' : 'Resume', async () => {
        if (t.status === 'active' && !await confirmDialog(`Suspend ${t.name || t.id}?`, 'Its devices and users are refused until it is resumed. Data is kept.', { ok: 'Suspend', danger: true })) return;
        await patch('/api/admin/tenants/' + enc(t.id), { status: t.status === 'active' ? 'suspended' : 'active' });
        toast(t.status === 'active' ? 'Suspended' : 'Resumed'); d.close(); reload();
      }),
      h('span', { class: 'spacer' }),
      button('Delete tenant', async () => {
        if (!await confirmDialog(`Delete ${t.name || t.id}?`, `This erases all of ${t.id}'s data, credentials, settings, backups and Grafana organization. It cannot be undone.`, { ok: 'Delete forever', danger: true, typed: t.id })) return;
        await del('/api/admin/tenants/' + enc(t.id) + '?confirm=' + enc(t.id)); toast('Deleted ' + t.id); d.close(); reload();
      }, { kind: 'danger' })));
  await modal(t.name || t.id, body, { wide: true, ref: d });
}

async function tenantForm(t, reload) {
  const q = t?.quota || {};
  const id = h('input', { value: t?.id || '', readonly: !!t, placeholder: 'acme', spellcheck: 'false' });
  const name = h('input', { value: t?.name || '', placeholder: 'Acme Mining' });
  const note = h('input', { value: t?.note || '', placeholder: 'Only the platform sees this' });
  const n = (v) => h('input', { type: 'number', value: v ?? '', placeholder: 'default' });
  const rate = n(q.messagesPerSecond), daily = n(q.messagesPerDay), sensors = n(q.maxSensors), devices = n(q.maxDevices),
    raw = n(q.rawRetentionDays), rollup = n(q.rollupRetentionDays), jobs = n(q.maxJobs);
  const self = h('input', { type: 'checkbox', checked: t?.deviceSelfService });
  const val = (i) => (i.value === '' ? undefined : Number(i.value));
  const ok = await modal(t ? 'Edit ' + (t.name || t.id) : 'New tenant', h('div', { class: 'stack' },
    h('div', { class: 'form-grid' },
      field('Tenant id', id, { hint: t ? null : '2–40 of a-z, 0-9, -. Used in MQTT topics; cannot change.' }),
      field('Display name', name), field('Internal note', note, { cls: 'full' })),
    field('Tenant admins issue device credentials', self, { cls: 'check' }),
    h('h3', { style: 'margin:6px 0 0;font-size:13px', text: 'Plan' }),
    h('p', { class: 'hint', style: 'margin:0', text: 'Empty uses the platform default; -1 means unlimited.' }),
    h('div', { class: 'form-grid' }, field('Messages / second', rate), field('Messages / day', daily), field('Sensors', sensors),
      field('Devices and users', devices), field('Raw data days', raw), field('Rollup days', rollup), field('Stream jobs', jobs))), {
    wide: true,
    actions: [['Cancel'], [t ? 'Save' : 'Create tenant', async () => {
      const quota = { messagesPerSecond: val(rate), messagesPerDay: val(daily), maxSensors: val(sensors), maxDevices: val(devices),
        rawRetentionDays: val(raw), rollupRetentionDays: val(rollup), maxJobs: val(jobs) };
      if (t) await patch('/api/admin/tenants/' + enc(t.id), { name: name.value, note: note.value, quota, deviceSelfService: self.checked });
      else await post('/api/admin/tenants', { id: id.value.trim(), name: name.value.trim() || undefined, note: note.value.trim() || undefined, quota, deviceSelfService: self.checked });
    }, 'primary']],
  });
  if (!ok) return;
  toast(t ? 'Saved' : 'Tenant created');
  await reload();
}
