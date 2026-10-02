// Tile registry. Adding a tile type = one registerTile() call; the server
// stores the layout as opaque JSON so no backend change is needed.
//
// registerTile(type, {
//   label,                 // shown in the "Add tile" dialog
//   multi,                 // true: tile takes several fields (cfg.fields), else cfg.field
//   options: [{ key, label, type: 'number'|'text' }],
//   create(el, cfg, ctx) -> { update(t, value|values), render(), destroy?() }
// })
//
// ctx = { sensor, unit(field), history(field, limit) -> Promise<{t:[],v:[]}>, invalidate() }
// update() only records data; render() runs at most once per animation frame.

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

// ---- line: up to 4 fields over time, crosshair tooltip --------------------
// All fields share one y-axis, so only group fields with the same unit/scale;
// put different measures in separate tiles.
registerTile('line', {
  label: 'Line chart',
  multi: true,
  defaultSize: { w: 2, h: 2 },
  options: [
    { key: 'points', label: 'Points kept', type: 'number' },
    { key: 'min', label: 'Y min', type: 'number' }, { key: 'max', label: 'Y max', type: 'number' },
  ],
  create(el, cfg, ctx) {
    const o = cfg.options || {};
    const fields = (cfg.fields || [cfg.field]).slice(0, SERIES.length);
    const cap = Math.min(5000, Math.max(10, num(o.points) ?? 300));
    const body = header(el, cfg, ctx, fields);
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
    const series = fields.map(() => new Series(cap));
    fields.forEach((f, k) => ctx.history(f, cap).then((h) => { series[k].load(h); ctx.invalidate(); }));
    let hoverX = null;
    const pad = { l: 36, r: 6, t: 6, b: 16 };
    wrap.addEventListener('pointermove', (ev) => { hoverX = ev.offsetX; ctx.invalidate(); });
    wrap.addEventListener('pointerleave', () => { hoverX = null; tip.style.display = 'none'; ctx.invalidate(); });
    const ro = new ResizeObserver(() => ctx.invalidate()); ro.observe(wrap);

    return {
      update(t, values) {
        fields.forEach((f, k) => { const v = values[f]; if (typeof v === 'number') series[k].push(t, v); else if (typeof v === 'boolean') series[k].push(t, +v); });
      },
      render() {
        const [g, W, H] = fitCanvas(cv);
        const w = W - pad.l - pad.r, h = H - pad.t - pad.b;
        if (w < 10 || h < 10 || series.every((s) => s.n === 0)) return;
        const e = extent(series, o);
        g.font = '10px system-ui, sans-serif'; g.fillStyle = css('--muted'); g.textBaseline = 'middle'; g.textAlign = 'right';
        g.strokeStyle = css('--grid'); g.lineWidth = 1;
        for (let i = 0; i <= 3; i++) { // recessive hairline grid + y ticks
          const y = Math.round(pad.t + (h * i) / 3) + 0.5;
          g.beginPath(); g.moveTo(pad.l, y); g.lineTo(pad.l + w, y); g.stroke();
          g.fillText(fmt(e.hi - ((e.hi - e.lo) * i) / 3), pad.l - 4, y);
        }
        g.textBaseline = 'alphabetic';
        g.textAlign = 'left'; g.fillText(timeFmt.format(e.t0), pad.l, H - 2);
        g.textAlign = 'right'; g.fillText(timeFmt.format(e.t1), pad.l + w, H - 2);
        const colors = fields.map((_, k) => css(SERIES[k]));
        drawLines(g, series, colors, pad.l, pad.t, w, h, e, 2);

        if (hoverX == null || hoverX < pad.l || hoverX > pad.l + w) { tip.style.display = 'none'; return; }
        // Crosshair snaps to the nearest sample time of the first non-empty series.
        const tx = e.t0 + ((hoverX - pad.l) / w) * (e.t1 - e.t0);
        const ref = series.find((s) => s.n) ;
        let best = 0, bd = Infinity;
        for (let i = 0; i < ref.n; i++) { const d = Math.abs(ref.t[ref.idx(i)] - tx); if (d < bd) { bd = d; best = i; } }
        const ts = ref.t[ref.idx(best)];
        const x = pad.l + ((ts - e.t0) / (e.t1 - e.t0)) * w;
        g.strokeStyle = css('--axis'); g.beginPath(); g.moveTo(Math.round(x) + 0.5, pad.t); g.lineTo(Math.round(x) + 0.5, pad.t + h); g.stroke();
        tip.replaceChildren();
        const tt = document.createElement('div'); tt.className = 't'; tt.textContent = timeFmt.format(ts); tip.append(tt);
        series.forEach((s, k) => {
          let v; // value at the nearest time in this series
          let d = Infinity;
          for (let i = 0; i < s.n; i++) { const j = s.idx(i), dd = Math.abs(s.t[j] - ts); if (dd < d) { d = dd; v = s.v[j]; } }
          if (v === undefined) return;
          g.fillStyle = colors[k]; g.strokeStyle = css('--surface'); g.lineWidth = 2;
          const y = pad.t + h - ((v - e.lo) / (e.hi - e.lo)) * h;
          g.beginPath(); g.arc(x, y, 4, 0, 7); g.fill(); g.stroke();
          const row = document.createElement('div'); row.className = 'row'; row.style.setProperty('--c', `var(${SERIES[k]})`);
          const b = document.createElement('b'); b.textContent = fmt(v) + ' ' + ctx.unit(fields[k]);
          const n = document.createElement('span'); n.textContent = fields[k];
          row.append(b, n); tip.append(row);
        });
        tip.style.display = 'block';
        const left = x + 12 + tip.offsetWidth > W ? x - 12 - tip.offsetWidth : x + 12;
        tip.style.left = left + 'px'; tip.style.top = pad.t + 'px';
      },
      destroy() { ro.disconnect(); },
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
