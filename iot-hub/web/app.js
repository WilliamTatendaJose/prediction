// App shell: sign-in, navigation, tenant switcher, hash router, live status
// and the alarm bell. Pages live in pages/*.js and are loaded on first visit.
import {
  $, h, icon, api, session, role, isSuper, canManage, canOperate, multiTenant, hasTenant, tenantLabel, safeGet, safeSet,
  whenSignedOut, Unauthorized, live, connectLive, disconnectLive, empty, linkButton, toast,
} from './core.js';

// ---- routes ------------------------------------------------------------------
// tenant: needs a tenant (a superadmin must pick one); show: who sees it.
const ROUTES = [
  { path: 'overview', label: 'Overview', icon: 'overview', group: 'Monitor', tenant: true },
  { path: 'dashboard', label: 'Dashboard', icon: 'dashboard', group: 'Monitor', tenant: true },
  { path: 'alarms', label: 'Alarms', icon: 'alarm', group: 'Monitor', tenant: true },
  { path: 'sensors', label: 'Sensors', icon: 'sensor', group: 'Assets', tenant: true },
  { path: 'devices', label: 'Devices & access', icon: 'device', group: 'Assets', tenant: true, show: canManage },
  { path: 'commands', label: 'Commands', icon: 'command', group: 'Assets', tenant: true, show: canOperate },
  { path: 'jobs', label: 'Stream jobs', icon: 'jobs', group: 'Automate', tenant: true, show: canManage },
  { path: 'notifications', label: 'Notifications', icon: 'bell', group: 'Automate', tenant: true, show: canManage },
  { path: 'grafana', label: 'Grafana', icon: 'grafana', group: 'Analyze', tenant: true, show: () => canManage() && session.me.grafana },
  { path: 'system', label: 'System', icon: 'system', group: 'Administer', tenant: true, show: canManage },
  { path: 'tenants', label: 'Tenants', icon: 'tenants', group: 'Platform', show: () => multiTenant() && isSuper() },
];
const visible = (r) => !r.show || r.show();

function parseHash() {
  const raw = location.hash.replace(/^#\/?/, '');
  // Old links: admin.html#devices, #sensors …
  const [path, ...rest] = raw.split('/').map((x) => { try { return decodeURIComponent(x); } catch { return x; } });
  return { path: path || '', params: rest.filter(Boolean) };
}
export function go(path) { location.hash = '#/' + path; }

function defaultRoute() {
  if (multiTenant() && isSuper() && !session.tenant) return 'tenants';
  return 'overview';
}

// ---- navigation ----------------------------------------------------------------
function renderNav(current) {
  const nav = $('nav');
  nav.replaceChildren();
  let group = null, ul = null;
  for (const r of ROUTES.filter(visible)) {
    if (r.group !== group) {
      group = r.group;
      ul = h('ul');
      nav.append(h('div', { class: 'nav-group' }, h('p', { class: 'nav-label', text: group }), ul));
    }
    const disabled = r.tenant && !hasTenant();
    ul.append(h('li', {}, h('a', {
      href: '#/' + r.path, class: 'nav-item' + (r.path === current ? ' active' : '') + (disabled ? ' disabled' : ''),
      'aria-current': r.path === current ? 'page' : null,
      title: disabled ? 'Choose a tenant first' : null,
    }, icon(r.icon), h('span', { text: r.label }), r.path === 'alarms' && h('span', { id: 'nav-alarms', class: 'nav-count', hidden: true }))));
  }
  renderAlarmCount();
}

function setCrumbs(parts, tenantScoped = true) {
  const c = $('crumbs');
  c.replaceChildren();
  const t = tenantLabel();
  if (t && tenantScoped) c.append(h('span', { class: 'crumb tenant', text: t }), h('span', { class: 'sep', text: '/' }));
  parts.forEach(([label, href], i) => {
    if (i) c.append(h('span', { class: 'sep', text: '/' }));
    c.append(href ? h('a', { class: 'crumb', href, text: label }) : h('span', { class: 'crumb current', text: label }));
  });
  document.title = (parts.at(-1)?.[0] ? parts.at(-1)[0] + ' · ' : '') + 'IoT Hub';
}

// ---- router ------------------------------------------------------------------
let leave = [];
let navSeq = 0;
async function route() {
  const { path, params } = parseHash();
  const r = ROUTES.find((x) => x.path === path);
  if (!r || !visible(r)) { history.replaceState(null, '', '#/' + defaultRoute()); return route(); }
  closeNav();
  for (const fn of leave) { try { fn(); } catch { /* page cleanup */ } }
  leave = [];
  const seq = ++navSeq;
  renderNav(r.path);
  setCrumbs([[r.label]], !!r.tenant);
  const el = $('page');
  el.replaceChildren(h('div', { class: 'loading' }, h('span', { class: 'spinner' }), 'Loading…'));
  if (r.tenant && !hasTenant()) {
    el.replaceChildren(empty('Choose a tenant', 'This page shows one tenant\'s data. Pick a tenant in the sidebar, or create the first one.',
      linkButton('Go to tenants', '#/tenants', { kind: 'primary' })));
    return;
  }
  try {
    const mod = await import(`./pages/${r.path}.js`);
    if (seq !== navSeq) return;
    const ctx = {
      params,
      onLeave: (fn) => leave.push(fn),
      crumbs: (extra) => setCrumbs([[r.label, extra?.length ? '#/' + r.path : null], ...(extra || [])], !!r.tenant),
      current: () => seq === navSeq,
      reload: () => route(),
    };
    el.replaceChildren();
    await mod.render(el, ctx);
    if (seq === navSeq && !params.length) el.focus({ preventScroll: true });
  } catch (e) {
    if (seq !== navSeq || e instanceof Unauthorized) return;
    el.replaceChildren(empty('Something went wrong', e.message || String(e), h('button', { class: 'btn', onclick: () => route() }, 'Try again')));
  }
}
addEventListener('hashchange', route);

// ---- mobile drawer -------------------------------------------------------------
function openNav() { document.body.classList.add('nav-open'); $('scrim').hidden = false; }
function closeNav() { document.body.classList.remove('nav-open'); $('scrim').hidden = true; }
$('nav-open').append(icon('menu')); $('nav-open').onclick = openNav;
$('nav-close').append(icon('close')); $('nav-close').onclick = closeNav;
$('scrim').onclick = closeNav;
addEventListener('keydown', (e) => { if (e.key === 'Escape') closeNav(); });

// ---- theme -------------------------------------------------------------------
const savedTheme = safeGet('iothub.theme');
if (savedTheme) document.documentElement.dataset.theme = savedTheme;
$('theme').append(icon('sun'));
$('theme').onclick = () => {
  const dark = document.documentElement.dataset.theme
    ? document.documentElement.dataset.theme === 'dark'
    : matchMedia('(prefers-color-scheme: dark)').matches;
  document.documentElement.dataset.theme = dark ? 'light' : 'dark';
  safeSet('iothub.theme', document.documentElement.dataset.theme);
  live.emit('theme'); // canvases read colors at draw time
};

// ---- live status and alarm bell ------------------------------------------------
live.on('state', (s) => {
  const c = $('conn');
  c.dataset.state = s;
  c.textContent = s === 'live' ? 'Live' : s === 'down' ? (navigator.onLine ? 'Reconnecting…' : 'Offline') : isSuper() && !hasTenant() ? 'Platform' : 'Idle';
});
const active = new Map(); // open, unshelved alarms
async function loadActive() {
  if (!hasTenant()) { active.clear(); renderAlarmCount(); return; }
  const list = await api('/api/anomalies?active=1&limit=1000').catch(() => []);
  active.clear();
  for (const a of list) if (!a.shelved) active.set(a.id, a);
  renderAlarmCount();
}
function renderAlarmCount() {
  const unacked = [...active.values()].filter((a) => !a.ack).length;
  const bell = $('bell');
  bell.hidden = !hasTenant();
  bell.replaceChildren(icon('alarm'));
  if (unacked) bell.append(h('span', { class: 'bell-count', text: unacked > 99 ? '99+' : unacked }));
  bell.title = unacked ? `${unacked} unacknowledged alarm${unacked === 1 ? '' : 's'}` : active.size ? `${active.size} active, all acknowledged` : 'No active alarms';
  bell.setAttribute('aria-label', bell.title);
  bell.classList.toggle('hot', unacked > 0);
  const n = $('nav-alarms');
  if (n) { n.hidden = !unacked; n.textContent = unacked; }
}
live.on('anomaly', (a) => { if (a.end || a.shelved) active.delete(a.id); else active.set(a.id, a); renderAlarmCount(); });
live.on('alarms', loadActive);

// ---- sign in -------------------------------------------------------------------
function showSignin(msg) {
  disconnectLive();
  $('app').hidden = true;
  $('signin').hidden = false;
  $('signin-err').textContent = msg || '';
  $('signin-public').hidden = !session.me.publicRead || !!session.me.identity;
  $('signin-token').focus();
}
whenSignedOut(() => showSignin(session.me.identity ? 'Your session ended. Sign in again.' : ''));
$('signin-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const res = await fetch('/api/login', {
    method: 'POST', credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'iothub' },
    body: JSON.stringify({ token: $('signin-token').value.trim() }),
  }).catch(() => null);
  if (!res) { $('signin-err').textContent = 'The hub did not answer. Check the connection and try again.'; return; }
  if (!res.ok) { $('signin-err').textContent = 'That token was not accepted.'; return; }
  $('signin-token').value = '';
  booted = false;
  boot();
});
$('signin-public').onclick = () => { $('signin').hidden = true; startApp(); };
$('logout').onclick = async () => {
  await fetch('/api/logout', { method: 'POST', credentials: 'same-origin', headers: { 'X-Requested-With': 'iothub' } }).catch(() => {});
  session.me = { authEnabled: true }; session.tenant = null;
  showSignin('You are signed out.');
};
$('login').onclick = () => showSignin();

const ROLE_NAMES = { superadmin: 'Platform operator', admin: 'Administrator', operator: 'Operator', viewer: 'Viewer' };
function renderUser() {
  const me = session.me, id = me.identity;
  const u = $('user');
  u.replaceChildren();
  if (id && me.authEnabled) {
    u.append(h('span', { class: 'avatar', text: (id.id || '?').slice(0, 1).toUpperCase() }),
      h('span', { class: 'user-text' }, h('b', { text: id.id }), h('small', { text: id.role === 'superadmin' && !multiTenant() ? 'Administrator' : ROLE_NAMES[id.role] || id.role })));
  } else if (!me.authEnabled) {
    u.append(h('span', { class: 'avatar', text: '·' }), h('span', { class: 'user-text' }, h('b', { text: 'Open access' }), h('small', { text: 'Authentication is off' })));
  } else {
    u.append(h('span', { class: 'avatar', text: '?' }), h('span', { class: 'user-text' }, h('b', { text: 'Guest' }), h('small', { text: 'Read-only' })));
  }
  $('logout').hidden = !me.authEnabled || !id;
  $('login').hidden = !me.authEnabled || !!id;
}

// ---- tenant switcher (multi-tenant superadmin) ---------------------------------
function setupTenants() {
  const ws = $('workspace'), sel = $('tenant');
  if (!multiTenant() || !isSuper()) {
    session.tenant = null;
    ws.hidden = !(multiTenant() && session.me.tenant);
    if (!ws.hidden) { sel.replaceChildren(h('option', { text: session.me.tenant.name || session.me.tenant.id })); sel.disabled = true; }
    return;
  }
  const ts = (session.me.tenants || []).filter((t) => t.status === 'active');
  const saved = safeGet('iothub.tenant');
  session.tenant = ts.some((t) => t.id === saved) ? saved : ts[0]?.id || null;
  sel.disabled = false;
  sel.replaceChildren(...ts.map((t) => h('option', { value: t.id, text: t.name ? `${t.name} (${t.id})` : t.id })));
  if (!ts.length) sel.append(h('option', { value: '', text: 'No active tenants' }));
  sel.value = session.tenant || '';
  ws.hidden = false;
  sel.onchange = () => switchTenant(sel.value);
}
export function switchTenant(id) {
  session.tenant = id || null;
  safeSet('iothub.tenant', session.tenant);
  $('tenant').value = id || '';
  connectLive();
  loadActive();
  const { path } = parseHash();
  const r = ROUTES.find((x) => x.path === path);
  if (r?.tenant) { go(r.path); route(); } else route();
}
// Pages that change the tenant list (create, suspend) refresh the switcher.
export async function refreshMe() {
  session.me = await api('/api/me');
  const before = session.tenant;
  setupTenants();
  renderNav(parseHash().path);
  if (session.tenant !== before) { connectLive(); loadActive(); }
}

// ---- boot ----------------------------------------------------------------------
// Retries until the hub answers: an installed app is often opened before the
// network (or the hub) is back.
let bootDelay = 2000, bootTimer = null, booting = false, booted = false;
async function boot() {
  if (booting || booted) return;
  booting = true;
  clearTimeout(bootTimer);
  try {
    const r = await fetch('/api/me', { credentials: 'same-origin' });
    session.me = await r.json();
    booted = true;
    if (r.status === 401 && !session.me.publicRead) { showSignin(); return; }
    startApp();
  } catch {
    $('signin').hidden = true;
    $('app').hidden = false;
    $('conn').dataset.state = 'down';
    $('conn').textContent = navigator.onLine ? 'Hub unreachable' : 'Offline';
    $('page').replaceChildren(empty(navigator.onLine ? "Can't reach the hub" : 'No network connection',
      'The app will load by itself when the connection returns.'));
    bootTimer = setTimeout(boot, bootDelay);
    bootDelay = Math.min(bootDelay * 2, 60000);
  } finally {
    booting = false;
  }
}
function startApp() {
  $('signin').hidden = true;
  $('app').hidden = false;
  document.body.dataset.role = role() || 'guest';
  setupTenants();
  renderUser();
  connectLive();
  loadActive();
  if (!location.hash || location.hash === '#') history.replaceState(null, '', '#/' + defaultRoute());
  route();
}
addEventListener('online', () => { bootDelay = 2000; if (!booted) boot(); else connectLive(); });
addEventListener('offline', () => live.emit('state', 'down'));
// Hidden tabs hold no connection; on return, resync.
document.addEventListener('visibilitychange', () => {
  if (!booted || $('app').hidden) return;
  if (document.hidden) { disconnectLive(); return; }
  connectLive(); loadActive(); live.emit('resync');
});
boot();

// ---- install (PWA) -------------------------------------------------------------
if ('serviceWorker' in navigator && window.isSecureContext) {
  navigator.serviceWorker.register('/sw.js').catch(() => { /* still works as a page */ });
}
let installPrompt = null;
addEventListener('beforeinstallprompt', (e) => { e.preventDefault(); installPrompt = e; $('install').hidden = false; });
addEventListener('appinstalled', () => { installPrompt = null; $('install').hidden = true; toast('Installed'); });
$('install').onclick = async () => {
  if (!installPrompt) return;
  installPrompt.prompt();
  await installPrompt.userChoice.catch(() => {});
  installPrompt = null;
  $('install').hidden = true;
};
