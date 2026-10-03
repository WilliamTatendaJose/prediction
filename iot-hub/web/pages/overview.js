// Overview: the state of the plant at a glance — what is reporting, what
// needs attention, how much of the plan is used.
import {
  h, api, icon, pageHeader, card, badge, empty, table, meter, linkButton, live, canManage, multiTenant,
  tenantLabel, ago, num, plural, enc, alarmHref,
} from '../core.js';

// Freshness of a sensor, always shown as a label next to its dot.
export function freshness(lastSeen, now = Date.now()) {
  if (!lastSeen) return ['Never reported', 'neutral'];
  const s = (now - lastSeen) / 1000;
  if (s < 120) return ['Reporting', 'good'];
  if (s < 3600) return ['Quiet', 'warning'];
  return ['Silent', 'critical'];
}

// A device's state from its last data. With an expected interval it is
// overdue once twice the interval has passed (at least interval + 30 s, for
// jitter); without one, the sensor scale above applies.
export function deviceFreshness(lastData, expectedSec, now = Date.now()) {
  if (!lastData) return ['Never sent data', 'neutral'];
  if (expectedSec) {
    const late = now - lastData > Math.max(2 * expectedSec, expectedSec + 30) * 1000;
    return late ? ['Overdue', 'critical'] : ['Sending data', 'good'];
  }
  const [label, tone] = freshness(lastData, now);
  return [label === 'Reporting' ? 'Sending data' : label, tone];
}
export function every(sec) {
  if (!sec) return '';
  if (sec % 86400 === 0) return sec / 86400 + ' d';
  if (sec % 3600 === 0) return sec / 3600 + ' h';
  if (sec % 60 === 0) return sec / 60 + ' min';
  return sec + ' s';
}

export function fieldSummary(s, max = 3) {
  const f = s.fields || {};
  return Object.keys(s.last || {}).sort().slice(0, max).map((k) => {
    const u = f[k]?.unit;
    return h('span', { class: 'chip' }, `${f[k]?.label || k} `, h('b', { text: num(s.last[k]) + (u ? ' ' + u : '') }));
  });
}

export async function render(el, ctx) {
  let [sensors, activeList, recent, usage, devices, twins] = await Promise.all([
    api('/api/sensors'),
    api('/api/anomalies?active=1&limit=1000').catch(() => []),
    api('/api/anomalies?from=-24h&limit=1000').catch(() => []),
    multiTenant() ? api('/api/usage').catch(() => null) : null,
    canManage() ? api('/api/devices').catch(() => null) : null,
    canManage() ? api('/api/twins').catch(() => []) : [],
  ]);
  const byId = new Map(sensors.map((s) => [s.id, s]));
  const active = new Map(activeList.filter((a) => !a.shelved).map((a) => [a.id, a]));
  let msgs = usage?.messagesToday ?? null;

  const kpis = h('div', { class: 'kpis' });
  const alarmsCard = h('div');
  const healthCard = h('div');
  const usageCard = h('div');

  function renderKpis() {
    const now = Date.now();
    const reporting = sensors.filter((s) => freshness(s.lastSeen, now)[1] === 'good').length;
    const silent = sensors.filter((s) => freshness(s.lastSeen, now)[1] === 'critical').length;
    const open = [...active.values()];
    const unacked = open.filter((a) => !a.ack).length;
    const things = (devices || []).filter((d) => d.role === 'device' || d.role === 'service');
    const tw = Object.fromEntries(twins.map((t) => [t.deviceId, t]));
    const st = (d) => deviceFreshness(tw[d.id]?.lastDataTime, tw[d.id]?.expectedIntervalSec, now)[1];
    const live_ = things.filter((d) => !d.disabled && st(d) === 'good').length;
    const stale = things.filter((d) => !d.disabled && tw[d.id]?.lastDataTime && st(d) !== 'good').length;
    const never = things.filter((d) => !d.disabled && !tw[d.id]?.lastDataTime).length;
    const connected = twins.filter((t) => t.connectionState === 'Connected').length;
    kpis.replaceChildren(
      h('a', { class: 'kpi', href: '#/sensors' },
        h('span', { class: 'kpi-label' }, icon('sensor'), 'Sensors reporting'),
        h('span', { class: 'kpi-value' }, String(reporting), h('small', { text: '/ ' + sensors.length })),
        h('span', { class: 'kpi-foot' }, silent ? badge(`${silent} silent over 1 h`, 'critical') : badge('None silent', 'good'))),
      h('a', { class: 'kpi', href: '#/alarms' },
        h('span', { class: 'kpi-label' }, icon('alarm'), 'Unacknowledged alarms'),
        h('span', { class: 'kpi-value', text: String(unacked) }),
        h('span', { class: 'kpi-foot' }, unacked ? badge('Needs attention', 'critical') : badge(open.length ? `${open.length} active, acknowledged` : 'All clear', open.length ? 'warning' : 'good'),
          `${plural(recent.length, 'alarm')} in 24 h`)),
      devices && h('a', { class: 'kpi', href: '#/devices' },
        h('span', { class: 'kpi-label' }, icon('device'), 'Devices sending data'),
        h('span', { class: 'kpi-value' }, String(live_), h('small', { text: '/ ' + things.length })),
        h('span', { class: 'kpi-foot' }, stale ? badge(`${stale} overdue, quiet or silent`, 'warning') : null, never ? badge(`${never} never sent`, 'neutral') : null,
          !stale && !never && things.length ? badge('All sending', 'good') : null, `${connected} on MQTT now`)),
      msgs != null && h('div', { class: 'kpi' },
        h('span', { class: 'kpi-label' }, icon('jobs'), 'Messages today'),
        h('span', { class: 'kpi-value', text: num(msgs) }),
        usage.quota.messagesPerDay > 0
          ? h('span', { class: 'kpi-foot', text: `of ${num(usage.quota.messagesPerDay)} a day (UTC)` })
          : h('span', { class: 'kpi-foot', text: usage.messagesRejected ? `${num(usage.messagesRejected)} rejected by rate limit` : 'No daily limit' }),
        usage.quota.messagesPerDay > 0 && meter(msgs, usage.quota.messagesPerDay)),
    );
  }

  function renderAlarms() {
    const open = [...active.values()].sort((a, b) => (!!a.ack - !!b.ack) || b.start - a.start);
    const items = open.slice(0, 6).map((a) => h('li', { class: 'alarm-row' },
      a.ack ? badge('Acknowledged', 'warning') : badge('Active', 'critical'),
      h('div', {},
        h('div', { class: 'what' }, h('a', { href: alarmHref(a), text: byId.get(a.sensor)?.name || a.sensor }), a.field ? ` · ${a.field}` : '', h('span', { class: 'chip', text: a.kind })),
        h('div', { class: 'msg', text: a.message })),
      h('div', { class: 'side', text: 'since ' + ago(a.start) })));
    alarmsCard.replaceChildren(card('Active alarms', {
      sub: open.length > 6 ? `Showing 6 of ${open.length}` : null,
      actions: linkButton('Open alarms', '#/alarms', { kind: 'small' }),
    }, items.length ? h('ul', { class: 'list' }, items) : empty('All clear', 'No alarms are active right now.')));
  }

  function renderHealth() {
    const now = Date.now();
    const rank = { critical: 0, warning: 1, neutral: 2, good: 3 };
    const list = [...sensors].sort((a, b) => rank[freshness(a.lastSeen, now)[1]] - rank[freshness(b.lastSeen, now)[1]] || a.id.localeCompare(b.id));
    const rows = list.slice(0, 12).map((s) => {
      const [label, tone] = freshness(s.lastSeen, now);
      const alarms = [...active.values()].filter((a) => a.sensor === s.id).length;
      return h('tr', { class: 'link', onclick: () => { location.hash = '#/sensors/' + enc(s.id); } },
        h('td', {}, h('div', { class: 'primary-cell' }, h('a', { href: '#/sensors/' + enc(s.id), text: s.name || s.id }), s.name && s.name !== s.id && h('small', { text: s.id }))),
        h('td', {}, fieldSummary(s, 2)),
        h('td', {}, badge(label, tone), alarms ? ' ' : null, alarms ? badge(plural(alarms, 'alarm'), 'critical') : null),
        h('td', { class: 'n', text: ago(s.lastSeen) }));
    });
    healthCard.replaceChildren(card('Sensor health', {
      sub: sensors.length > 12 ? `The 12 that most need a look, of ${sensors.length}` : null,
      actions: linkButton('All sensors', '#/sensors', { kind: 'small' }),
    }, sensors.length
      ? table(['Sensor', 'Latest', 'Status', { text: 'Last seen', cls: 'n' }], rows)
      : empty('No sensors yet', 'Sensors appear by themselves when devices start sending data. Add a device and send its first reading.',
        canManage() ? linkButton('Add a device', '#/devices', { kind: 'primary' }) : null)));
  }

  function renderUsage() {
    if (!usage) { usageCard.replaceChildren(); return; }
    const q = usage.quota;
    const line = (label, used, max, unit = '') => h('div', { class: 'stack', style: 'gap:4px' },
      h('div', { class: 'row' }, h('span', { text: label }), h('span', { class: 'spacer' }), h('b', { text: `${num(used)}${max > 0 ? ' / ' + num(max) : ''}${unit}` })),
      max > 0 ? meter(used, max) : h('span', { class: 'hint', text: 'Unlimited' }));
    usageCard.replaceChildren(card('Plan and usage', { sub: tenantLabel() },
      h('div', { class: 'stack' },
        line('Sensors', usage.sensors, q.maxSensors),
        line('Devices and users', usage.devices, q.maxDevices),
        line('Messages today', msgs, q.messagesPerDay),
        h('dl', { class: 'dl' },
          h('dt', { text: 'Rate limit' }), h('dd', { text: q.messagesPerSecond > 0 ? `${num(q.messagesPerSecond)} messages/s` : 'Unlimited' }),
          h('dt', { text: 'Raw data kept' }), h('dd', { text: q.rawRetentionDays > 0 ? `${q.rawRetentionDays} days` : 'Forever' }),
          h('dt', { text: 'Rollups kept' }), h('dd', { text: q.rollupRetentionDays > 0 ? `${q.rollupRetentionDays} days` : 'Forever' }),
          h('dt', { text: 'Stream jobs' }), h('dd', { text: q.maxJobs > 0 ? `up to ${q.maxJobs}` : 'Unlimited' })))));
  }

  const t = tenantLabel();
  el.append(
    pageHeader('Overview', { sub: t ? `${t} · live` : 'Live state of every sensor and alarm',
      actions: [linkButton('Open dashboard', '#/dashboard', { ico: 'dashboard' })] }),
    kpis,
    h('div', { class: 'grid-main' }, h('div', {}, healthCard), h('div', {}, alarmsCard, usageCard)));
  renderKpis(); renderAlarms(); renderHealth(); renderUsage();

  // Live: readings update freshness and values; alarms update the lists.
  let dirty = false;
  ctx.onLeave(live.on('reading', (r) => {
    let s = byId.get(r.s);
    if (!s) { s = { id: r.s, fields: {}, last: {} }; byId.set(r.s, s); sensors.push(s); }
    Object.assign(s.last ||= {}, r.v);
    s.lastSeen = r.t;
    dirty = true;
  }));
  ctx.onLeave(live.on('anomaly', (a) => {
    if (a.end || a.shelved) active.delete(a.id); else active.set(a.id, a);
    if (!recent.some((x) => x.id === a.id)) recent.push(a);
    renderKpis(); renderAlarms(); dirty = true;
  }));
  ctx.onLeave(live.on('alarms', async () => {
    const list = await api('/api/anomalies?active=1&limit=1000').catch(() => null);
    if (!list) return;
    active.clear();
    for (const a of list) if (!a.shelved) active.set(a.id, a);
    renderKpis(); renderAlarms(); dirty = true;
  }));
  // Message counts come from the server (a reading on the stream is not
  // always one message), re-read every 30 s.
  const usageTimer = usage ? setInterval(async () => {
    const u = await api('/api/usage').catch(() => null);
    if (u && ctx.current()) { Object.assign(usage, u); msgs = u.messagesToday; dirty = true; }
  }, 30000) : null;
  ctx.onLeave(() => clearInterval(usageTimer));
  // Devices' last-data times live on the server (a reading on the stream
  // doesn't say which device sent it): re-read every 30 s.
  const twinTimer = devices ? setInterval(async () => {
    const t = await api('/api/twins').catch(() => null);
    if (t && ctx.current()) { twins = t; dirty = true; }
  }, 30000) : null;
  ctx.onLeave(() => clearInterval(twinTimer));
  const timer = setInterval(() => {
    if (!dirty || !ctx.current()) return;
    dirty = false;
    renderKpis(); renderHealth(); renderUsage();
  }, 2000);
  ctx.onLeave(() => clearInterval(timer));
}
