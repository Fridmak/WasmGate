'use strict';

/* WasmGate control plane - admin UI.
 *
 * Served by the control plane itself (wwwroot), so it talks to the admin API on
 * the same origin and needs no CORS or build step. The bearer token is kept in
 * sessionStorage: closing the tab clears it.
 */

const ENV_KEY = 'wasmgate.env';
const TOKEN_KEY = 'wasmgate.token';
const REFRESH_MS = 15000;

const AGENT_STATUS = ['Pending', 'Online', 'Offline', 'Draining', 'Disabled'];
const DEPLOY_STATUS = ['Pending', 'Running', 'Succeeded', 'Failed', 'Rolled back', 'Cancelled'];
const WAVE_STATUS = ['Pending', 'Running', 'Succeeded', 'Failed', 'Skipped'];
const RELEASE_STATUS = ['Draft', 'Validated', 'Published', 'Revoked'];
const ARTIFACT_KINDS = ['Unspecified', 'WASM', 'Certificate', 'Key', 'Other'];

const state = {
  view: 'dashboard',
  env: localStorage.getItem(ENV_KEY) || 'prod',
  envs: [],
  token: sessionStorage.getItem(TOKEN_KEY) || '',
  busy: false,
};

const $ = (selector) => document.querySelector(selector);

/** Maps the URL fragment to a view, so reload and back/forward keep the tab. */
function viewFromHash() {
  const candidate = location.hash.replace('#', '');
  return Object.hasOwn(VIEWS, candidate) ? candidate : 'dashboard';
}

document.addEventListener('DOMContentLoaded', () => {
  state.view = viewFromHash();
  $('#env-current').textContent = state.env;
  $('#token').value = state.token;
  setActiveNav();

  window.addEventListener('hashchange', () => {
    const view = viewFromHash();
    if (view === state.view) return;
    state.view = view;
    setActiveNav();
    refresh();
  });

  $('#save').addEventListener('click', saveSettings);
  $('#refresh').addEventListener('click', () => refresh());
  $('#env-button').addEventListener('click', (event) => {
    event.stopPropagation();
    setEnvPopover($('#env-popover').hidden);
  });
  $('#env-create').addEventListener('submit', createEnvironment);

  document.addEventListener('click', (event) => {
    if (!event.target.closest('.field.env')) setEnvPopover(false);

    const envDelete = event.target.closest('[data-env-delete]');
    if (envDelete) {
      deleteEnvironment(envDelete.dataset.envDelete);
      return;
    }

    const envOption = event.target.closest('[data-env-option]');
    if (envOption) {
      setEnvironment(envOption.dataset.envOption);
      return;
    }

    const viewButton = event.target.closest('[data-view]');
    if (viewButton) {
      showView(viewButton.dataset.view);
      return;
    }

    if (event.target.closest('[data-close]') || event.target.id === 'modal') {
      closeModal();
      return;
    }

    const target = event.target.closest('[data-action]');
    if (!target) return;

    const action = target.dataset.action;
    const id = target.dataset.id;
    const handler = ACTIONS[action];
    if (handler) {
      event.preventDefault();
      Promise.resolve(handler(id, target)).catch((error) => toast(error.message, 'error'));
    }
  });

  document.addEventListener('keydown', (event) => {
    if (event.key !== 'Escape') return;
    if (!$('#env-popover').hidden) {
      setEnvPopover(false);
      return;
    }
    closeModal();
  });

  showView(state.view);
  loadEnvironments().catch(() => { /* keep the saved name until the API answers */ });
  setInterval(() => {
    // Only the dashboard is refreshed automatically: re-rendering a table would
    // collapse open rows and detach whatever the operator is interacting with.
    if (state.view !== 'dashboard') return;
    if (!$('#modal').hidden) return;
    if (document.visibilityState !== 'visible') return;
    refresh();
  }, REFRESH_MS);
});

/* ------------------------------------------------------------------ */
/* Settings and API                                                    */
/* ------------------------------------------------------------------ */

function saveSettings() {
  state.token = $('#token').value.trim();
  sessionStorage.setItem(TOKEN_KEY, state.token);
  hideBanner();
  toast('Settings saved', 'ok');
  loadEnvironments().catch(() => {});
  refresh();
}

/* ------------------------------------------------------------------ */
/* Environments                                                        */
/* ------------------------------------------------------------------ */

/** Loads the environment catalog and redraws the picker. */
async function loadEnvironments() {
  const list = await api('/api/v1/environments');
  state.envs = sortEnvironments(list.map((item) => item.name).filter(Boolean));
  renderEnvironmentOptions();
  $('#env-current').textContent = state.env;
}

/** Puts the active environment first, then prod, then the rest by name. */
function sortEnvironments(names) {
  return [...new Set(names)].sort((left, right) => {
    if (left === state.env) return -1;
    if (right === state.env) return 1;
    if (left === 'prod') return -1;
    if (right === 'prod') return 1;
    return left.localeCompare(right);
  });
}

/** Returns the catalog, loading it if this is the first use. */
async function ensureEnvironments() {
  if (state.envs.length === 0) await loadEnvironments();
  return state.envs;
}

function renderEnvironmentOptions() {
  const list = $('#env-list');
  if (!list) return;
  list.innerHTML = state.envs.length === 0
    ? '<p class="muted" style="padding:6px 8px;margin:0">No environments yet</p>'
    : state.envs.map((name) => `
      <div class="env-option-row">
        <button class="env-option ${name === state.env ? 'active' : ''}" type="button"
                role="option" aria-selected="${name === state.env}" data-env-option="${esc(name)}">
          <span>${esc(name)}</span>
          ${name === state.env ? '<span class="env-option-check" aria-hidden="true">✓</span>' : ''}
        </button>
        ${name === 'prod' ? '' : `
          <button class="env-delete" type="button" title="Delete environment"
                  aria-label="Delete environment ${esc(name)}" data-env-delete="${esc(name)}">✕</button>`}
      </div>`).join('');
}

function setEnvPopover(open) {
  $('#env-popover').hidden = !open;
  $('#env-button').setAttribute('aria-expanded', String(open));
  if (!open) return;
  $('#env-error').hidden = true;
  loadEnvironments().catch((error) => showEnvError(error.message));
}

function showEnvError(message) {
  const element = $('#env-error');
  element.textContent = message;
  element.hidden = false;
}

/** Switches the environment shown by every screen. */
function setEnvironment(name) {
  const next = String(name || '').trim();
  if (!next) return;
  state.env = next;
  localStorage.setItem(ENV_KEY, next);
  $('#env-current').textContent = next;
  state.envs = sortEnvironments(state.envs);
  renderEnvironmentOptions();
  setEnvPopover(false);
  hideBanner();
  refresh();
}

async function createEnvironment(event) {
  event.preventDefault();
  const input = $('#env-new');
  const name = input.value.trim();
  if (!name) return;

  const button = $('#env-create button');
  button.disabled = true;
  try {
    const created = await api('/api/v1/environments', {
      method: 'POST',
      body: { name },
    });
    input.value = '';
    await loadEnvironments();
    setEnvironment(created.name);
    toast(`Environment ${created.name} selected`, 'ok');
  } catch (error) {
    showEnvError(error.message);
  } finally {
    button.disabled = false;
  }
}

/** Deletes an empty environment, or reports why it is still in use. */
async function deleteEnvironment(name) {
  if (!confirm(
    `Delete environment "${name}"?\n\n` +
    'Its configurations, releases, deployments and enrollment tokens will be deleted. ' +
    'This cannot be undone. Agents must be moved to another environment first.')) {
    return;
  }

  try {
    await api(`/api/v1/environments/${encodeURIComponent(name)}`, { method: 'DELETE' });
    toast(`Environment ${name} deleted`, 'ok');
    if (state.env === name) {
      await loadEnvironments();
      setEnvironment('prod');
    } else {
      await loadEnvironments();
    }
  } catch (error) {
    showEnvError(error.message);
  }
}

function authHeaders(hasBody) {
  const headers = {};
  if (hasBody) headers['Content-Type'] = 'application/json';
  if (state.token) headers.Authorization = `Bearer ${state.token}`;
  return headers;
}

async function api(path, options = {}) {
  const method = options.method || 'GET';
  const response = await fetch(path, {
    method,
    headers: authHeaders(Boolean(options.body)),
    body: options.body ? JSON.stringify(options.body) : undefined,
  });

  if (response.status === 401) {
    showUnauthorized();
    throw new Error('Unauthorized: set the admin token');
  }

  if (response.status === 204) return null;

  const text = await response.text();
  let data = text;
  if (text) {
    try { data = JSON.parse(text); } catch { /* plain text error */ }
  } else {
    data = null;
  }

  if (!response.ok) {
    const message = typeof data === 'string'
      ? data
      : data?.error?.message || data?.title || response.statusText;
    throw new Error(message || `HTTP ${response.status}`);
  }

  return data;
}

async function probe(path) {
  try {
    const response = await fetch(path);
    const text = (await response.text()).trim();
    return { ok: response.ok, label: response.ok ? text || 'ok' : `HTTP ${response.status}` };
  } catch {
    return { ok: false, label: 'unreachable' };
  }
}

function showUnauthorized() {
  const banner = $('#banner');
  banner.hidden = false;
  banner.textContent = 'The admin API rejected the token. Paste WASMGATE_CP_ADMIN_TOKEN above and press Save.';
  $('#token').focus();
}

function hideBanner() {
  $('#banner').hidden = true;
}

/* ------------------------------------------------------------------ */
/* Rendering helpers                                                   */
/* ------------------------------------------------------------------ */

function esc(value) {
  return String(value ?? '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}

function fmtTime(value) {
  if (!value) return '—';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString();
}

function fmtBytes(value) {
  const size = Number(value) || 0;
  if (size < 1024) return `${size} B`;
  const units = ['KiB', 'MiB', 'GiB'];
  let result = size / 1024;
  let unit = 0;
  while (result >= 1024 && unit < units.length - 1) { result /= 1024; unit += 1; }
  return `${result.toFixed(result >= 10 ? 0 : 1)} ${units[unit]}`;
}

function short(value, length = 12) {
  const text = String(value ?? '');
  return text.length > length ? text.slice(0, length) + '…' : text;
}

function badge(text, kind) {
  return `<span class="badge ${kind}">${esc(text)}</span>`;
}

function agentBadge(status) {
  const kind = status === 1 ? 'ok' : status === 0 ? 'warn' : status === 2 ? 'neutral' : 'bad';
  return badge(AGENT_STATUS[status] ?? `#${status}`, kind);
}

function deployBadge(status) {
  const kind = status === 2 ? 'ok' : status === 1 ? 'warn' : status === 3 ? 'bad' : 'neutral';
  return badge(DEPLOY_STATUS[status] ?? `#${status}`, kind);
}

function waveBadge(status) {
  const kind = status === 2 ? 'ok' : status === 1 ? 'warn' : status === 3 ? 'bad' : 'neutral';
  return badge(WAVE_STATUS[status] ?? `#${status}`, kind);
}

function releaseBadge(status) {
  const kind = status === 3 ? 'bad' : status >= 1 ? 'ok' : 'neutral';
  return badge(RELEASE_STATUS[status] ?? `#${status}`, kind);
}

function chips(labels) {
  const entries = Object.entries(labels || {});
  if (entries.length === 0) return '<span class="muted">—</span>';
  return `<div class="chips">${entries
    .map(([key, value]) => `<span class="chip"><b>${esc(key)}</b>=${esc(value)}</span>`)
    .join('')}</div>`;
}

function emptyRow(columns, text) {
  return `<tr><td colspan="${columns}" class="empty">${esc(text)}</td></tr>`;
}

function viewHead(title, subtitle, actions = '') {
  return `<div class="view-head">
    <h1>${esc(title)}</h1>${subtitle ? `<span class="sub">${esc(subtitle)}</span>` : ''}
    <div class="spacer"></div>${actions}
  </div>`;
}

function render(html) {
  $('#view').innerHTML = html;
}

function setActiveNav() {
  document.querySelectorAll('.nav-item').forEach((item) => {
    item.classList.toggle('active', item.dataset.view === state.view);
  });
}

async function showView(view) {
  state.view = view;
  if (location.hash !== `#${view}`) history.replaceState(null, '', `#${view}`);
  setActiveNav();
  await refresh();
}

async function refresh() {
  if (state.busy) return;
  state.busy = true;
  const refreshButton = $('#refresh');
  refreshButton.disabled = true;
  try {
    await VIEWS[state.view]();
    hideBanner();
  } catch (error) {
    const message = error instanceof TypeError
      ? 'The control plane is unreachable.'
      : error.message;
    render(`<div class="panel"><div class="error-box">${esc(message)}</div></div>`);
  } finally {
    state.busy = false;
    refreshButton.disabled = false;
  }
}

/* ------------------------------------------------------------------ */
/* Views                                                               */
/* ------------------------------------------------------------------ */

const VIEWS = {
  dashboard: viewDashboard,
  agents: viewAgents,
  artifacts: viewArtifacts,
  configs: viewConfigs,
  releases: viewReleases,
  deployments: viewDeployments,
  audit: viewAudit,
};

async function viewDashboard() {
  const [health, ready, allAgents, deployments, audit] = await Promise.all([
    probe('/healthz'),
    probe('/readyz'),
    api('/api/v1/agents'),
    api(`/api/v1/deployments/${encodeURIComponent(state.env)}`),
    api('/api/v1/audit'),
  ]);

  updateHealth(health, ready);

  const agents = allAgents.filter((agent) => agent.environment === state.env);
  const online = agents.filter((agent) => agent.status === 1).length;
  const running = deployments.filter((item) => item.status === 1).length;
  const failed = deployments.filter((item) => item.status === 3).length;

  const recentDeployments = deployments.slice(0, 5);
  const recentAudit = audit.slice(0, 6);

  render(`
    ${viewHead('Dashboard', state.env, `
      <span class="badge ${health.ok ? 'ok' : 'bad'}">CP ${esc(health.label)}</span>
      <span class="badge ${ready.ok ? 'ok' : 'bad'}">DB ${esc(ready.label)}</span>`)}
    <div class="cards">
      <div class="card"><div class="label">Agents</div><div class="value">${agents.length}</div><div class="note">${online} online in ${esc(state.env)}</div></div>
      <div class="card ${running ? 'warn' : ''}"><div class="label">Deployments running</div><div class="value">${running}</div><div class="note">${deployments.length} recent</div></div>
      <div class="card ${failed ? 'bad' : ''}"><div class="label">Deployments failed</div><div class="value">${failed}</div><div class="note">in the last 100</div></div>
      <div class="card"><div class="label">Audit events</div><div class="value">${audit.length}</div><div class="note">last 100</div></div>
    </div>

    <div class="panel">
      <div class="panel-head"><h2>Maintenance</h2><div class="spacer"></div>
        <button class="btn small" data-action="backup-download" type="button">Download SQLite backup</button></div>
      <div class="panel-body">
        <p class="muted">Creates a consistent snapshot with SQLite <code>VACUUM INTO</code>
          and downloads it. The snapshot contains token hashes, certificates and
          configuration documents; store it like the rest of the control-plane data.</p>
      </div>
    </div>

    <div class="panel">
      <div class="panel-head"><h2>Recent deployments</h2><div class="spacer"></div>
        <button class="btn small" data-view="deployments" type="button">Open</button></div>
      <div class="panel-body flush">
        <table>
          <thead><tr><th>Status</th><th>Release</th><th>Waves</th><th>Created</th></tr></thead>
          <tbody>${recentDeployments.length === 0
            ? emptyRow(4, 'No deployments in this environment yet')
            : recentDeployments.map((item) => `
              <tr>
                <td>${deployBadge(item.status)}</td>
                <td class="mono">${esc(short(item.releaseId))}</td>
                <td>${item.waves.map((wave) => waveBadge(wave.status)).join(' ')}</td>
                <td class="nowrap muted">${esc(fmtTime(item.createdAt))}</td>
              </tr>`).join('')}
          </tbody>
        </table>
      </div>
    </div>

    <div class="panel">
      <div class="panel-head"><h2>Recent activity</h2><div class="spacer"></div>
        <button class="btn small" data-view="audit" type="button">Open</button></div>
      <div class="panel-body flush">
        <table>
          <thead><tr><th>Action</th><th>Actor</th><th>Target</th><th>At</th></tr></thead>
          <tbody>${recentAudit.length === 0
            ? emptyRow(4, 'No audit events yet')
            : recentAudit.map((event) => `
              <tr>
                <td>${esc(event.action)}</td>
                <td class="mono">${esc(short(event.actor, 28))}</td>
                <td class="mono">${esc(short(event.target, 18))}</td>
                <td class="nowrap muted">${esc(fmtTime(event.at))}</td>
              </tr>`).join('')}
          </tbody>
        </table>
      </div>
    </div>
  `);
}

function updateHealth(health, ready) {
  const element = $('#health');
  const ok = health.ok && ready.ok;
  element.className = `health ${ok ? 'ok' : 'bad'}`;
  element.querySelector('span:last-child').textContent = ok
    ? 'control plane ready'
    : `degraded: CP ${health.label}, DB ${ready.label}`;
}

async function viewAgents() {
  const allAgents = await api('/api/v1/agents');
  const agents = allAgents.filter((agent) => agent.environment === state.env);

  render(`
    ${viewHead('Agents', `${agents.length} in ${state.env}`, `
      <button class="btn primary" data-action="agent-new" type="button">New agent</button>`)}
    <div class="panel"><div class="panel-body flush">
      <table>
        <thead><tr><th>Name</th><th>Environment</th><th>Status</th><th>Labels</th><th>Last seen</th><th>Release</th><th></th></tr></thead>
        <tbody>${agents.length === 0
          ? emptyRow(7, 'No agents in this environment yet: create one or switch the environment')
          : agents.map((agent) => `
            <tr>
              <td><b>${esc(agent.name)}</b><div class="mono muted">${esc(agent.id)}</div></td>
              <td>${badge(agent.environment, 'neutral')}</td>
              <td>${agentBadge(agent.status)}</td>
              <td>${chips(agent.labels)}</td>
              <td class="nowrap muted">${esc(fmtTime(agent.lastSeenAt))}</td>
              <td class="mono">${agent.activeReleaseId ? esc(short(agent.activeReleaseId)) : '<span class="muted">—</span>'}</td>
              <td class="actions">
                <button class="btn small" data-action="agent-details" data-id="${esc(agent.id)}" type="button">Details</button>
                <button class="btn small" data-action="agent-token" data-id="${esc(agent.id)}" type="button">New token</button>
              </td>
            </tr>`).join('')}
        </tbody>
      </table>
    </div></div>
  `);
}

async function viewArtifacts() {
  const artifacts = await api('/api/v1/artifacts');

  render(`
    ${viewHead('Artifacts', `${artifacts.length} stored`, `
      <button class="btn primary" data-action="artifact-upload" type="button">Upload artifact</button>`)}
    <div class="panel"><div class="panel-body flush">
      <table>
        <thead><tr><th>Hash</th><th>Kind</th><th>Size</th><th>Media type</th><th>Uploaded</th><th>By</th></tr></thead>
        <tbody>${artifacts.length === 0
          ? emptyRow(6, 'No artifacts yet: upload a WASM plugin or a file')
          : artifacts.map((artifact) => `
            <tr>
              <td class="mono">
                <div class="copy-row">
                  <span class="mono">${esc(short(artifact.id, 20))}</span>
                  <button class="btn small" data-action="copy" data-value="${esc(artifact.id)}" type="button">Copy</button>
                </div>
              </td>
              <td>${badge(ARTIFACT_KINDS[artifact.kind] ?? `#${artifact.kind}`, 'neutral')}</td>
              <td class="nowrap">${esc(fmtBytes(artifact.size))}</td>
              <td class="mono">${esc(artifact.mediaType || '—')}</td>
              <td class="nowrap muted">${esc(fmtTime(artifact.createdAt))}</td>
              <td class="mono">${esc(artifact.createdBy || '—')}</td>
            </tr>`).join('')}
        </tbody>
      </table>
    </div></div>
  `);
}

async function viewConfigs() {
  const configs = await api(`/api/v1/configs/${encodeURIComponent(state.env)}`);

  render(`
    ${viewHead('Configurations', state.env, `
      <button class="btn primary" data-action="config-new" type="button">New configuration</button>`)}
    <div class="panel"><div class="panel-body flush">
      <table>
        <thead><tr><th>Hash</th><th>Message</th><th>Created</th><th>By</th><th></th></tr></thead>
        <tbody>${configs.length === 0
          ? emptyRow(5, 'No configuration versions in this environment')
          : configs.map((config) => `
            <tr>
              <td class="mono">${esc(short(config.sha256, 20))}</td>
              <td>${esc(config.message || '—')}</td>
              <td class="nowrap muted">${esc(fmtTime(config.createdAt))}</td>
              <td class="mono">${esc(config.createdBy)}</td>
              <td class="actions">
                <button class="btn small" data-action="config-yaml" data-id="${esc(config.id)}" type="button">View YAML</button>
              </td>
            </tr>`).join('')}
        </tbody>
      </table>
    </div></div>
  `);
}

async function viewReleases() {
  const releases = await api(`/api/v1/releases/${encodeURIComponent(state.env)}`);

  render(`
    ${viewHead('Releases', state.env, `
      <button class="btn primary" data-action="release-new" type="button">New release</button>`)}
    <div class="panel"><div class="panel-body flush">
      <table>
        <thead><tr><th>Release</th><th>Config</th><th>Artifacts</th><th>Status</th><th>Created</th><th></th></tr></thead>
        <tbody>${releases.length === 0
          ? emptyRow(6, 'No releases in this environment')
          : releases.map((release) => `
            <tr>
              <td class="mono">${esc(short(release.id, 16))}</td>
              <td class="mono">${esc(short(release.configSha256, 16))}</td>
              <td>${release.artifacts.length === 0 ? '<span class="muted">—</span>' : `
                <details class="wave"><summary>${release.artifacts.length} file(s)</summary>
                  ${release.artifacts.map((artifact) => `
                    <div class="wave-row"><span class="mono">${esc(artifact.name)}</span>
                    <span class="muted mono">${esc(short(artifact.artifactId, 10))}</span></div>`).join('')}
                </details>`}
              </td>
              <td>${releaseBadge(release.status)}</td>
              <td class="nowrap muted">${esc(fmtTime(release.createdAt))}</td>
              <td class="actions">
                ${release.status === 3
                  ? '<span class="muted">revoked</span>'
                  : `<button class="btn small danger" data-action="release-revoke" data-id="${esc(release.id)}" type="button">Revoke</button>`}
              </td>
            </tr>`).join('')}
        </tbody>
      </table>
    </div></div>
  `);
}

async function viewDeployments() {
  const deployments = await api(`/api/v1/deployments/${encodeURIComponent(state.env)}`);

  render(`
    ${viewHead('Deployments', state.env, `
      <button class="btn primary" data-action="deployment-new" type="button">New deployment</button>`)}
    <div class="panel"><div class="panel-body flush">
      <table>
        <thead><tr><th>Status</th><th>Release</th><th>Waves</th><th>Created</th><th>Finished</th><th></th></tr></thead>
        <tbody>${deployments.length === 0
          ? emptyRow(6, 'No deployments in this environment')
          : deployments.map((item) => `
            <tr>
              <td>${deployBadge(item.status)}${item.message ? `<div class="subline">${esc(short(item.message, 76))}</div>` : ''}</td>
              <td class="mono">${esc(short(item.releaseId))}</td>
              <td>
                <details class="wave"><summary>${item.waves.length} wave(s)</summary>
                  ${item.waves.map((wave) => `
                    <div class="wave-row">
                      <span class="badge neutral">#${wave.index}</span>
                      <span class="mono">${esc(wave.selector)}</span>
                      ${waveBadge(wave.status)}
                    </div>`).join('')}
                </details>
              </td>
              <td class="nowrap muted">${esc(fmtTime(item.createdAt))}</td>
              <td class="nowrap muted">${esc(fmtTime(item.completedAt))}</td>
              <td class="actions">
                <button class="btn small" data-action="deployment-details" data-id="${esc(item.id)}" type="button">Details</button>
                ${renderRollback(item)}
              </td>
            </tr>`).join('')}
        </tbody>
      </table>
    </div></div>
  `);
}

/**
 * Rollback is only meaningful for terminal deployments and only when an older
 * release exists in the same environment. The server computes the target; the
 * button is disabled instead of failing after a click.
 */
function renderRollback(item) {
  if (item.status !== 2 && item.status !== 3) return '';
  if (!item.rollbackReleaseId) {
    return `<span title="No older release exists in this environment">
      <button class="btn small" type="button" disabled>Rollback</button>
    </span>`;
  }

  return `<button class="btn small" data-action="deployment-rollback"
    data-id="${esc(item.id)}" type="button">Rollback</button>`;
}

async function viewAudit() {
  const events = await api('/api/v1/audit');

  render(`
    ${viewHead('Audit', `${events.length} recent events`)}
    <div class="panel">
      <div class="panel-head">
        <input id="audit-filter" placeholder="Filter by action, actor or target" style="flex:1">
      </div>
      <div class="panel-body flush">
        <table>
          <thead><tr><th>At</th><th>Action</th><th>Actor</th><th>Target</th></tr></thead>
          <tbody id="audit-body">${events.map((event) => `
            <tr data-search="${esc(`${event.action} ${event.actor} ${event.target}`)}">
              <td class="nowrap muted">${esc(fmtTime(event.at))}</td>
              <td>${esc(event.action)}</td>
              <td class="mono">${esc(event.actor)}</td>
              <td class="mono">${esc(event.target)}</td>
            </tr>`).join('') || emptyRow(4, 'No audit events yet')}
          </tbody>
        </table>
      </div>
    </div>
  `);

  const filter = $('#audit-filter');
  filter.addEventListener('input', () => {
    const term = filter.value.toLowerCase();
    document.querySelectorAll('#audit-body tr[data-search]').forEach((row) => {
      row.hidden = !row.dataset.search.toLowerCase().includes(term);
    });
  });
  filter.focus();
}

/* ------------------------------------------------------------------ */
/* Modal and toasts                                                    */
/* ------------------------------------------------------------------ */

function showModal(title, body, actions = []) {
  $('#modal-title').textContent = title;
  $('#modal-body').innerHTML = body;
  const footer = $('#modal-foot');
  footer.innerHTML = '';

  for (const action of actions) {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = `btn ${action.kind || ''}`.trim();
    button.textContent = action.label;
    button.onclick = async () => {
      button.disabled = true;
      try {
        await action.onClick();
      } catch (error) {
        toast(error.message, 'error');
      } finally {
        button.disabled = false;
      }
    };
    footer.appendChild(button);
  }

  $('#modal').hidden = false;
  const first = $('#modal-body input, #modal-body select, #modal-body textarea');
  if (first) first.focus();
}

function closeModal() {
  $('#modal').hidden = true;
  $('#modal-body').innerHTML = '';
  $('#modal-foot').innerHTML = '';
}

function toast(message, kind = '') {
  const element = document.createElement('div');
  element.className = `toast ${kind}`.trim();
  element.textContent = message;
  $('#toasts').appendChild(element);
  setTimeout(() => element.remove(), 4200);
}

async function copy(value) {
  try {
    await navigator.clipboard.writeText(value);
    toast('Copied', 'ok');
  } catch {
    toast('Clipboard is not available', 'error');
  }
}

/* ------------------------------------------------------------------ */
/* Actions                                                             */
/* ------------------------------------------------------------------ */

function parseLabels(text) {
  const labels = {};
  for (const line of text.split('\n')) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const index = trimmed.indexOf('=');
    if (index <= 0) throw new Error(`Invalid label line: ${trimmed}`);
    labels[trimmed.slice(0, index).trim()] = trimmed.slice(index + 1).trim();
  }
  return labels;
}

function tokenBlock(token, expiresAt, environment) {
  return `
    <div class="form">
      ${environment ? `<p class="muted" style="margin:0">Environment: <b>${esc(environment)}</b></p>` : ''}
      <p>This token is shown once. Pass it to the data plane as <code>-cp-token</code>.</p>
      <div class="copy-row">
        <span class="mono">${esc(token)}</span>
        <button class="btn small" data-action="copy" data-value="${esc(token)}" type="button">Copy</button>
      </div>
      ${expiresAt ? `<p class="muted">Expires ${esc(fmtTime(expiresAt))}</p>` : ''}
    </div>`;
}

/** Shows the free-form name input when "New environment…" is selected. */
function bindNewEnvironmentField(select, input) {
  select.addEventListener('change', () => {
    const isNew = select.value === '__new';
    input.hidden = !isNew;
    if (isNew) input.focus();
  });
}

/** Resolves a select value, creating a new environment when requested. */
async function resolveEnvironment(select, input) {
  if (select.value !== '__new') return select.value;
  const name = input.value.trim();
  if (!name) throw new Error('Environment name is required');
  const created = await api('/api/v1/environments', { method: 'POST', body: { name } });
  await loadEnvironments();
  return created.name;
}

const ACTIONS = {
  async 'backup-download'() {
    const response = await fetch('/api/v1/backup', {
      method: 'POST',
      headers: authHeaders(false),
    });

    if (response.status === 401) {
      showUnauthorized();
      throw new Error('Unauthorized: set the admin token');
    }

    if (!response.ok) {
      const text = (await response.text()).trim();
      throw new Error(text || `HTTP ${response.status}`);
    }

    const blob = await response.blob();
    const disposition = response.headers.get('Content-Disposition') || '';
    const match = /filename="?([^";]+)"?/i.exec(disposition);
    const filename = match ? match[1] : 'wasmgate-control.db';
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = filename;
    document.body.appendChild(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
    toast('Backup downloaded', 'ok');
  },

  async 'agent-new'() {
    const environments = await ensureEnvironments();
    showModal('New agent', `
      <div class="form">
        <div class="field"><label for="agent-name">Name</label>
          <input id="agent-name" placeholder="edge-1" autocomplete="off"></div>
        <div class="field"><label for="agent-env">Environment</label>
          <select id="agent-env">${environments.map((name) => `
            <option value="${esc(name)}" ${name === state.env ? 'selected' : ''}>${esc(name)}</option>`).join('')}
            <option value="__new">New environment…</option>
          </select>
          <input id="agent-env-new" placeholder="qa" autocomplete="off" spellcheck="false" hidden>
        </div>
        <div class="field"><label for="agent-labels">Labels (one key=value per line)</label>
          <textarea id="agent-labels" rows="4" placeholder="role=edge&#10;region=eu"></textarea></div>
      </div>`, [
      { label: 'Cancel', onClick: closeModal },
      {
        label: 'Create', kind: 'primary', onClick: async () => {
          const name = $('#agent-name').value.trim();
          if (!name) throw new Error('Name is required');
          const environment = await resolveEnvironment($('#agent-env'), $('#agent-env-new'));
          const result = await api('/api/v1/agents', {
            method: 'POST',
            body: { name, environment, labels: parseLabels($('#agent-labels').value) },
          });
          await loadEnvironments();
          if (environment !== state.env) setEnvironment(environment);
          showModal('Agent created', tokenBlock(result.enrollToken, null, environment), [
            { label: 'Close', kind: 'primary', onClick: () => { closeModal(); refresh(); } },
          ]);
        },
      },
    ]);

    bindNewEnvironmentField($('#agent-env'), $('#agent-env-new'));
  },

  async 'agent-details'(id) {
    const agents = await api('/api/v1/agents');
    const agent = agents.find((item) => item.id === id);
    if (!agent) throw new Error('Agent not found');
    const certificates = await api(`/api/v1/agents/${encodeURIComponent(id)}/certificates`);

    showModal(`Agent ${agent.name}`, `
      <div class="form">
        <div><span class="muted">Status</span> ${agentBadge(agent.status)}</div>
        <div><span class="muted">Environment</span>
          <div class="copy-row" style="margin-top:4px">
            ${badge(agent.environment, 'neutral')}
            <button class="btn small" data-action="agent-environment" data-id="${esc(agent.id)}" type="button">Change</button>
          </div>
        </div>
        <div><span class="muted">Id</span><div class="mono">${esc(agent.id)}</div></div>
        <div><span class="muted">Labels</span><div>${chips(agent.labels)}</div></div>
        <div><span class="muted">Active release</span><div class="mono">${esc(agent.activeReleaseId || '—')}</div></div>
        <div><span class="muted">Last seen</span><div>${esc(fmtTime(agent.lastSeenAt))}</div></div>
        <h3 style="margin:8px 0 0;font-size:13px">Certificates</h3>
        <table>
          <thead><tr><th>Serial</th><th>Expires</th><th></th></tr></thead>
          <tbody>${certificates.length === 0
            ? emptyRow(3, 'No active certificates')
            : certificates.map((certificate) => `
              <tr>
                <td class="mono">${esc(certificate.serialNumber)}</td>
                <td class="nowrap">${esc(fmtTime(certificate.notAfter))}</td>
                <td class="actions">
                  <button class="btn small danger" data-action="certificate-revoke"
                    data-id="${esc(agent.id)}" data-serial="${esc(certificate.serialNumber)}" type="button">Revoke</button>
                </td>
              </tr>`).join('')}
          </tbody>
        </table>
      </div>`, [
      { label: 'New token', onClick: async () => {
        const result = await api(`/api/v1/agents/${encodeURIComponent(id)}/tokens`, { method: 'POST' });
        showModal('Enrollment token', tokenBlock(result.token, result.expiresAt), [
          { label: 'Close', kind: 'primary', onClick: closeModal },
        ]);
      } },
      { label: 'Delete agent', kind: 'danger', onClick: async () => {
        if (!confirm(
          `Delete agent "${agent.name}"?\n\n` +
          'Its certificates and enrollment tokens will be removed and the live session will be closed. ' +
          'The proxy process on the host is not stopped: stop it separately.')) {
          return;
        }
        await api(`/api/v1/agents/${encodeURIComponent(id)}`, { method: 'DELETE' });
        toast('Agent deleted', 'ok');
        closeModal();
        refresh();
      } },
      { label: 'Close', kind: 'primary', onClick: closeModal },
    ]);
  },

  async 'agent-environment'(id) {
    const agents = await api('/api/v1/agents');
    const agent = agents.find((item) => item.id === id);
    if (!agent) throw new Error('Agent not found');
    const environments = await ensureEnvironments();

    showModal(`Environment of ${agent.name}`, `
      <div class="form">
        <div class="field"><label for="agent-move-env">Environment</label>
          <select id="agent-move-env">${environments.map((name) => `
            <option value="${esc(name)}" ${name === agent.environment ? 'selected' : ''}>${esc(name)}</option>`).join('')}
            <option value="__new">New environment…</option>
          </select>
          <input id="agent-move-env-new" placeholder="qa" autocomplete="off" spellcheck="false" hidden>
        </div>
        <p class="muted" style="margin:0">Deployments only reach agents of their own
          environment. The agent keeps its current release until the next deployment
          in the new environment.</p>
      </div>`, [
      { label: 'Cancel', onClick: closeModal },
      {
        label: 'Move', kind: 'primary', onClick: async () => {
          const environment = await resolveEnvironment($('#agent-move-env'), $('#agent-move-env-new'));
          if (environment !== agent.environment) {
            await api(`/api/v1/agents/${encodeURIComponent(id)}/environment`, {
              method: 'POST',
              body: { environment },
            });
            await loadEnvironments();
            toast(`Agent moved to ${environment}`, 'ok');
          }
          closeModal();
          if (environment !== state.env) setEnvironment(environment);
          else refresh();
        },
      },
    ]);

    bindNewEnvironmentField($('#agent-move-env'), $('#agent-move-env-new'));
  },

  async 'agent-token'(id) {
    const result = await api(`/api/v1/agents/${encodeURIComponent(id)}/tokens`, { method: 'POST' });
    showModal('Enrollment token', tokenBlock(result.token, result.expiresAt), [
      { label: 'Close', kind: 'primary', onClick: closeModal },
    ]);
  },

  async 'certificate-revoke'(id, element) {
    const serial = element.dataset.serial;
    if (!confirm(`Revoke certificate ${serial}? The agent will be rejected at its next session.`)) return;
    await api(`/api/v1/agents/${encodeURIComponent(id)}/certificates/${encodeURIComponent(serial)}/revoke`, {
      method: 'POST',
    });
    toast('Certificate revoked', 'ok');
    closeModal();
    refresh();
  },

  async 'artifact-upload'() {
    showModal('Upload artifact', `
      <div class="form">
        <div class="field"><label for="artifact-file">File</label>
          <input id="artifact-file" type="file"></div>
        <div class="row">
          <div class="field"><label for="artifact-kind">Kind</label>
            <select id="artifact-kind">
              <option value="1" selected>WASM plugin</option>
              <option value="2">Certificate</option>
              <option value="3">Key</option>
              <option value="4">Other</option>
            </select></div>
          <div class="field"><label for="artifact-media">Media type (optional)</label>
            <input id="artifact-media" placeholder="application/wasm"></div>
        </div>
        <p class="muted">The artifact is stored by the SHA-256 of its content and is shown once uploaded.</p>
      </div>`, [
      { label: 'Cancel', onClick: closeModal },
      {
        label: 'Upload', kind: 'primary', onClick: async () => {
          const file = $('#artifact-file').files[0];
          if (!file) throw new Error('Choose a file');
          if (file.size > 64 * 1024 * 1024) throw new Error('Artifact exceeds the 64 MiB limit');
          const buffer = await file.arrayBuffer();
          const result = await api('/api/v1/artifacts', {
            method: 'POST',
            body: {
              kind: Number($('#artifact-kind').value),
              mediaType: $('#artifact-media').value.trim() || null,
              contentBase64: toBase64(buffer),
            },
          });
          toast(`Uploaded ${fmtBytes(result.size)}`, 'ok');
          closeModal();
          refresh();
        },
      },
    ]);
  },

  async 'config-new'() {
    showModal(`New configuration in ${state.env}`, `
      <div class="form">
        <div class="field"><label for="config-yaml">YAML</label>
          <textarea id="config-yaml" rows="16" placeholder="version: 1&#10;listeners: []"></textarea></div>
        <div class="field"><label for="config-file">or load a file</label>
          <input id="config-file" type="file"></div>
        <div class="field"><label for="config-message">Message (optional)</label>
          <input id="config-message" placeholder="what changed"></div>
      </div>`, [
      { label: 'Cancel', onClick: closeModal },
      {
        label: 'Create', kind: 'primary', onClick: async () => {
          const yaml = $('#config-yaml').value;
          if (!yaml.trim()) throw new Error('Configuration body is required');
          const result = await api('/api/v1/configs', {
            method: 'POST',
            body: { environment: state.env, yaml, message: $('#config-message').value.trim() || null },
          });
          toast(`Configuration ${short(result.sha256, 12)} stored`, 'ok');
          closeModal();
          refresh();
        },
      },
    ]);

    $('#config-file').addEventListener('change', async (event) => {
      const file = event.target.files[0];
      if (file) $('#config-yaml').value = await file.text();
    });
  },

  async 'config-yaml'(id) {
    const configs = await api(`/api/v1/configs/${encodeURIComponent(state.env)}`);
    const config = configs.find((item) => item.id === id);
    if (!config) throw new Error('Configuration not found');
    showModal(`Configuration ${short(config.sha256, 14)}`, `
      <pre class="code">${esc(config.yaml)}</pre>`, [
      { label: 'Close', kind: 'primary', onClick: closeModal },
    ]);
  },

  async 'release-new'() {
    const [configs, artifacts] = await Promise.all([
      api(`/api/v1/configs/${encodeURIComponent(state.env)}`),
      api('/api/v1/artifacts'),
    ]);
    if (configs.length === 0) throw new Error('Create a configuration first');

    showModal('New release', `
      <div class="form">
        <div class="field"><label for="release-config">Configuration</label>
          <select id="release-config">${configs.map((config) => `
            <option value="${esc(config.id)}">${esc(short(config.sha256, 14))} — ${esc(config.message || 'no message')} (${esc(fmtTime(config.createdAt))})</option>`).join('')}
          </select></div>
        <div class="field"><label>Artifacts (install path inside the release)</label>
          <div id="release-artifacts" class="form"></div>
          <button class="btn small" id="release-add" type="button">Add artifact</button>
        </div>
      </div>`, [
      { label: 'Cancel', onClick: closeModal },
      {
        label: 'Create', kind: 'primary', onClick: async () => {
          const rows = Array.from(document.querySelectorAll('#release-artifacts .release-artifact'));
          const list = rows.map((row) => ({
            artifactId: row.querySelector('select').value,
            name: row.querySelector('input').value.trim(),
          })).filter((item) => item.artifactId);
          for (const item of list) {
            if (!item.name) throw new Error('Every selected artifact needs an install path');
          }
          const result = await api('/api/v1/releases', {
            method: 'POST',
            body: { environment: state.env, configVersionId: $('#release-config').value, artifacts: list },
          });
          toast(`Release ${short(result.id, 12)} created`, 'ok');
          closeModal();
          refresh();
        },
      },
    ]);

    const container = $('#release-artifacts');
    const addRow = () => {
      if (artifacts.length === 0) {
        container.innerHTML = '<p class="muted">Upload an artifact first to include files.</p>';
        return;
      }
      const row = document.createElement('div');
      row.className = 'row release-artifact';
      row.innerHTML = `
        <select>${artifacts.map((artifact) => `
          <option value="${esc(artifact.id)}">${esc(ARTIFACT_KINDS[artifact.kind] ?? artifact.kind)} — ${esc(short(artifact.id, 12))} — ${esc(fmtBytes(artifact.size))}</option>`).join('')}
        </select>
        <input placeholder="plugins/ipguard.wasm">
        <button class="btn small" type="button">Remove</button>`;
      row.querySelector('button').onclick = () => row.remove();
      container.appendChild(row);
    };

    $('#release-add').addEventListener('click', addRow);
    addRow();
  },

  async 'release-revoke'(id) {
    if (!confirm('Revoke this release? It can no longer be deployed.')) return;
    await api(`/api/v1/releases/${encodeURIComponent(id)}/revoke`, { method: 'POST' });
    toast('Release revoked', 'ok');
    refresh();
  },

  async 'deployment-new'() {
    const [releases, allAgents] = await Promise.all([
      api(`/api/v1/releases/${encodeURIComponent(state.env)}`),
      api('/api/v1/agents'),
    ]);
    const deployable = releases.filter((release) => release.status !== 3);
    if (deployable.length === 0) throw new Error('Create a release first');

    const envAgents = allAgents.filter((agent) => agent.environment === state.env);
    const online = envAgents.filter((agent) => agent.status === 1).length;
    const targetNote = envAgents.length === 0
      ? `No agents belong to ${state.env}: the deployment will be recorded as Failed.`
      : `${online} of ${envAgents.length} agent(s) in ${state.env} are online.`;

    showModal('New deployment', `
      <div class="form">
        <div class="field"><label for="deployment-release">Release</label>
          <select id="deployment-release">${deployable.map((release) => `
            <option value="${esc(release.id)}">${esc(short(release.id, 14))} — ${esc(short(release.configSha256, 10))} (${esc(fmtTime(release.createdAt))})</option>`).join('')}
          </select></div>
        <div class="field"><label for="deployment-waves">Wave selectors, one per line</label>
          <textarea id="deployment-waves" rows="4">*</textarea>
          <span class="muted">Use <code>*</code> or a JSON object such as <code>{"role":"edge"}</code>.</span></div>
        <p class="muted" style="margin:0">${esc(targetNote)}</p>
      </div>`, [
      { label: 'Cancel', onClick: closeModal },
      {
        label: 'Deploy', kind: 'primary', onClick: async () => {
          const lines = $('#deployment-waves').value.split('\n').map((line) => line.trim()).filter(Boolean);
          for (const line of lines) {
            if (line !== '*' && !/^\{.*\}$/s.test(line)) throw new Error(`Selector must be * or a JSON object: ${line}`);
            if (line !== '*') JSON.parse(line);
          }
          const result = await api('/api/v1/deployments', {
            method: 'POST',
            body: {
              environment: state.env,
              releaseId: $('#deployment-release').value,
              waveSelectors: lines,
            },
          });
          toast(`Deployment ${short(result.id, 12)} ${DEPLOY_STATUS[result.status] ?? result.status}`, 'ok');
          closeModal();
          showView('deployments');
        },
      },
    ]);
  },

  async 'deployment-details'(id) {
    const [deployments, audit] = await Promise.all([
      api(`/api/v1/deployments/${encodeURIComponent(state.env)}`),
      api('/api/v1/audit'),
    ]);
    const item = deployments.find((deployment) => deployment.id === id);
    if (!item) throw new Error('Deployment not found');
    const events = audit.filter((event) => event.target === id);

    showModal(`Deployment ${short(item.id, 16)}`, `
      <div class="form">
        <div><span class="muted">Environment</span> ${badge(item.environment, 'neutral')}</div>
        <div><span class="muted">Status</span> ${deployBadge(item.status)}</div>
        <div><span class="muted">Release</span><div class="mono">${esc(item.releaseId)}</div></div>
        <div><span class="muted">Created</span><div>${esc(fmtTime(item.createdAt))}</div></div>
        <div><span class="muted">Finished</span><div>${esc(fmtTime(item.completedAt))}</div></div>
        ${item.message
          ? `<div><span class="muted">Message</span><div class="error-box">${esc(item.message)}</div></div>`
          : item.status === 3
            ? '<p class="muted">No failure message was recorded for this deployment. It predates deployment messages.</p>'
            : ''}
        <div><span class="muted">Rollback target</span>
          <div class="mono">${item.rollbackReleaseId ? esc(item.rollbackReleaseId) : '—'}</div></div>

        <h3 class="section-title">Waves</h3>
        <table>
          <thead><tr><th>#</th><th>Selector</th><th>Status</th><th>Started</th><th>Finished</th></tr></thead>
          <tbody>${item.waves.length === 0
            ? emptyRow(5, 'No waves recorded')
            : item.waves.map((wave) => `
              <tr>
                <td>#${wave.index}</td>
                <td class="mono">${esc(wave.selector)}</td>
                <td>${waveBadge(wave.status)}</td>
                <td class="nowrap muted">${esc(fmtTime(wave.startedAt))}</td>
                <td class="nowrap muted">${esc(fmtTime(wave.completedAt))}</td>
              </tr>`).join('')}
          </tbody>
        </table>

        <h3 class="section-title">Related audit</h3>
        <table>
          <thead><tr><th>At</th><th>Action</th><th>Actor</th></tr></thead>
          <tbody>${events.length === 0
            ? emptyRow(3, 'No audit events for this deployment in the last 100')
            : events.map((event) => `
              <tr>
                <td class="nowrap muted">${esc(fmtTime(event.at))}</td>
                <td>${esc(event.action)}</td>
                <td class="mono">${esc(event.actor)}</td>
              </tr>`).join('')}
          </tbody>
        </table>
      </div>`, [
      { label: 'Close', kind: 'primary', onClick: closeModal },
    ]);
  },

  async 'deployment-rollback'(id) {
    const deployments = await api(`/api/v1/deployments/${encodeURIComponent(state.env)}`);
    const deployment = deployments.find((item) => item.id === id);
    const target = deployment?.rollbackReleaseId;
    if (!target) throw new Error('No older release exists in this environment');
    if (!confirm(`Roll this deployment back to release ${short(target, 14)}?`)) return;
    await api(`/api/v1/deployments/${encodeURIComponent(id)}/rollback`, { method: 'POST' });
    toast('Rollback started', 'ok');
    refresh();
  },

  'copy'(id, element) {
    return copy(element.dataset.value);
  },
};

function toBase64(buffer) {
  const bytes = new Uint8Array(buffer);
  let binary = '';
  const chunk = 0x8000;
  for (let index = 0; index < bytes.length; index += chunk) {
    binary += String.fromCharCode.apply(null, bytes.subarray(index, index + chunk));
  }
  return btoa(binary);
}
