// Stream jobs: windowed queries over live readings (Azure Stream
// Analytics style). List, then one editor page per job with a dry run.
import {
  h, api, post, put, del, enc, pageHeader, card, badge, empty, table, field, opt, button, linkButton, confirmDialog, when, num, toast,
} from '../core.js';

const EXAMPLE = 'SELECT avg(level) AS level_avg\nINTO [avg-{sensor}]\nFROM [tank-*]\nGROUP BY sensor, TumblingWindow(minute, 5)';
const SYNTAX = 'SELECT avg|min|max|sum|count|stddev|first|last|delta|increase(field) [AS name]\nINTO [sensor] | [prefix-{sensor}] | alert | webhook:<target>\nFROM [glob] [WHERE value …]\nGROUP BY [sensor,] TumblingWindow(minute, 5) | HoppingWindow(minute, 60, 15) | SlidingWindow(minute, 10)\n[HAVING value > 80]';

export async function render(el, ctx) {
  if (ctx.params[0]) return editor(el, ctx, ctx.params[0] === 'new' ? null : ctx.params[0]);
  const list = await api('/api/jobs');
  const rows = list.sort((a, b) => a.id.localeCompare(b.id)).map((j) => h('tr', { class: 'link', onclick: () => { location.hash = '#/jobs/' + enc(j.id); } },
    h('td', {}, h('div', { class: 'primary-cell' }, h('a', { href: '#/jobs/' + enc(j.id), text: j.id }), h('small', { class: 'mono', text: (j.query || '').split('\n')[0] }))),
    h('td', {}, j.error ? badge('Error', 'critical') : j.enabled ? badge('Running', 'good') : badge('Stopped', 'neutral')),
    ...['in', 'out', 'late', 'dropped', 'errors'].map((k) => h('td', { class: 'n', text: num(j[k] ?? 0) })),
    h('td', { text: j.lastOutput ? `${j.lastOutput.group ?? ''} ${num(j.lastOutput.value)}`.trim() : '—' }),
    h('td', { class: 'err', text: j.error || j.lastError || '' })));
  el.append(
    pageHeader('Stream jobs', {
      sub: 'Windowed queries over live readings. Results become new sensors, alarms or webhook posts.',
      actions: linkButton('New job', '#/jobs/new', { kind: 'primary', ico: 'plus' }),
    }),
    card(null, {}, list.length
      ? table(['Job', 'State', { text: 'In', cls: 'n' }, { text: 'Out', cls: 'n' }, { text: 'Late', cls: 'n' }, { text: 'Dropped', cls: 'n' }, { text: 'Errors', cls: 'n' }, 'Last output', 'Last error'], rows)
      : empty('No stream jobs yet', 'For example: a 5-minute average of every tank level, an alarm when the hourly energy use is too high, or a webhook with each shift total.',
        linkButton('Create a job', '#/jobs/new', { kind: 'primary' }))));
}

async function editor(el, ctx, id) {
  let cur = { id: '', query: EXAMPLE, enabled: true, lateness: '' };
  if (id) {
    cur = (await api('/api/jobs')).find((j) => j.id === id);
    if (!cur) { el.append(pageHeader(id, { back: ['#/jobs', 'Stream jobs'] }), empty('Job not found', 'It may have been deleted.', linkButton('All jobs', '#/jobs'))); return; }
  }
  ctx.crumbs([[id || 'New job']]);
  const jid = h('input', { value: cur.id, placeholder: 'tank-avg', readonly: !!id, spellcheck: 'false' });
  const q = h('textarea', { 'aria-label': 'Query', spellcheck: 'false', style: 'min-height:170px' });
  q.value = cur.query;
  const late = h('input', { value: cur.lateness || '', placeholder: '5s' });
  const on = h('input', { type: 'checkbox', checked: cur.enabled });
  const from = h('select', { 'aria-label': 'Test range' }, ['-1h', '-6h', '-24h', '-7d'].map((f) => opt(f, 'Over the last ' + f.slice(1), f === '-6h')));
  const err = h('p', { class: 'err', role: 'alert' });
  const result = h('div');

  const test = async () => {
    err.textContent = '';
    result.replaceChildren(h('div', { class: 'loading' }, h('span', { class: 'spinner' }), 'Running over history…'));
    try {
      const r = await post('/api/jobs/test?from=' + enc(from.value), { query: q.value, lateness: late.value || undefined });
      result.replaceChildren(
        h('p', { class: 'hint', text: `${num(r.readings)} readings from ${r.sensors.length} sensor(s) → ${r.rows.length}${r.truncated ? '+' : ''} result(s); ${r.status.late} late, ${r.status.filtered} filtered. The last, partial window is included.` }),
        r.alerts?.length ? h('ul', {}, r.alerts.map((a) => h('li', { text: a }))) : null,
        r.rows.length ? table(['Window end', 'Output', 'Field', { text: 'Value', cls: 'n' }, { text: 'Count', cls: 'n' }],
          r.rows.slice(0, 200).map((x) => h('tr', {}, h('td', { text: when(x.windowEnd) }), h('td', { text: x.group || '' }), h('td', { text: x.field }),
            h('td', { class: 'n', text: +x.value.toFixed(4) }), h('td', { class: 'n', text: x.count || '' })))) : null);
    } catch (e) { result.replaceChildren(); err.textContent = e.message; }
  };
  const save = async () => {
    err.textContent = '';
    const name = jid.value.trim();
    if (!name) { err.textContent = 'Give the job an id.'; return; }
    try {
      await put('/api/jobs/' + enc(name), { query: q.value, enabled: on.checked, lateness: late.value || undefined });
      toast('Job saved');
      if (!id) location.hash = '#/jobs/' + enc(name); else ctx.reload();
    } catch (e) { err.textContent = e.message; }
  };

  el.append(
    pageHeader(id || 'New stream job', {
      back: ['#/jobs', 'Stream jobs'],
      sub: id ? h('span', { class: 'page-sub' }, cur.error ? badge('Error', 'critical') : cur.enabled ? badge('Running', 'good') : badge('Stopped', 'neutral'),
        h('span', { text: `${num(cur.in ?? 0)} in · ${num(cur.out ?? 0)} out · ${num(cur.late ?? 0)} late` })) : 'Test it on stored history before saving.',
      actions: id && button('Delete', async () => {
        if (!await confirmDialog(`Delete job ${id}?`, 'It stops at once and its open alarms clear. Sensors it wrote keep their history.', { ok: 'Delete', danger: true })) return;
        await del('/api/jobs/' + enc(id)); toast('Deleted'); location.hash = '#/jobs';
      }, { kind: 'danger' }),
    }),
    h('div', { class: 'grid-main' },
      h('div', {}, card('Query', {},
        h('div', { class: 'stack' },
          h('div', { class: 'form-grid' }, field('Job id', jid), field('Allowed lateness', late, { hint: 'How late a reading may arrive, e.g. 5s' }),
            h('div', { class: 'field', style: 'justify-content:end' }, field('Running', on, { cls: 'check' }))),
          q, err,
          h('div', { class: 'row' }, button('Save', save, { kind: 'primary' }), button('Test on history', test), from))),
        card('Dry run', { sub: 'Runs the query over stored readings without saving anything.' }, result)),
      h('div', {}, card('Syntax', {}, h('pre', { class: 'code', text: SYNTAX })),
        cur.lastError && card('Last error', {}, h('p', { class: 'err', text: cur.lastError })))));
  result.append(h('p', { class: 'hint', text: 'Press "Test on history" to see what this query would produce.' }));
}
