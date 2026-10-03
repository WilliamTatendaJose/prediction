// Alarms: what is active (acknowledge, shelve, notes), the history, and
// what is shelved.
import {
  h, api, post, del, enc, pageHeader, card, badge, empty, tabs, modal, field, opt, button, icon, live, canOperate,
  ago, duration, when, toast, VERDICTS, verdictLabel, plural, alarmHref,
} from '../core.js';

export function alarmTone(a) {
  if (a.end) return ['Resolved', 'good'];
  if (a.shelved) return ['Shelved', 'neutral'];
  if (a.ack) return ['Acknowledged', 'warning'];
  return ['Active', 'critical'];
}

export async function acknowledge(a) {
  const v = h('select', {}, VERDICTS.map(([k, x]) => opt(k, x, a.ack?.verdict === k)));
  const note = h('textarea', { maxlength: 1000, placeholder: 'What happened, what was done (optional)', style: 'min-height:80px;font-family:inherit;font-size:13px' });
  note.value = a.ack?.note || '';
  return modal(a.ack ? 'Change verdict' : 'Acknowledge alarm', h('div', { class: 'stack' }, field('Verdict', v), field('Note', note)), {
    sub: `${a.sensor}${a.field ? '.' + a.field : ''} · ${a.message}`,
    actions: [['Cancel'], [a.ack ? 'Save' : 'Acknowledge', async () => { await post(`/api/anomalies/${enc(a.id)}/ack`, { verdict: v.value, note: note.value.trim() }); toast('Acknowledged'); }, 'primary']],
  });
}

export async function shelve(a) {
  const scope = h('select', {}, opt('field', `Only ${a.field || a.kind}`), opt('sensor', `Everything on ${a.sensor}`));
  const d = h('select', {}, opt('1h', '1 hour'), opt('8h', '8 hours (a shift)', true), opt('24h', '24 hours'), opt('168h', '7 days'));
  const reason = h('input', { maxlength: 200, placeholder: 'e.g. sensor being replaced', required: true });
  return modal('Shelve alarms', h('div', { class: 'stack' },
    h('p', { class: 'modal-text', text: 'Shelving silences this alarm and stops notifications until the time runs out. Everyone sees that it is shelved, by whom and why.' }),
    h('div', { class: 'form-grid' }, field('What', scope), field('For', d)), field('Reason (required)', reason)), {
    actions: [['Cancel'], ['Shelve', async () => {
      if (!reason.value.trim()) throw new Error('Give a reason.');
      await post('/api/shelves', { sensor: a.sensor, field: scope.value === 'field' ? a.field : '', kind: scope.value === 'field' ? a.kind : '', duration: d.value, reason: reason.value.trim() });
      toast('Shelved');
    }, 'primary']],
  });
}

export async function notes(a) {
  const list = h('div', { class: 'stack' });
  const load = async () => {
    const ns = await api(`/api/anomalies/${enc(a.id)}/notes`).catch(() => []);
    list.replaceChildren(...(ns.length ? ns.map((n) => h('div', { class: 'note' }, h('div', { class: 'hint', text: `${when(n.ts)} · ${n.by}` }), h('div', { text: n.text })))
      : [h('p', { class: 'hint', text: 'No notes yet.' })]));
  };
  await load();
  const text = h('textarea', { maxlength: 2000, placeholder: 'Add a note for the next shift', style: 'min-height:70px;font-family:inherit;font-size:13px' });
  return modal('Notes', h('div', { class: 'stack' }, list, canOperate() && field('New note', text)), {
    sub: `${a.sensor}${a.field ? '.' + a.field : ''} · ${a.message}`,
    actions: canOperate() ? [['Close'], ['Add note', async () => {
      if (!text.value.trim()) throw new Error('Write the note first.');
      await post(`/api/anomalies/${enc(a.id)}/notes`, { text: text.value.trim() });
      text.value = ''; await load(); return false; // stay open
    }, 'primary']] : [['Close']],
  });
}

// One alarm as a list row; refresh() runs after an action.
export function alarmRow(a, refresh, { names = {} } = {}) {
  const [label, tone] = alarmTone(a);
  const acts = h('div', { class: 'acts' });
  if (canOperate()) {
    if (!a.end || a.ack) acts.append(button(a.ack ? 'Change verdict' : 'Acknowledge', async () => { if (await acknowledge(a)) refresh(); }, { kind: 'small' + (a.ack ? '' : ' primary') }));
    else acts.append(button('Add verdict', async () => { if (await acknowledge(a)) refresh(); }, { kind: 'small' }));
    if (!a.end && !a.shelved) acts.append(button('Shelve', async () => { if (await shelve(a)) refresh(); }, { kind: 'small' }));
  }
  acts.append(button(a.notes ? plural(a.notes, 'note') : 'Notes', async () => { await notes(a); refresh(); }, { kind: 'small ghost' }));
  const lasted = a.end ? `lasted ${duration(a.end - a.start)}` : `for ${duration(Date.now() - a.start)}`;
  return h('li', { class: 'alarm-row' },
    badge(label, tone),
    h('div', {},
      h('div', { class: 'what' }, h('a', { href: alarmHref(a), text: names[a.sensor] || a.sensor }), a.field ? ` · ${a.field}` : '', ' ', h('span', { class: 'chip', text: a.kind })),
      h('div', { class: 'msg', text: a.message }),
      a.ack && h('div', { class: 'ackline' }, icon('check', 'ico'), ` ${a.ack.by}`, a.ack.verdict ? ` · ${verdictLabel[a.ack.verdict] || a.ack.verdict}` : '', a.ack.note ? ` · “${a.ack.note}”` : ''),
      acts),
    h('div', { class: 'side' }, h('div', { text: ago(a.start) }), h('div', { text: lasted })));
}

// Filters survive the live refresh of the page.
const filter = { q: '', kind: '' };

export async function render(el, ctx) {
  const tab = ['active', 'history', 'shelved'].includes(ctx.params[0]) ? ctx.params[0] : 'active';
  const [activeList, history, shelves, sensors] = await Promise.all([
    api('/api/anomalies?active=1&limit=1000'),
    tab === 'history' ? api('/api/anomalies?from=-7d&limit=1000') : [],
    api('/api/shelves').catch(() => []),
    api('/api/sensors').catch(() => []),
  ]);
  const names = Object.fromEntries(sensors.map((s) => [s.id, s.name || s.id]));
  const open = activeList.filter((a) => !a.shelved);
  const refresh = () => ctx.reload();

  const q = h('input', { type: 'search', placeholder: 'Filter by sensor, field or message', 'aria-label': 'Filter alarms' });
  const kind = h('select', { 'aria-label': 'Kind' }, opt('', 'All kinds'), ['range', 'spike', 'stale', 'rule', 'overdue'].map((k) => opt(k)));
  const listBox = h('div');

  function rows(list) {
    const t = q.value.trim().toLowerCase();
    return list.filter((a) => (!kind.value || a.kind === kind.value)
      && (!t || `${a.sensor} ${names[a.sensor] || ''} ${a.field || ''} ${a.message}`.toLowerCase().includes(t)));
  }
  function draw() {
    if (tab === 'shelved') {
      listBox.replaceChildren(card('Shelved', { sub: 'Shelved alarms are silent and send no notifications until their time runs out.' },
        shelves.length ? shelves.map((s) => h('div', { class: 'shelf' },
          h('div', {}, h('b', { text: `${names[s.sensor] || s.sensor}${s.field ? ' · ' + s.field : ' · all fields'}${s.kind ? ' · ' + s.kind : ''}` }),
            h('div', { class: 'hint', text: `Until ${when(s.until)} · ${s.reason} · by ${s.by}` })),
          canOperate() && button('Unshelve', async () => { await del('/api/shelves/' + enc(s.key)); toast('Unshelved'); refresh(); }, { kind: 'small' })))
          : empty('Nothing is shelved', 'Shelve a noisy alarm from the Active tab while its cause is being fixed.')));
      return;
    }
    const src = tab === 'active'
      ? open.sort((a, b) => (!!a.ack - !!b.ack) || b.start - a.start)
      : history.sort((a, b) => b.start - a.start);
    const list = rows(src);
    listBox.replaceChildren(card(null, {},
      list.length ? h('ul', { class: 'list' }, list.slice(0, 300).map((a) => alarmRow(a, refresh, { names })))
        : tab === 'active' && !src.length ? empty('All clear', 'No alarms are active. New alarms appear here as they open.')
          : empty('No matches', 'Nothing matches this filter.'),
      list.length > 300 && h('p', { class: 'hint', text: `Showing 300 of ${list.length}. Narrow the filter to see more.` })));
  }
  q.value = filter.q; kind.value = filter.kind;
  q.oninput = () => { filter.q = q.value; draw(); };
  kind.onchange = () => { filter.kind = kind.value; draw(); };

  const unacked = open.filter((a) => !a.ack).length;
  el.append(
    pageHeader('Alarms', { sub: unacked ? `${plural(unacked, 'alarm')} waiting for acknowledgement` : 'Everything active has been acknowledged' }),
    tabs([['active', 'Active', open.length], ['history', 'Last 7 days', tab === 'history' ? history.length : null], ['shelved', 'Shelved', shelves.length]], tab, (k) => { location.hash = '#/alarms/' + k; }),
    tab !== 'shelved' && h('div', { class: 'toolbar' }, h('div', { class: 'search' }, icon('search'), q), kind),
    listBox);
  draw();

  // Live: re-read on change, but never under someone's typing in a dialog.
  let pending = null;
  const later = () => { clearTimeout(pending); pending = setTimeout(() => { 
    if (!ctx.current()) return;
    if (document.querySelector('dialog[open]') || document.activeElement === q) later(); else refresh();
  }, 800); };
  ctx.onLeave(live.on('anomaly', later));
  ctx.onLeave(live.on('alarms', later));
  ctx.onLeave(() => clearTimeout(pending));
}
