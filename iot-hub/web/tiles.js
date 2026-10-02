// Tile registry. Adding a tile type = one registerTile() call; the server
// stores the layout as opaque JSON so no backend change is needed.
//
// registerTile(type, {
//   label,                 // shown in the "Add tile" dialog
//   multi,                 // true: tile takes several fields (cfg.fields), else cfg.field
//   options: [{ key, label, type: 'number'|'text'|'select', choices?: [[value, label]] }],
//   create(el, cfg, ctx) -> { update(t, value|values), render(), destroy?() }
// })
//
//   sensorOptional,        // true: may watch all sensors (cfg.sensor empty)
//   noField,               // true: takes no field
// ctx = { sensor, unit(field), history(field, limit) -> Promise<{t:[],v:[]}>,
//         api(path) -> Promise<json>, invalidate() }
// update() only records data; render() runs at most once per animation frame.
// Optional anomaly(event) receives anomaly episodes for the tile's sensor
// (or all sensors when cfg.sensor is empty).

export const TILE_TYPES = {};
export function registerTile(type, def) { TILE_TYPES[type] = def; }

const SERIES = ['--s1', '--s2', '--s3', '--s4'];
const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

export function fmt(v) {
  if (typeof v !== 'number' || !isFinite(v)) return v == null ? '—' : String(v);
  const a = Math.abs(v);
  if (Number.isInteger(v) && a < 1e4) return String(v);
  if (a >= 1e6) return (v / 1e6).toFixed(1) + 'M';
  if (a >= 1e4) return (v / 1e3).toFixed(1) + 'K';
  if (a >= 100) return v.toFixed(0);
  if (a >= 10) return v.toFixed(1);
  return v.toFixed(2);
}

const timeFmt = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' });

// Thresholds: warn/crit numbers. If crit < warn, low values are the bad side.
function statusOf(v, o) {
  const w = num(o.warn), c = num(o.crit);
  if (typeof v !== 'number' || (w == null && c == null)) return null;
  const lowBad = w != null && c != null && c < w;
  const past = (t) => t != null && (lowBad ? v <= t : v >= t);
  if (past(c)) return ['critical', 'Critical'];
  if (past(w)) return ['warning', 'Warning'];
  return ['good', 'Normal'];
}
const num = (x) => (x === '' || x == null || isNaN(+x) ? null : +x);

function statusEl() {
  const s = document.createElement('div');
  s.className = 'status';
  s.append(document.createElement('i'), document.createElement('span'));
  return s;
}
function setStatus(s, st) {
  s.hidden = !st;
  if (!st) return;
  s.firstChild.style.background = `var(--${st[0]})`;
  s.lastChild.textContent = st[1];
}

// Fixed-capacity ring of (t, v) in typed arrays: no growth, no GC churn.
export class Series {
  constructor(cap) { this.t = new Float64Array(cap); this.v = new Float32Array(cap); this.cap = cap; this.n = 0; this.h = 0; }
  push(t, v) {
    if (this.n && t < this.t[(this.h - 1 + this.cap) % this.cap]) return; // ignore out-of-order
    this.t[this.h] = t; this.v[this.h] = v;
    this.h = (this.h + 1) % this.cap;
    if (this.n < this.cap) this.n++;
  }
  idx(i) { return (this.h - this.n + i + this.cap) % this.cap; }
  last() { return this.n ? this.v[this.idx(this.n - 1)] : undefined; }
  // History may arrive after live points; keep both, oldest first.
  load(h) {
    const live = [];
    for (let i = 0; i < this.n; i++) { const j = this.idx(i); live.push([this.t[j], this.v[j]]); }
    const cut = live.length ? live[0][0] : Infinity;
    this.n = 0; this.h = 0;
    for (let i = 0; i < h.t.length; i++) if (h.t[i] < cut) this.push(h.t[i], h.v[i]);
    for (const [t, v] of live) this.push(t, v);
  }
}

// Sizes a canvas to its box at device pixel ratio; returns a 2D context in CSS px.
function fitCanvas(cv) {
  const r = cv.getBoundingClientRect(), d = window.devicePixelRatio || 1;
  const w = Math.max(1, Math.round(r.width * d)), h = Math.max(1, Math.round(r.height * d));
  if (cv.width !== w || cv.height !== h) { cv.width = w; cv.height = h; }
  const g = cv.getContext('2d');
  g.setTransform(d, 0, 0, d, 0, 0);
  g.clearRect(0, 0, r.width, r.height);
  return [g, r.width, r.height];
}

function extent(series, o) {
  let t0 = Infinity, t1 = -Infinity, lo = Infinity, hi = -Infinity;
  for (const s of series) for (let i = 0; i < s.n; i++) {
    const j = s.idx(i);
    if (s.t[j] < t0) t0 = s.t[j];
    if (s.t[j] > t1) t1 = s.t[j];
    if (s.v[j] < lo) lo = s.v[j];
    if (s.v[j] > hi) hi = s.v[j];
  }
  if (num(o.min) != null) lo = +o.min;
  if (num(o.max) != null) hi = +o.max;
  if (hi === lo) { hi += 1; lo -= 1; }
  else if (num(o.min) == null || num(o.max) == null) { const p = (hi - lo) * 0.08; lo -= p; hi += p; }
  return { t0, t1: t1 === t0 ? t0 + 1 : t1, lo, hi };
}

function drawLines(g, series, colors, x0, y0, w, h, e, lineW) {
  g.lineWidth = lineW; g.lineJoin = 'round'; g.lineCap = 'round';
  series.forEach((s, k) => {
    if (s.n < 1) return;
    g.strokeStyle = colors[k];
    g.beginPath();
    for (let i = 0; i < s.n; i++) {
      const j = s.idx(i);
      const x = x0 + ((s.t[j] - e.t0) / (e.t1 - e.t0)) * w;
      const y = y0 + h - ((s.v[j] - e.lo) / (e.hi - e.lo)) * h;
      i ? g.lineTo(x, y) : g.moveTo(x, y);
    }
    g.stroke();
  });
}

function header(el, cfg, ctx, fields) {
  const h = document.createElement('h2');
  h.textContent = cfg.title || `${ctx.sensor.name || ctx.sensor.id} · ${fields.join(', ')}`;
  const body = document.createElement('div');
  body.className = 'body';
  el.append(h, body);
  return body;
}

// ---- stat: current value, unit, status, sparkline -------------------------
registerTile('stat', {
  label: 'Stat (value + sparkline)',
  options: [{ key: 'warn', label: 'Warn at', type: 'number' }, { key: 'crit', label: 'Critical at', type: 'number' }],
  create(el, cfg, ctx) {
    const o = cfg.options || {};
    const body = header(el, cfg, ctx, [cfg.field]);
    const val = document.createElement('div'); val.className = 'value';
    const num_ = document.createTextNode('—');
    const unit = document.createElement('span'); unit.className = 'unit'; unit.textContent = ctx.unit(cfg.field);
    val.append(num_, unit);
    const st = statusEl();
    const cv = document.createElement('canvas'); cv.className = 'spark';
    body.append(val, st, cv);
    const s = new Series(60);
    let lastV;
    ctx.history(cfg.field, 60).then((h) => { s.load(h); if (lastV === undefined) lastV = s.last(); ctx.invalidate(); });
    return {
      update(t, v) { lastV = v; if (typeof v === 'number') s.push(t, v); },
      render() {
        num_.nodeValue = fmt(lastV);
        setStatus(st, statusOf(lastV, o));
        const [g, w, h] = fitCanvas(cv);
        if (s.n > 1) drawLines(g, [s], [css('--s1')], 1, 2, w - 2, h - 4, extent([s], {}), 1.5);
      },
    };
  },
});

// ---- meter: value against a min..max range --------------------------------
registerTile('meter', {
  label: 'Meter (range bar)',
  options: [
    { key: 'min', label: 'Min', type: 'number' }, { key: 'max', label: 'Max', type: 'number' },
    { key: 'warn', label: 'Warn at', type: 'number' }, { key: 'crit', label: 'Critical at', type: 'number' },
  ],
  create(el, cfg, ctx) {
    const o = cfg.options || {};
    const f = ctx.sensor.fields?.[cfg.field] || {};
    const min = num(o.min) ?? f.min ?? 0, max = num(o.max) ?? f.max ?? 100;
    const body = header(el, cfg, ctx, [cfg.field]);
    const val = document.createElement('div'); val.className = 'value';
    const t = document.createTextNode('—');
    const unit = document.createElement('span'); unit.className = 'unit'; unit.textContent = ctx.unit(cfg.field);
    val.append(t, unit);
    const st = statusEl();
    const meter = document.createElement('div'); meter.className = 'meter';
    meter.setAttribute('role', 'meter'); meter.setAttribute('aria-valuemin', min); meter.setAttribute('aria-valuemax', max);
    const fill = document.createElement('div'); meter.append(fill);
    const scale = document.createElement('div'); scale.className = 'meter-scale';
    const a = document.createElement('span'), b = document.createElement('span');
    a.textContent = fmt(min); b.textContent = fmt(max); scale.append(a, b);
    body.append(val, meter, scale, st);
    let v = ctx.sensor.last?.[cfg.field];
    return {
      update(_, x) { v = x; },
      render() {
        t.nodeValue = fmt(v);
        const p = typeof v === 'number' ? Math.min(1, Math.max(0, (v - min) / (max - min))) : 0;
        fill.style.width = (p * 100).toFixed(1) + '%';
        const s = statusOf(v, o);
        fill.style.background = s && s[0] !== 'good' ? `var(--${s[0]})` : 'var(--s1)';
        meter.setAttribute('aria-valuenow', typeof v === 'number' ? v : '');
        setStatus(st, s);
      },
    };
  },
});

// ---- line: up to 4 fields, live or historical, anomaly markers -----------
// All fields share one y-axis, so only group fields with the same unit/scale;
// put different measures in separate tiles. Historical ranges come from
// /series (bucket averages with a min-max band), refreshed periodically.
const RANGES = { '': 0, '1h': 3600e3, '6h': 6 * 3600e3, '24h': 86400e3, '7d': 7 * 86400e3, '30d': 30 * 86400e3 };
const dateTimeFmt = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });

registerTile('line', {
  label: 'Line chart',
  multi: true,
  defaultSize: { w: 2, h: 2 },
  options: [
    { key: 'range', label: 'Time range', type: 'select', choices: [['', 'Live'], ['1h', 'Last hour'], ['6h', 'Last 6 hours'], ['24h', 'Last 24 hours'], ['7d', 'Last 7 days'], ['30d', 'Last 30 days']] },
    { key: 'forecast', label: 'Forecast (first field)', type: 'select', choices: [['', 'None'], ['1h', 'Next hour'], ['6h', 'Next 6 hours'], ['24h', 'Next 24 hours']] },
    { key: 'points', label: 'Points kept (live)', type: 'number' },
    { key: 'min', label: 'Y min', type: 'number' }, { key: 'max', label: 'Y max', type: 'number' },
  ],
  create(el, cfg, ctx) {
    const o = cfg.options || {};
    const span = RANGES[o.range] || 0; // 0 = live
    const fields = (cfg.fields || [cfg.field]).slice(0, SERIES.length);
    const cap = Math.min(5000, Math.max(10, num(o.points) ?? 300));
    const body = header(el, cfg, ctx, fields);
    if (span) el.querySelector('h2').textContent += ` · ${o.range}`;
    const wrap = document.createElement('div'); wrap.className = 'chart';
    const cv = document.createElement('canvas');
    const tip = document.createElement('div'); tip.className = 'tip';
    wrap.append(cv, tip);
    body.append(wrap);
    if (fields.length > 1) { // legend for >= 2 series; identity never by color alone
      const lg = document.createElement('div'); lg.className = 'legend';
      fields.forEach((f, k) => { const s = document.createElement('span'); s.style.setProperty('--c', `var(${SERIES[k]})`); s.textContent = f; lg.append(s); });
      body.append(lg);
    }
    let series = fields.map(() => new Series(cap));
    let bands = fields.map(() => null); // historical: { min: Float32Array, max: Float32Array }
    let marks = [];                      // anomaly episodes { start, end, field, kind, message }
    let timer = null, alive = true;
    let fc = null, fcTimer = null; // forecast for fields[0]
    async function loadForecast() {
      fc = await ctx.api(`/api/sensors/${encodeURIComponent(cfg.sensor)}/forecast?field=${encodeURIComponent(fields[0])}&horizon=${o.forecast}`).catch(() => null);
      if (!alive) return;
      ctx.invalidate();
      fcTimer = setTimeout(loadForecast, Math.max(60e3, fc?.bucket || 0));
    }
    if (o.forecast) loadForecast();

    const loadMarks = (from) => ctx.api(`/api/anomalies?sensor=${encodeURIComponent(cfg.sensor)}&from=${from}&limit=200`)
      .then((evs) => { marks = evs.filter((e) => !e.field || fields.includes(e.field)); ctx.invalidate(); })
      .catch(() => {});

    async function loadHistory() {
      const from = Date.now() - span;
      const res = await Promise.all(fields.map((f) =>
        ctx.api(`/api/sensors/${encodeURIComponent(cfg.sensor)}/series?field=${encodeURIComponent(f)}&from=${from}`).catch(() => null)));
      if (!alive) return;
      series = res.map((r) => { const s = new Series(Math.max(1, r?.t.length || 0)); if (r) s.load({ t: r.t, v: r.avg }); return s; });
      bands = res.map((r) => r && { min: Float32Array.from(r.min), max: Float32Array.from(r.max) });
      const bucket = res.find(Boolean)?.bucket || 60e3;
      await loadMarks(from);
      timer = setTimeout(loadHistory, Math.max(15e3, bucket)); // no point refreshing faster than a bucket fills
    }
    if (span) loadHistory();
    else {
      fields.forEach((f, k) => ctx.history(f, cap).then((h) => { series[k].load(h); ctx.invalidate(); }));
      loadMarks(Date.now() - 3600e3);
    }

    let hoverX = null;
    const pad = { l: 36, r: 6, t: 6, b: 16 };
    wrap.addEventListener('pointermove', (ev) => { hoverX = ev.offsetX; ctx.invalidate(); });
    wrap.addEventListener('pointerleave', () => { hoverX = null; tip.style.display = 'none'; ctx.invalidate(); });
    const ro = new ResizeObserver(() => ctx.invalidate()); ro.observe(wrap);

    return {
      update(t, values) {
        if (span) return; // historical view refreshes on its own schedule
        fields.forEach((f, k) => { const v = values[f]; if (typeof v === 'number') series[k].push(t, v); else if (typeof v === 'boolean') series[k].push(t, +v); });
      },
      anomaly(ev) {
        if (ev.field && !fields.includes(ev.field)) return;
        const i = marks.findIndex((m) => m.id === ev.id);
        if (i >= 0) marks[i] = ev; else marks.push(ev);
      },
      render() {
        const [g, W, H] = fitCanvas(cv);
        const w = W - pad.l - pad.r, h = H - pad.t - pad.b;
        if (w < 10 || h < 10 || series.every((s) => s.n === 0)) return;
        const e = extent(series, o);
        if (span) {
          e.t1 = Date.now(); e.t0 = e.t1 - span;
          bands.forEach((b) => { if (!b) return; for (let i = 0; i < b.min.length; i++) { if (num(o.min) == null) e.lo = Math.min(e.lo, b.min[i]); if (num(o.max) == null) e.hi = Math.max(e.hi, b.max[i]); } });
        }
        if (fc?.t?.length) { // make room for the forecast; the band may clip, the central line may not
          e.t1 = Math.max(e.t1, fc.t[fc.t.length - 1]);
          for (const v of fc.yhat) { if (num(o.min) == null) e.lo = Math.min(e.lo, v); if (num(o.max) == null) e.hi = Math.max(e.hi, v); }
        }
        const X = (t) => pad.l + ((t - e.t0) / (e.t1 - e.t0)) * w;
        const Y = (v) => pad.t + h - ((v - e.lo) / (e.hi - e.lo)) * h;
        const tf = e.t1 - e.t0 >= 12 * 3600e3 ? dateTimeFmt : timeFmt; // dates once the ends can share a clock time
        g.font = '10px system-ui, sans-serif'; g.fillStyle = css('--muted'); g.textBaseline = 'middle'; g.textAlign = 'right';
        g.strokeStyle = css('--grid'); g.lineWidth = 1;
        for (let i = 0; i <= 3; i++) { // recessive hairline grid + y ticks
          const y = Math.round(pad.t + (h * i) / 3) + 0.5;
          g.beginPath(); g.moveTo(pad.l, y); g.lineTo(pad.l + w, y); g.stroke();
          g.fillText(fmt(e.hi - ((e.hi - e.lo) * i) / 3), pad.l - 4, y);
        }
        g.textBaseline = 'alphabetic';
        g.textAlign = 'left'; g.fillText(tf.format(e.t0), pad.l, H - 2);
        g.textAlign = 'right'; g.fillText(tf.format(e.t1), pad.l + w, H - 2);
        const colors = fields.map((_, k) => css(SERIES[k]));

        // min-max band behind each historical line
        bands.forEach((b, k) => {
          const s = series[k];
          if (!b || s.n < 2) return;
          g.beginPath();
          for (let i = 0; i < s.n; i++) { const x = X(s.t[s.idx(i)]); i ? g.lineTo(x, Y(b.max[i])) : g.moveTo(x, Y(b.max[i])); }
          for (let i = s.n - 1; i >= 0; i--) g.lineTo(X(s.t[s.idx(i)]), Y(b.min[i]));
          g.closePath(); g.globalAlpha = 0.15; g.fillStyle = colors[k]; g.fill(); g.globalAlpha = 1;
        });
        drawLines(g, series, colors, pad.l, pad.t, w, h, e, 2);

        if (fc?.t?.length) {
          g.save();
          g.beginPath(); g.rect(pad.l, pad.t, w, h); g.clip();
          g.beginPath(); // 80 % band
          fc.t.forEach((t, i) => (i ? g.lineTo(X(t), Y(fc.hi[i])) : g.moveTo(X(t), Y(fc.hi[i]))));
          for (let i = fc.t.length - 1; i >= 0; i--) g.lineTo(X(fc.t[i]), Y(fc.lo[i]));
          g.closePath(); g.globalAlpha = 0.12; g.fillStyle = colors[0]; g.fill(); g.globalAlpha = 1;
          g.setLineDash([5, 4]); g.strokeStyle = colors[0]; g.lineWidth = 2; // central forecast
          g.beginPath(); fc.t.forEach((t, i) => (i ? g.lineTo(X(t), Y(fc.yhat[i])) : g.moveTo(X(t), Y(fc.yhat[i])))); g.stroke();
          const nowX = Math.round(X(Date.now())) + 0.5; // "now" divider
          g.setLineDash([2, 3]); g.strokeStyle = css('--axis'); g.lineWidth = 1;
          g.beginPath(); g.moveTo(nowX, pad.t); g.lineTo(nowX, pad.t + h); g.stroke();
          for (const c of fc.crossings || []) { // limit line + where the forecast meets it
            if (c.threshold < e.lo || c.threshold > e.hi) continue;
            g.strokeStyle = css('--critical'); g.globalAlpha = 0.6;
            g.beginPath(); g.moveTo(pad.l, Y(c.threshold)); g.lineTo(pad.l + w, Y(c.threshold)); g.stroke();
            g.globalAlpha = 1;
            if (c.eta) { g.setLineDash([]); g.fillStyle = css('--critical'); g.beginPath(); g.arc(X(c.eta), Y(c.threshold), 4, 0, 7); g.fill(); }
          }
          g.restore();
          g.font = '10px system-ui, sans-serif'; g.fillStyle = css('--muted'); g.textAlign = 'left'; g.textBaseline = 'top';
          g.fillText('forecast →', nowX + 4, pad.t + 2);
        }

        // anomaly markers: dashed rule + a small flag at the top
        const crit = css('--critical');
        g.strokeStyle = crit; g.fillStyle = crit; g.lineWidth = 1; g.setLineDash([3, 3]);
        const seen = [];
        for (const m of marks) {
          if (m.start < e.t0 || m.start > e.t1) continue;
          const x = Math.round(X(m.start)) + 0.5;
          seen.push([x, m]);
          g.beginPath(); g.moveTo(x, pad.t + 6); g.lineTo(x, pad.t + h); g.stroke();
          g.beginPath(); g.moveTo(x - 4, pad.t); g.lineTo(x + 4, pad.t); g.lineTo(x, pad.t + 6); g.closePath(); g.fill();
        }
        g.setLineDash([]);

        if (hoverX == null || hoverX < pad.l || hoverX > pad.l + w) { tip.style.display = 'none'; return; }
        const tx = e.t0 + ((hoverX - pad.l) / w) * (e.t1 - e.t0);
        if (fc?.t?.length && tx > Date.now()) { // hovering the forecast
          let i = 0;
          for (let k = 1; k < fc.t.length; k++) if (Math.abs(fc.t[k] - tx) < Math.abs(fc.t[i] - tx)) i = k;
          const x = X(fc.t[i]);
          g.strokeStyle = css('--axis'); g.beginPath(); g.moveTo(Math.round(x) + 0.5, pad.t); g.lineTo(Math.round(x) + 0.5, pad.t + h); g.stroke();
          tip.replaceChildren();
          const tt = document.createElement('div'); tt.className = 't'; tt.textContent = 'forecast · ' + tf.format(fc.t[i]); tip.append(tt);
          const row = document.createElement('div'); row.className = 'row'; row.style.setProperty('--c', `var(${SERIES[0]})`);
          const b = document.createElement('b'); b.textContent = fmt(fc.yhat[i]) + ' ' + ctx.unit(fields[0]);
          const n = document.createElement('span'); n.textContent = `likely ${fmt(fc.lo[i])} to ${fmt(fc.hi[i])}`;
          row.append(b, n); tip.append(row);
          tip.style.display = 'block';
          const left = x + 12 + tip.offsetWidth > W ? x - 12 - tip.offsetWidth : x + 12;
          tip.style.left = Math.max(0, left) + 'px'; tip.style.top = pad.t + 'px';
          return;
        }
        // Crosshair snaps to the nearest sample time of the first non-empty series.
        const ref = series.find((s) => s.n);
        let best = 0, bd = Infinity;
        for (let i = 0; i < ref.n; i++) { const d = Math.abs(ref.t[ref.idx(i)] - tx); if (d < bd) { bd = d; best = i; } }
        const ts = ref.t[ref.idx(best)];
        const x = X(ts);
        g.strokeStyle = css('--axis'); g.beginPath(); g.moveTo(Math.round(x) + 0.5, pad.t); g.lineTo(Math.round(x) + 0.5, pad.t + h); g.stroke();
        tip.replaceChildren();
        const tt = document.createElement('div'); tt.className = 't'; tt.textContent = tf.format(ts); tip.append(tt);
        series.forEach((s, k) => {
          let v, bi = -1; // value at the nearest time in this series
          let d = Infinity;
          for (let i = 0; i < s.n; i++) { const j = s.idx(i), dd = Math.abs(s.t[j] - ts); if (dd < d) { d = dd; v = s.v[j]; bi = i; } }
          if (v === undefined) return;
          g.fillStyle = colors[k]; g.strokeStyle = css('--surface'); g.lineWidth = 2;
          g.beginPath(); g.arc(x, Y(v), 4, 0, 7); g.fill(); g.stroke();
          const row = document.createElement('div'); row.className = 'row'; row.style.setProperty('--c', `var(${SERIES[k]})`);
          const b = document.createElement('b'); b.textContent = fmt(v) + ' ' + ctx.unit(fields[k]);
          const n = document.createElement('span');
          n.textContent = bands[k] ? `${fields[k]} avg · ${fmt(bands[k].min[bi])}–${fmt(bands[k].max[bi])}` : fields[k];
          row.append(b, n); tip.append(row);
        });
        for (const [mx, m] of seen) { // anomalies near the pointer
          if (Math.abs(mx - hoverX) > 8) continue;
          const row = document.createElement('div'); row.className = 'row'; row.style.setProperty('--c', 'var(--critical)');
          const b = document.createElement('b'); b.textContent = m.kind;
          const n = document.createElement('span'); n.textContent = m.message;
          row.append(b, n); tip.append(row);
        }
        tip.style.display = 'block';
        const left = x + 12 + tip.offsetWidth > W ? x - 12 - tip.offsetWidth : x + 12;
        tip.style.left = Math.max(0, left) + 'px'; tip.style.top = pad.t + 'px';
      },
      destroy() { alive = false; clearTimeout(timer); clearTimeout(fcTimer); ro.disconnect(); },
    };
  },
});

// ---- eta: time until a forecast reaches a limit ---------------------------
const dur = (ms) => {
  const m = Math.round(ms / 60e3);
  if (m < 60) return `${m} min`;
  const h = Math.floor(m / 60);
  return h < 48 ? `${h} h ${m % 60} min` : `${Math.round(h / 24)} days`;
};
registerTile('eta', {
  label: 'Time to limit (forecast)',
  options: [
    { key: 'horizon', label: 'Look ahead', type: 'select', choices: [['24h', '24 hours'], ['6h', '6 hours'], ['7d', '7 days']] },
    { key: 'threshold', label: 'Limit (default: field alert limit)', type: 'number' },
    { key: 'side', label: 'Alert when the value goes', type: 'select', choices: [['below', 'below the limit'], ['above', 'above the limit']] },
  ],
  create(el, cfg, ctx) {
    const o = cfg.options || {};
    const horizon = o.horizon || '24h';
    const body = header(el, cfg, ctx, [cfg.field]);
    el.querySelector('h2').textContent = cfg.title || `${ctx.sensor.name || ctx.sensor.id} · ${cfg.field} · time to limit`;
    const val = document.createElement('div'); val.className = 'value';
    const sub = document.createElement('div'); sub.className = 'sub';
    const st = statusEl();
    body.append(val, sub, st);
    let f = null, err = '', timer, alive = true;
    const load = async () => {
      const q = num(o.threshold) != null ? `&threshold=${o.threshold}&side=${o.side || 'below'}` : '';
      try {
        f = await ctx.api(`/api/sensors/${encodeURIComponent(cfg.sensor)}/forecast?field=${encodeURIComponent(cfg.field)}&horizon=${horizon}${q}`);
        err = '';
      } catch (e) { f = null; err = e.message; }
      if (!alive) return;
      ctx.invalidate();
      timer = setTimeout(load, 60e3);
    };
    load();
    return {
      update() {},
      render() {
        const c = f?.crossings?.[0];
        if (!f || !c) {
          val.textContent = '—';
          sub.textContent = err || 'Set a limit here or a low/high alert limit on the field.';
          setStatus(st, null);
          return;
        }
        const unit = ctx.unit(cfg.field);
        if (c.eta) {
          val.textContent = dur(c.eta - Date.now());
          const lo = c.etaEarly ? dur(c.etaEarly - Date.now()) : null, hi = c.etaLate ? dur(c.etaLate - Date.now()) : `>${horizon}`;
          sub.textContent = `until ${c.side} ${fmt(c.threshold)} ${unit}${lo ? ` · likely ${lo} – ${hi}` : ''}`;
        } else {
          val.textContent = `> ${horizon}`;
          sub.textContent = `no ${c.side === 'below' ? 'drop below' : 'rise above'} ${fmt(c.threshold)} ${unit} forecast` + (c.etaEarly ? ` (possible in ${dur(c.etaEarly - Date.now())})` : '');
        }
        // Say plainly when the forecast is not better than assuming no change.
        setStatus(st, f.skill > 0.2 ? ['good', `Forecast reliable (${Math.round(f.skill * 100)}% better than no-change)`]
          : ['warning', 'Low confidence: recent data is too irregular to forecast']);
      },
      destroy() { alive = false; clearTimeout(timer); },
    };
  },
});

// ---- stats: summary over a time range -------------------------------------
registerTile('stats', {
  label: 'Statistics (mean, min, max, std)',
  options: [{ key: 'range', label: 'Time range', type: 'select', choices: [['1h', 'Last hour'], ['24h', 'Last 24 hours'], ['7d', 'Last 7 days'], ['30d', 'Last 30 days']] }],
  create(el, cfg, ctx) {
    const range = (cfg.options || {}).range || '24h';
    const body = header(el, cfg, ctx, [cfg.field]);
    el.querySelector('h2').textContent += ` · ${range}`;
    const val = document.createElement('div'); val.className = 'value';
    const t = document.createTextNode('—');
    const unit = document.createElement('span'); unit.className = 'unit'; unit.textContent = 'mean ' + ctx.unit(cfg.field);
    val.append(t, unit);
    const grid = document.createElement('dl'); grid.className = 'kv';
    const cells = {};
    for (const k of ['min', 'max', 'std', 'n']) {
      const dt = document.createElement('dt'); dt.textContent = k === 'n' ? 'samples' : k;
      const dd = document.createElement('dd'); dd.textContent = '—';
      grid.append(dt, dd); cells[k] = dd;
    }
    body.append(val, grid);
    let st = null, timer, alive = true;
    const load = async () => {
      st = await ctx.api(`/api/sensors/${encodeURIComponent(cfg.sensor)}/stats?field=${encodeURIComponent(cfg.field)}&from=-${range}`).catch(() => null);
      if (!alive) return;
      ctx.invalidate();
      timer = setTimeout(load, 30e3);
    };
    load();
    return {
      update() {},
      render() {
        const ok = st && st.n > 0;
        t.nodeValue = ok ? fmt(st.mean) : '—';
        cells.min.textContent = ok ? fmt(st.min) : '—';
        cells.max.textContent = ok ? fmt(st.max) : '—';
        cells.std.textContent = ok ? fmt(st.std) : '—';
        cells.n.textContent = st ? st.n.toLocaleString() : '—';
      },
      destroy() { alive = false; clearTimeout(timer); },
    };
  },
});

// ---- anomalies: recent episodes, open ones first --------------------------
registerTile('anomalies', {
  label: 'Anomaly log',
  sensorOptional: true,
  noField: true,
  defaultSize: { w: 2, h: 2 },
  options: [],
  create(el, cfg, ctx) {
    const h = document.createElement('h2');
    h.textContent = cfg.title || (cfg.sensor ? `Anomalies · ${ctx.sensor?.name || cfg.sensor}` : 'Anomalies');
    const list = document.createElement('ul'); list.className = 'alog';
    el.append(h, list);
    let evs = [];
    const q = cfg.sensor ? `&sensor=${encodeURIComponent(cfg.sensor)}` : '';
    ctx.api(`/api/anomalies?limit=50${q}`).then((r) => { evs = r; ctx.invalidate(); }).catch(() => {});
    const ago = (ms) => { const s = (Date.now() - ms) / 1000; return s < 90 ? `${Math.round(s)}s ago` : s < 5400 ? `${Math.round(s / 60)}m ago` : s < 129600 ? `${Math.round(s / 3600)}h ago` : `${Math.round(s / 86400)}d ago`; };
    return {
      update() {},
      anomaly(ev) {
        const i = evs.findIndex((e) => e.id === ev.id);
        if (i >= 0) evs[i] = ev; else evs.unshift(ev);
        if (evs.length > 50) evs.length = 50;
      },
      render() {
        list.replaceChildren();
        const sorted = [...evs].sort((a, b) => (!a.end - !b.end) * -1 || b.start - a.start);
        if (!sorted.length) { const li = document.createElement('li'); li.className = 'sub'; li.textContent = 'No anomalies recorded.'; list.append(li); }
        for (const e of sorted.slice(0, 30)) {
          const li = document.createElement('li');
          const st = statusEl(); setStatus(st, e.end ? ['muted', 'Resolved'] : ['critical', 'Active']);
          const kind = document.createElement('b'); kind.textContent = `${e.kind} · ${e.sensor}${e.field ? '.' + e.field : ''}`;
          const msg = document.createElement('div'); msg.className = 'msg'; msg.textContent = e.message;
          const when = document.createElement('span'); when.className = 'sub';
          const secs = (e.end - e.start) / 1000;
          when.textContent = e.end ? `${ago(e.start)} · lasted ${secs < 1 ? '<1s' : secs < 90 ? Math.round(secs) + 's' : Math.round(secs / 60) + 'm'}` : `since ${ago(e.start)}`;
          li.append(st, kind, when, msg);
          list.append(li);
        }
      },
    };
  },
});

// ---- state: text / boolean with an "ok" value -----------------------------
registerTile('state', {
  label: 'State (text / on-off)',
  options: [{ key: 'ok', label: 'Normal value (e.g. Closed, true)', type: 'text' }],
  create(el, cfg, ctx) {
    const o = cfg.options || {};
    const body = header(el, cfg, ctx, [cfg.field]);
    const val = document.createElement('div'); val.className = 'value';
    const st = statusEl();
    const since = document.createElement('div'); since.className = 'sub';
    body.append(val, st, since);
    let v = ctx.sensor.last?.[cfg.field], changed = null;
    return {
      update(t, x) { if (x !== v) changed = t; v = x; },
      render() {
        val.textContent = v === true ? 'On' : v === false ? 'Off' : v == null ? '—' : typeof v === 'number' ? fmt(v) : v;
        setStatus(st, o.ok ? (String(v).toLowerCase() === String(o.ok).toLowerCase() ? ['good', 'Normal'] : ['warning', 'Attention']) : null);
        since.textContent = changed ? 'Changed ' + timeFmt.format(changed) : '';
      },
    };
  },
});
