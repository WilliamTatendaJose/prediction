// Grafana: this tenant's own Grafana organization and its users.
import {
  h, api, put, del, enc, pageHeader, card, badge, empty, table, field, opt, button, linkButton, modal, confirmDialog, showSecrets, tenantId, toast,
} from '../core.js';

export async function render(el, ctx) {
  const g = await api('/api/grafana');
  if (!g.grafana || g.grafana.error) {
    el.append(pageHeader('Grafana'), empty(g.grafana?.error ? 'Grafana setup failed' : 'Grafana is not set up yet',
      g.grafana?.error ? `${g.grafana.error} The platform operator can retry it from Tenants.` : 'The platform operator sets up a Grafana organization for this tenant.'));
    return;
  }
  const prefix = tenantId() + '.';
  const addUser = async () => {
    const login = h('input', { placeholder: 'ana', spellcheck: 'false' });
    const email = h('input', { type: 'email', placeholder: 'ana@plant.co (optional)' });
    const role = h('select', {}, opt('Viewer', 'Viewer — looks at dashboards'), opt('Editor', 'Editor — builds dashboards and queries'));
    const r = await modal('Add Grafana user', h('div', { class: 'stack' },
      field('Login', login, { hint: `They sign in to Grafana as ${prefix}<login>` }), field('Email', email), field('Role', role)), {
      actions: [['Cancel'], ['Add user', () => {
        if (!login.value.trim()) throw new Error('Give a login.');
        return put('/api/grafana/users/' + enc(login.value.trim()), { role: role.value, email: email.value.trim() || undefined });
      }, 'primary']],
    });
    if (!r) return;
    if (r.password) await showSecrets('Grafana sign-in for ' + r.login, r, 'The password is shown only once. Pass it on securely; they can change it in Grafana.');
    else toast('Role updated');
    ctx.reload();
  };
  const rows = (g.users || []).map((u) => h('tr', {},
    h('td', {}, h('div', { class: 'primary-cell' }, h('b', { text: prefix + u.login }), u.email && h('small', { text: u.email }))),
    h('td', {}, badge(u.role, u.role === 'Editor' ? 'info' : 'neutral')),
    h('td', { class: 'actions' },
      button(u.role === 'Viewer' ? 'Make editor' : 'Make viewer', async () => {
        await put('/api/grafana/users/' + enc(u.login), { role: u.role === 'Viewer' ? 'Editor' : 'Viewer' }); ctx.reload();
      }, { kind: 'small' }),
      button('Remove', async () => {
        if (!await confirmDialog(`Remove ${prefix + u.login}?`, 'They can no longer sign in to Grafana.', { ok: 'Remove', danger: true })) return;
        await del('/api/grafana/users/' + enc(u.login)); ctx.reload();
      }, { kind: 'small danger' }))));
  el.append(
    pageHeader('Grafana', {
      sub: 'This tenant has its own Grafana organization with the IoT Hub dashboards. Its data source can only read this tenant\'s data, so editors may write their own SQL.',
      actions: [linkButton('Open Grafana', g.grafana.url, { ico: 'external', target: '_blank' }), button('Add user', addUser, { kind: 'primary', ico: 'plus' })],
    }),
    card('Users', { sub: 'Editors can build dashboards and write queries; viewers only look. Organization admin stays with the platform.' },
      rows.length ? table(['Login', 'Role', ''], rows) : empty('No Grafana users yet', 'Add people who should see this tenant\'s data in Grafana.', button('Add user', addUser, { kind: 'primary' }))));
}
