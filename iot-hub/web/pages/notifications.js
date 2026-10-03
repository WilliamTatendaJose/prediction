// Notifications: where alarms and reports go (targets), when (rules,
// escalation), and shift reports.
import {
  h, api, post, put, del, enc, pageHeader, card, badge, empty, table, field, opt, button, modal, confirmDialog, when, toast,
} from '../core.js';

const KINDS = [['teams', 'Microsoft Teams'], ['slack', 'Slack'], ['email', 'Email (SMTP)'], ['webhook', 'Webhook'], ['discord', 'Discord'], ['telegram', 'Telegram']];
const PLACEHOLDER = { email: 'smtp://user:pass@host:587?from=hub@plant.co&to=ops@plant.co', telegram: 'https://api.telegram.org/bot<token>/sendMessage?chat_id=…' };

export async function render(el, ctx) {
  const { settings: st, recentEscalations } = await api('/api/settings');
  st.notify.targets ||= []; st.notify.kinds ||= []; st.reports.targets ||= []; st.escalation.levels ||= []; st.shifts ||= []; st.targets ||= [];
  const ids = st.targets.map((t) => t.id);

  const addTarget = async () => {
    const id = h('input', { placeholder: 'ops-teams', spellcheck: 'false' });
    const kind = h('select', {}, KINDS.map(([k, l]) => opt(k, l)));
    const url = h('input', { placeholder: 'https://…', spellcheck: 'false' });
    kind.onchange = () => { url.placeholder = PLACEHOLDER[kind.value] || 'https://…'; };
    const ok = await modal('Add notification target', h('div', { class: 'stack' },
      h('div', { class: 'form-grid' }, field('Name', id), field('Kind', kind)),
      field('Address', url, { hint: 'Webhook URL or SMTP address. It holds a secret and is never shown again.' })), {
      actions: [['Cancel'], ['Add target', async () => {
        if (!id.value.trim() || !url.value.trim()) throw new Error('Give a name and an address.');
        await put('/api/settings/targets/' + enc(id.value.trim()), { spec: kind.value + '=' + url.value.trim() });
      }, 'primary']],
    });
    if (ok) { toast('Target added'); ctx.reload(); }
  };

  const targetRows = st.targets.map((t) => h('tr', {},
    h('td', {}, h('div', { class: 'primary-cell' }, h('b', { text: t.id }), h('small', { text: t.display }))),
    h('td', {}, t.lastError ? badge('Failing', 'critical') : t.sent ? badge('Working', 'good') : badge('Not used yet', 'neutral')),
    h('td', { class: 'n', text: t.sent }), h('td', { class: 'n', text: t.failed }),
    h('td', { class: 'err', text: t.lastError || '' }),
    h('td', { class: 'actions' },
      button('Send test', async () => { await post('/api/settings/targets/' + enc(t.id) + '/test'); toast('Test sent to ' + t.id); }, { kind: 'small' }),
      button('Remove', async () => {
        if (!await confirmDialog(`Remove ${t.id}?`, 'Alarm rules and reports stop sending to it.', { ok: 'Remove', danger: true })) return;
        await del('/api/settings/targets/' + enc(t.id)); ctx.reload();
      }, { kind: 'small danger' }))));

  const picks = (selected) => {
    const box = h('div', { class: 'checks' });
    for (const id of ids) box.append(h('label', {}, h('input', { type: 'checkbox', value: id, checked: selected?.includes(id) }), id));
    if (!ids.length) box.append(h('span', { class: 'hint', text: 'Add a target first.' }));
    box.read = () => [...box.querySelectorAll('input:checked')].map((c) => c.value);
    return box;
  };
  const nTargets = picks(st.notify.targets);
  const kinds = h('div', { class: 'checks' });
  for (const [k, l] of [['range', 'Range limits'], ['spike', 'Spikes'], ['stale', 'Stale data'], ['rule', 'Rules and jobs']]) {
    kinds.append(h('label', {}, h('input', { type: 'checkbox', value: k, checked: !st.notify.kinds.length || st.notify.kinds.includes(k) }), l));
  }
  const resolved = h('input', { type: 'checkbox', checked: st.notify.resolved });
  const cooldown = h('input', { type: 'number', min: '0', value: st.notify.cooldownMin || 10 });
  const levels = h('div', { class: 'stack' });
  const levelRow = (l = { afterMin: '', targets: [] }) => {
    const after = h('input', { type: 'number', min: '1', value: l.afterMin, style: 'width:90px' });
    const tg = picks(l.targets);
    const row = h('div', { class: 'shelf' }, h('div', { class: 'row' }, h('span', { text: 'After' }), after, h('span', { text: 'minutes unacknowledged, notify' })), tg,
      h('button', { class: 'btn ghost small danger', type: 'button', onclick: () => row.remove() }, 'Remove'));
    row.read = () => ({ afterMin: Number(after.value), targets: tg.read() });
    return row;
  };
  for (const l of st.escalation.levels) levels.append(levelRow(l));
  const repeat = h('input', { type: 'number', min: '0', value: st.escalation.repeatMin || 0 });
  const rTargets = picks(st.reports.targets);
  const shifts = h('input', { value: st.shifts.join(', '), placeholder: '06:00, 14:00, 22:00' });
  const tz = h('input', { value: st.timeZone || '', placeholder: 'Africa/Harare' });
  const secret = h('input', { type: 'password', placeholder: st.webhookSecretSet ? 'Set — leave empty to keep' : 'None', autocomplete: 'new-password' });
  const err = h('p', { class: 'err', role: 'alert' });

  const save = async () => {
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
    try { await put('/api/settings', body); toast('Settings saved'); ctx.reload(); } catch (e) { err.textContent = e.message; }
  };

  el.append(
    pageHeader('Notifications', { sub: 'Who hears about alarms, when it escalates, and where shift reports go.' }),
    card('Targets', { sub: 'Each destination is named once and then used by the rules below.', actions: button('Add target', addTarget, { kind: 'small', ico: 'plus' }) },
      st.targets.length ? table(['Target', 'Health', { text: 'Sent', cls: 'n' }, { text: 'Failed', cls: 'n' }, 'Last error', ''], targetRows)
        : empty('No targets yet', 'Add a Teams, Slack, email or webhook target to send alarms and reports to.', button('Add target', addTarget, { kind: 'primary' }))),
    h('div', { class: 'grid-2' },
      card('When an alarm opens', {},
        h('div', { class: 'stack' },
          field('Notify', nTargets), field('For these kinds', kinds),
          h('div', { class: 'form-grid' }, field('Wait between repeats (min)', cooldown), h('div', { class: 'field', style: 'justify-content:end' }, field('Also when it clears', resolved, { cls: 'check' }))))),
      card('Escalation', { sub: 'While nobody acknowledges, each level notifies its targets once its delay passes. Acknowledging or shelving stops it.' },
        h('div', { class: 'stack' }, levels,
          h('div', { class: 'row top' }, button('Add level', () => levels.append(levelRow()), { kind: 'small', ico: 'plus' }), h('span', { class: 'spacer' }),
            field('Repeat the last level every (min, 0 = never)', repeat)),
          recentEscalations?.length ? h('p', { class: 'hint', text: `${recentEscalations.length} escalation(s) sent recently; latest ${when(recentEscalations.at(-1).at)}.` }) : null))),
    card('Shift reports and plant time', { sub: 'A report of production, energy and alarms is sent at each shift change.' },
      h('div', { class: 'stack' }, field('Send to', rTargets),
        h('div', { class: 'form-grid' }, field('Shifts start at', shifts), field('Time zone', tz), field('Webhook signing secret', secret, { hint: 'Signs webhook posts (HMAC)' })))),
    err,
    h('div', { class: 'row' }, button('Save notification settings', save, { kind: 'primary' })));
}
