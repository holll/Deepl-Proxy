'use strict';

/* ============================================================
   Deepl Proxy 管理台
   约定：所有网络错误都有可见反馈；所有异步按钮都有提交锁；
   行内编辑用原生 input，Enter 提交、Esc 还原、未修改不发请求。
   ============================================================ */

const API = {
  session: '/admin/session',
  login: '/admin/login',
  logout: '/admin/logout',
  keys: '/admin/keys',
  usage: '/admin/usage-refresh',
  cache: '/admin/cache',
  setup: '/admin/setup',
  settings: '/admin/settings',
  webhookTest: '/admin/webhook/test',
  rotateSecret: '/admin/settings/rotate-secret'
};

class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.name = 'ApiError';
    this.status = status || 0;
  }
}

const byId = (id) => document.getElementById(id);

const dom = {
  login: byId('login'),
  loginForm: byId('loginForm'),
  loginBtn: byId('loginBtn'),
  token: byId('tokenInput'),
  tokenField: byId('tokenField'),
  loginError: byId('loginError'),

  setupForm: byId('setupForm'),
  setupAdmin: byId('setupAdmin'),
  setupGateway: byId('setupGateway'),
  setupError: byId('setupError'),
  setupBtn: byId('setupBtn'),

  app: byId('app'),
  status: byId('statusBar'),
  alert: byId('alertBar'),
  main: byId('main'),

  addForm: byId('addForm'),
  addBtn: byId('addBtn'),
  addError: byId('addError'),
  fName: byId('fName'),
  fAuth: byId('fAuth'),
  fEndpoint: byId('fEndpoint'),
  fEndpointSelect: byId('fEndpointSelect'),
  fProvider: byId('fProvider'),
  fQuota: byId('fQuota'),
  nameField: byId('nameField'),
  endpointField: byId('endpointField'),

  reload: byId('reloadBtn'),
  refresh: byId('refreshUsageBtn'),
  logout: byId('logoutBtn'),

  tb: byId('tb'),
  keysCount: byId('keysCount'),
  keyTestToast: byId('keyTestToast'),
  keyTestToastMessage: byId('keyTestToastMessage'),
  keyTestToastClose: byId('keyTestToastClose'),
  keyTestAnnouncement: byId('keyTestAnnouncement'),
  keyDialog: byId('keyDialog'),
  keyDialogTitle: byId('keyDialogTitle'),
  keyDialogStatus: byId('keyDialogStatus'),
  keyValue: byId('keyValue'),
  copyKey: byId('copyKeyBtn'),

  cacheTb: byId('cacheTb'),
  cacheCount: byId('cacheCount'),
  cacheDetails: byId('cacheDetails'),
  cacheLatest: byId('cacheLatest'),
  pageInfo: byId('pageInfo'),
  pageSize: byId('pageSize'),
  cachePrev: byId('cachePrev'),
  cacheNext: byId('cacheNext'),

  settingsBtn: byId('settingsBtn'),
  settingsDialog: byId('settingsDialog'),
  settingsForm: byId('settingsForm'),
  settingsSave: byId('settingsSave'),
  settingsCancel: byId('settingsCancel'),
  settingsError: byId('settingsError'),
  settingsClose: byId('settingsClose'),
  rotateSecretBtn: byId('rotateSecretBtn'),
  webhookTestBtn: byId('webhookTestBtn'),
  webhookTestResult: byId('webhookTestResult'),

  sAdminToken: byId('sAdminToken'),
  sGatewayToken: byId('sGatewayToken'),
  sSessionTTL: byId('sSessionTTL'),
  wURLField: byId('wURLField'),
  sTTLDays: byId('sTTLDays'),
  sCleanupInterval: byId('sCleanupInterval'),
  sCleanupBatch: byId('sCleanupBatch'),
  sCleanupRounds: byId('sCleanupRounds'),
  sLegacyOff: byId('sLegacyOff'),

  wEnabled: byId('wEnabled'),
  wFormat: byId('wFormat'),
  wURL: byId('wURL'),
  wNickname: byId('wNickname'),
  wNicknameField: byId('wNicknameField'),
  wTitlePrefix: byId('wTitlePrefix'),
  wTimeout: byId('wTimeout'),
  wMinInterval: byId('wMinInterval'),
  wNotifyKeyDisabled: byId('wNotifyKeyDisabled'),
  wNotifyTemporary: byId('wNotifyTemporary'),
  wNotifyNoKeys: byId('wNotifyNoKeys'),

  confirm: byId('confirmDialog'),
  confirmTitle: byId('confirmTitle'),
  confirmBody: byId('confirmBody'),
  confirmOk: byId('confirmOk'),
  confirmCancel: byId('confirmCancel')
};

const state = {
  gen: 0,
  settingsDirty: false,
  settingsBaseline: null,
  keys: [],
  page: { limit: Number(dom.pageSize.value) || 30, offset: 0, total: 0 }
};

const dateTimeFormat = new Intl.DateTimeFormat('zh-CN', {
  year: 'numeric', month: '2-digit', day: '2-digit',
  hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false
});
const numberFormat = new Intl.NumberFormat('zh-CN');

/* ---------------- 提示 ------------- */

function clearStatus() {
  dom.status.textContent = '';
  dom.status.classList.remove('is-ok');
}

function clearAlert() {
  dom.alert.textContent = '';
  dom.alert.hidden = true;
}

/** 成功/进度走 polite live region，错误走 assertive。 */
function notify(message, kind) {
  if (kind === 'error') {
    dom.status.textContent = '';
    dom.alert.textContent = message;
    dom.alert.hidden = false;
    return;
  }
  clearAlert();
  dom.status.textContent = message;
  dom.status.classList.toggle('is-ok', kind === 'ok');
}

function setBusy(button, busy, label) {
  if (!button) return;
  button.disabled = busy;
  if (busy) button.setAttribute('aria-busy', 'true');
  else button.removeAttribute('aria-busy');
  if (busy && label) {
    button.dataset.idleLabel = button.textContent;
    button.textContent = label;
  } else if (!busy && button.dataset.idleLabel) {
    button.textContent = button.dataset.idleLabel;
    delete button.dataset.idleLabel;
  }
}

async function withBusy(button, work, failureLabel) {
  setBusy(button, true);
  try {
    await work();
  } catch (err) {
    notify(`${failureLabel}：${err.message}`, 'error');
  } finally {
    setBusy(button, false);
  }
}

/* ---------------- API ---------------- */

async function api(path, options) {
  const opts = options || {};
  const method = opts.method || 'GET';
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), opts.timeout || 15000);

  let response;
  try {
    response = await fetch(path, {
      method,
      credentials: 'same-origin',
      cache: 'no-store',
      headers: opts.body
        ? { 'Content-Type': 'application/json', Accept: 'application/json' }
        : { Accept: 'application/json' },
      body: opts.body,
      signal: controller.signal
    });
  } catch (err) {
    if (err && err.name === 'AbortError') {
      throw new ApiError('请求超时，请检查服务状态后重试');
    }
    throw new ApiError('网络请求失败，请确认服务仍在运行');
  } finally {
    clearTimeout(timer);
  }

  const text = await response.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch (err) {
      data = { raw: text };
    }
  }

  if (response.status === 401) {
    const message = path === API.login ? '令牌不正确，请重试' : '登录已失效，请重新验证';
    if (path !== API.login) showLogin(message);
    throw new ApiError(message, 401);
  }

  if (!response.ok) {
    const detail = data && (data.error || data.message);
    throw new ApiError(detail || `请求失败（HTTP ${response.status}）`, response.status);
  }

  return data;
}

/* ---------------- 视图切换 ---------------- */

function showLogin(message) {
  state.gen += 1;
  if (dom.confirm.open) dom.confirm.close('cancel');
  if (dom.settingsDialog.open) dom.settingsDialog.close('cancel');
  if (dom.keyDialog.open) dom.keyDialog.close();
  clearKeyDialog();
  dismissKeyTestToast();
  dom.app.hidden = true;
  dom.login.hidden = false;
  dom.loginForm.hidden = false;
  dom.setupForm.hidden = true;
  clearStatus();
  clearAlert();
  dom.loginError.textContent = message || '';
  dom.token.value = '';
  requestAnimationFrame(() => dom.token.focus());
}

/** 首次启动：数据库里还没有管理员令牌，先让用户设置。 */
function showSetup(message) {
  state.gen += 1;
  dom.app.hidden = true;
  dom.login.hidden = false;
  dom.loginForm.hidden = true;
  dom.setupForm.hidden = false;
  clearStatus();
  clearAlert();
  dom.setupError.textContent = message || '';
  requestAnimationFrame(() => dom.setupAdmin.focus());
}

function showApp() {
  state.gen += 1;
  dom.login.hidden = true;
  dom.token.value = '';
  dom.loginError.textContent = '';
  dom.token.removeAttribute('aria-invalid');
  dom.tokenField.classList.remove('is-invalid');
  dom.app.hidden = false;
}

/* ---------------- 格式化 ---------------- */

function esc(value) {
  return String(value == null ? '' : value)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function fmtTs(ms) {
  if (!ms) return '—';
  return dateTimeFormat.format(new Date(ms));
}

function fmtSize(bytes) {
  const b = Number(bytes) || 0;
  if (b < 1024) return `${b} B`;
  if (b < 1048576) return `${(b / 1024).toFixed(1)} KB`;
  return `${(b / 1048576).toFixed(1)} MB`;
}

function countdownText(until) {
  const left = Number(until) - Date.now();
  if (!Number.isFinite(left) || left <= 0) return '已到期';
  const seconds = Math.floor(left / 1000);
  if (seconds < 60) return `${seconds} 秒`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 时`;
  return `${Math.floor(seconds / 86400)} 天`;
}

/* ---------------- Key 列表 ---------------- */

function statusBadge(status) {
  if (status === 'active') return '<span class="badge badge-ok">可用</span>';
  if (status === 'dead') return '<span class="badge badge-danger">已熔断</span>';
  return '<span class="badge badge-neutral">已停用</span>';
}

function disableReason(key) {
  if (key.status === 'active' || !key.disable_type) return '';
  if (key.disable_type === 'temporary') return '<span class="cell-note">临时故障</span>';
  if (key.disable_type === 'quota') return '<span class="cell-note">配额耗尽</span>';
  if (key.disable_type === 'permanent') return '<span class="cell-note">Key 失效</span>';
  if (key.disable_type === 'manual') return '<span class="cell-note">手动停用</span>';
  return '';
}

function siteBadge(key) {
  if (key.provider === 'deeplx') return '<span class="badge badge-info">deeplx</span>';
  switch (key.site_type) {
    case 'deepl_pro': return '<span class="badge badge-info">deepl-pro</span>';
    case 'deepl_free': return '<span class="badge badge-ok">deepl free</span>';
    case 'deepl_official': return '<span class="badge badge-neutral">官方</span>';
    default: return '<span class="badge badge-neutral">自定义</span>';
  }
}

function usageCell(key) {
  const count = key.character_count;
  const limit = key.character_limit;
  const hasCount = Number.isFinite(count) && count >= 0;
  const hasLimit = Number.isFinite(limit) && limit >= 0;
  if (!hasCount && !hasLimit) return '<span class="cell-note">未获取</span>';

  const used = hasCount ? numberFormat.format(count) : '未知';
  const total = !hasLimit ? '未知' : limit === 0 ? '不限' : numberFormat.format(limit);
  const hasQuota = hasCount && hasLimit && limit > 0;
  const pct = hasQuota ? Math.min(100, Math.round((count / limit) * 100)) : null;
  const level = pct >= 95 ? ' is-danger' : pct >= 80 ? ' is-warn' : '';
  const remaining = hasLimit && limit === 0 ? '不限' : hasQuota
    ? `${numberFormat.format(Math.max(0, limit - count))} 字符` : '未知';
  const amount = hasQuota ? `${pct}%(${used}/${total})` : `已用 ${used} / ${total} 字符`;
  return `<div class="usage">
      <span class="usage-remaining">剩余 ${remaining}</span>
      ${hasQuota ? `<span class="usage-bar${level}" aria-hidden="true" data-pct="${pct}"><i></i></span>` : ''}
      <span class="usage-amount">${amount}</span>
    </div>`;
}

function keyRow(key) {
  const until = key.disabled_until && key.status !== 'active'
    ? `<span class="countdown" data-until="${Number(key.disabled_until)}">${countdownText(key.disabled_until)}</span>`
    : '—';
  const toggleTo = (key.status === 'active' || key.status === 'dead') ? 'disabled' : 'active';
  const toggleLabel = toggleTo === 'disabled' ? '禁用' : '启用';
  const name = key.name || '';
  const endpoint = key.endpoint || '';
  const errorTitle = key.last_error_message ? ` title="${esc(key.last_error_message)}"` : '';

  return `<tr data-id="${key.id}">
    <td class="cell-id" data-label="ID">#${key.id}</td>
    <td data-label="名称"><input class="cell-input" data-id="${key.id}" data-field="name" value="${esc(name)}" title="${esc(name)}" aria-label="名称" spellcheck="false" autocomplete="off" /></td>
    <td data-label="Endpoint"><input class="cell-input" data-id="${key.id}" data-field="endpoint" value="${esc(endpoint)}" title="${esc(endpoint)}" aria-label="Endpoint" spellcheck="false" autocomplete="off"${errorTitle} /></td>
    <td data-label="站点">${siteBadge(key)}</td>
    <td data-label="状态"><span class="status-wrap">${statusBadge(key.status)}${disableReason(key)}</span></td>
    <td data-label="用量">${usageCell(key)}</td>
    <td data-label="截至">${until}</td>
    <td class="cell-actions" data-label="操作">
      <button class="btn btn-sm" type="button" aria-haspopup="dialog" data-action="view-key" data-id="${key.id}" data-name="${esc(name)}">查看 Key</button>
      <button class="btn btn-sm" type="button" data-action="test" data-id="${key.id}" data-name="${esc(name)}">测试</button>
      <button class="btn btn-sm" type="button" data-action="toggle" data-id="${key.id}" data-to="${toggleTo}" data-name="${esc(name)}">${toggleLabel}</button>
      <button class="btn btn-sm btn-quiet-danger" type="button" data-action="delete" data-id="${key.id}" data-name="${esc(name)}">删除</button>
    </td>
  </tr>`;
}

/** CSP 禁止内联 style 属性，进度宽度在渲染后通过 CSSOM 设置。 */
function applyUsageBars(root) {
  root.querySelectorAll('.usage-bar[data-pct]').forEach((bar) => {
    const pct = Number(bar.getAttribute('data-pct')) || 0;
    const fill = bar.firstElementChild;
    if (fill) fill.style.width = `${pct}%`;
  });
}

function keyStatusRank(key) {
  if (key.disable_type === 'quota') return 2;
  return key.status === 'active' ? 0 : 1;
}

function renderKeys() {
  dom.keysCount.textContent = state.keys.length ? `${numberFormat.format(state.keys.length)} 个 Key` : '';
  dom.tb.innerHTML = state.keys.length
    ? [...state.keys].sort((a, b) => keyStatusRank(a) - keyStatusRank(b)).map(keyRow).join('')
    : '<tr class="empty-row"><td colspan="8">还没有 Key。填写 Name 与 Auth Key 后点“新增”。</td></tr>';
  applyUsageBars(dom.tb);
}

async function loadKeys(options) {
  const silent = Boolean(options && options.silent);
  const gen = state.gen;
  if (!silent) dom.keysCount.textContent = '加载中…';
  try {
    const data = await api(API.keys);
    if (gen !== state.gen) return false;
    state.keys = Array.isArray(data && data.keys) ? data.keys : [];
    renderKeys();
    return true;
  } catch (err) {
    if (gen !== state.gen) return false;
    notify(`Key 列表加载失败：${err.message}`, 'error');
    return false;
  }
}

const isCellInput = (node) => node instanceof HTMLElement && node.classList.contains('cell-input');

/* ---------------- 缓存列表 ---------------- */

function cacheRow(entry) {
  const key = entry.cache_key || '';
  const badge = entry.expired
    ? '<span class="badge badge-danger">已过期</span>'
    : '<span class="badge badge-ok">有效</span>';

  return `<tr data-key="${esc(key)}">
    <td class="cell-mono" data-label="时间">${fmtTs(entry.created_at)}</td>
    <td class="cell-mono" data-label="译文预览"><span class="truncate truncate-wide" title="${esc(entry.preview || '')}">${esc(entry.preview || '—')}</span></td>
    <td data-label="状态">${badge}</td>
    <td class="cell-actions" data-label="操作">
      <button class="btn btn-sm btn-quiet-danger" type="button" data-action="delete-cache" data-key="${esc(key)}" data-preview="${esc((entry.preview || key).slice(0, 36))}">删除</button>
    </td>
  </tr>`;
}

/** “3 分钟前”这类相对时间，只用于回答“服务还在跑吗”。 */
function fmtAgo(ms) {
  if (!ms) return '还没有翻译记录';
  const diff = Date.now() - Number(ms);
  if (!Number.isFinite(diff)) return '还没有翻译记录';
  if (diff < 0) return '刚刚';
  const minutes = Math.floor(diff / 60000);
  if (minutes < 1) return '刚刚';
  if (minutes < 60) return `${minutes} 分钟前`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时前`;
  return `${Math.floor(hours / 24)} 天前`;
}

function renderLatest(entry, total) {
  if (!total) {
    dom.cacheLatest.textContent = '还没有翻译记录';
    return;
  }
  const when = entry ? fmtAgo(entry.created_at) : '';
  dom.cacheLatest.textContent = when ? `最新记录：${when}` : `共 ${numberFormat.format(total)} 条`;
}

function renderPager() {
  const { limit, offset, total } = state.page;
  const pages = Math.max(1, Math.ceil(total / limit));
  const current = Math.min(pages, Math.floor(offset / limit) + 1);
  dom.pageInfo.textContent = `第 ${current} / ${pages} 页`;
  dom.cachePrev.disabled = offset <= 0;
  dom.cacheNext.disabled = offset + limit >= total;
}

function renderCache(entries, total) {
  state.page.total = total;
  dom.cacheCount.textContent = total ? `共 ${numberFormat.format(total)} 条` : '';
  if (entries.length) renderLatest(entries[0], total);
  dom.cacheTb.innerHTML = entries.length
    ? entries.map(cacheRow).join('')
    : '<tr class="empty-row"><td colspan="4">还没有翻译记录</td></tr>';
  renderPager();
}

async function loadCache(options) {
  const silent = Boolean(options && options.silent);
  const gen = state.gen;
  const { limit, offset } = state.page;
  if (!silent) dom.cacheCount.textContent = '加载中…';
  try {
    const data = await api(`${API.cache}?limit=${limit}&offset=${offset}`);
    if (gen !== state.gen) return false;
    const total = Number(data && data.total) || 0;
    // 删除后 offset 可能越界：回夹到最后一个有效页，避免空白页
    if (total > 0 && offset >= total) {
      state.page.offset = Math.floor((total - 1) / limit) * limit;
      return loadCache({ silent });
    }
    if (total === 0) state.page.offset = 0;
    renderCache(Array.isArray(data && data.entries) ? data.entries : [], total);
    return true;
  } catch (err) {
    if (gen !== state.gen) return false;
    notify(`缓存加载失败：${err.message}`, 'error');
    return false;
  }
}

/** 记录列表默认收起：只拉 1 条用于“最近有没有新记录”。 */
async function loadLatest() {
  try {
    const data = await api(`${API.cache}?limit=1&offset=0`);
    const total = Number(data && data.total) || 0;
    const entries = Array.isArray(data && data.entries) ? data.entries : [];
    renderLatest(entries[0], total);
    dom.cacheCount.textContent = total ? `共 ${numberFormat.format(total)} 条` : '';
  } catch (err) {
    dom.cacheLatest.textContent = '无法读取翻译记录';
  }
}

async function refreshAll() {
  const jobs = [loadKeys({ silent: true }), loadLatest()];
  if (dom.cacheDetails.open) jobs.push(loadCache({ silent: true }));
  const results = await Promise.all(jobs);
  if (results.every(Boolean)) notify('已刷新', 'ok');
}

/* ---------------- 确认对话框 ---------------- */

function confirmAction(options) {
  return new Promise((resolve) => {
    const dialog = dom.confirm;
    if (typeof dialog.showModal !== 'function') {
      resolve(window.confirm(`${options.title}\n\n${options.body}`));
      return;
    }
    dom.confirmTitle.textContent = options.title;
    dom.confirmBody.textContent = options.body;
    dom.confirmOk.textContent = options.confirmText || '确认';
    dom.confirmOk.classList.toggle('btn-danger', options.danger !== false);
    dom.confirmOk.classList.toggle('btn-primary', options.danger === false);

    const onClose = () => {
      dialog.removeEventListener('close', onClose);
      resolve(dialog.returnValue === 'ok');
    };
    dialog.addEventListener('close', onClose);
    dialog.returnValue = '';
    dialog.showModal();
    dom.confirmCancel.focus();
  });
}

/* ---------------- Key 行内操作 ---------------- */

async function deleteKey(button) {
  const id = button.dataset.id;
  const name = button.dataset.name || `#${id}`;
  const ok = await confirmAction({
    title: '删除 Key',
    body: `确定删除「${name}」？该操作不可撤销。`,
    confirmText: '删除'
  });
  if (!ok) return;

  await withBusy(button, async () => {
    await api(`${API.keys}/${encodeURIComponent(id)}`, { method: 'DELETE' });
    notify(`已删除「${name}」`, 'ok');
    await loadKeys({ silent: true });
  }, '删除失败');
}

async function toggleKey(button) {
  const id = button.dataset.id;
  const name = button.dataset.name || `#${id}`;
  const target = button.dataset.to;
  const label = target === 'disabled' ? '禁用' : '启用';

  await withBusy(button, async () => {
    await api(`${API.keys}/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: JSON.stringify({ status: target })
    });
    notify(`已${label}「${name}」`, 'ok');
    await loadKeys({ silent: true });
  }, `${label}失败`);
}

let keyTestToastSource;

function showKeyTestToast(message, kind, source) {
  const toast = dom.keyTestToast;
  keyTestToastSource = source;
  toast.dataset.kind = kind;
  dom.keyTestToastMessage.textContent = message;
  if (typeof toast.showPopover !== 'function') toast.removeAttribute('popover');
  toast.hidden = false;
  if (toast.hasAttribute('popover') && !toast.matches(':popover-open')) toast.showPopover();
  const announcement = dom.keyTestAnnouncement;
  announcement.textContent = announcement.textContent === message ? `${message}\u00a0` : message;
}

function dismissKeyTestToast() {
  const toast = dom.keyTestToast;
  if (typeof toast.hidePopover === 'function' && toast.matches(':popover-open')) toast.hidePopover();
  toast.hidden = true;
  dom.keyTestToastMessage.textContent = '';
  dom.keyTestAnnouncement.textContent = '';
  keyTestToastSource = null;
}

dom.keyTestToastClose.addEventListener('click', () => {
  const source = keyTestToastSource;
  const restoreFocus = document.activeElement === dom.keyTestToastClose;
  dismissKeyTestToast();
  if (restoreFocus) (source?.isConnected && !source.disabled ? source : dom.main).focus();
});

async function testKey(button) {
  const id = button.dataset.id;
  const name = button.dataset.name || `#${id}`;
  const gen = state.gen;
  setBusy(button, true, '测试中…');
  try {
    const data = await api(`${API.keys}/${encodeURIComponent(id)}/test`, { method: 'POST' });
    if (gen !== state.gen) return;
    if (data?.ok) {
      const site = data.site_type || data.provider;
      showKeyTestToast(`「${name}」连通正常（${site ? `${site}，` : ''}${data.latency_ms} ms）`, 'ok', button);
    } else {
      const detail = data?.message || data?.error_type || '未知错误';
      showKeyTestToast(`「${name}」测试失败：${detail}`, 'error', button);
    }
  } catch (err) {
    if (gen === state.gen) showKeyTestToast(`「${name}」测试失败：${err.message}`, 'error', button);
  } finally {
    setBusy(button, false);
  }
}

let keyView;
let copyFeedbackTimer;

function clearKeyDialog() {
  keyView = null;
  clearTimeout(copyFeedbackTimer);
  dom.keyValue.value = '';
  dom.keyValue.hidden = true;
  dom.keyDialogStatus.textContent = '';
  dom.copyKey.disabled = true;
  dom.copyKey.removeAttribute('data-copied');
}

async function viewKey(button) {
  const dialog = dom.keyDialog;
  if (dialog.open) return;
  if (typeof dialog.showModal !== 'function') {
    notify('当前浏览器不支持查看 Key 的弹窗，请升级浏览器。', 'error');
    return;
  }
  clearKeyDialog();
  const view = { id: button.dataset.id };
  keyView = view;
  const name = button.dataset.name;
  dom.keyDialogTitle.textContent = name ? `查看 Key · ${name}` : `查看 Key #${view.id}`;
  dom.keyDialogStatus.textContent = '正在读取 Key…';
  dialog.showModal();
  try {
    const data = await api(`${API.keys}/${encodeURIComponent(view.id)}/secret`);
    if (!dialog.open || keyView !== view) return;
    if (typeof data?.auth_key !== 'string' || !data.auth_key) {
      dom.keyDialogStatus.textContent = '此条目未配置 Key。';
      return;
    }
    dom.keyValue.value = data.auth_key;
    dom.keyValue.hidden = false;
    dom.copyKey.disabled = false;
    dom.keyDialogStatus.textContent = '';
  } catch (err) {
    if (dialog.open && keyView === view) dom.keyDialogStatus.textContent = `读取失败：${err.message}`;
  }
}

dom.keyDialog.addEventListener('close', () => {
  if (!dom.keyDialog.open) clearKeyDialog();
});

function copySelectedKey() {
  const focused = document.activeElement;
  dom.keyValue.select();
  if (document.execCommand('copy')) {
    dom.keyValue.setSelectionRange(0, 0);
    focused?.focus();
    return;
  }
  throw new Error('Clipboard unavailable');
}

async function copyKey() {
  if (!dom.keyDialog.open || !dom.keyValue.value) return;
  const view = keyView;
  let copied = false;
  try {
    await navigator.clipboard.writeText(dom.keyValue.value);
    copied = true;
  } catch {
    if (!dom.keyDialog.open || keyView !== view) return;
    try {
      copySelectedKey();
      copied = true;
    } catch {
      // 保留选中状态，方便用户手动复制。
    }
  }
  if (!dom.keyDialog.open || keyView !== view) return;
  clearTimeout(copyFeedbackTimer);
  dom.copyKey.removeAttribute('data-copied');
  if (copied) {
    // 若用户连续复制，同一按钮也重新播放成功动效。
    void dom.copyKey.offsetWidth;
    dom.copyKey.setAttribute('data-copied', '');
    copyFeedbackTimer = setTimeout(() => {
      dom.copyKey.removeAttribute('data-copied');
      dom.keyDialogStatus.textContent = '';
    }, 2400);
  }
  const message = copied ? '复制成功' : '无法自动复制，请手动复制已选中的 Key。';
  dom.keyDialogStatus.textContent = dom.keyDialogStatus.textContent === message ? `${message}\u00a0` : message;
}

if ('CommandEvent' in window) {
  dom.keyValue.addEventListener('command', (event) => {
    if (event.command === '--copy' && event.source === dom.copyKey) copyKey();
  });
} else {
  dom.copyKey.addEventListener('click', copyKey);
}

dom.tb.addEventListener('click', (event) => {
  const button = event.target.closest('button[data-action]');
  if (!button) return;
  if (button.dataset.action === 'delete') deleteKey(button);
  else if (button.dataset.action === 'toggle') toggleKey(button);
  else if (button.dataset.action === 'test') testKey(button);
  else if (button.dataset.action === 'view-key') viewKey(button);
});

dom.tb.addEventListener('focusin', (event) => {
  const input = event.target;
  if (isCellInput(input)) input.dataset.original = input.value;
});

dom.tb.addEventListener('keydown', (event) => {
  const input = event.target;
  if (!isCellInput(input)) return;
  // 输入法组字期间 Enter/Esc 属于候选框操作，不能当成提交/取消
  if (event.isComposing || event.keyCode === 229) return;
  if (event.key === 'Enter') {
    event.preventDefault();
    input.blur();
  } else if (event.key === 'Escape') {
    event.preventDefault();
    input.value = input.dataset.original || '';
    input.blur();
  }
});

dom.tb.addEventListener('change', async (event) => {
  const input = event.target;
  if (!isCellInput(input) || input.dataset.saving === '1') return;

  const id = input.dataset.id;
  const field = input.dataset.field;
  const original = input.dataset.original || '';
  const value = input.value.trim();

  if (value === original) return;
  if (!value) {
    input.value = original;
    notify(field === 'name' ? '名称不能为空' : 'Endpoint 不能为空', 'error');
    return;
  }

  input.dataset.saving = '1';
  input.readOnly = true;
  input.setAttribute('aria-busy', 'true');
  try {
    await api(`${API.keys}/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: JSON.stringify({ [field]: value })
    });
    input.dataset.original = value;
    notify(`已更新${field === 'name' ? '名称' : ' Endpoint'}`, 'ok');
  } catch (err) {
    input.value = original;
    notify(`更新失败：${err.message}`, 'error');
  } finally {
    input.dataset.saving = '0';
    input.readOnly = false;
    input.removeAttribute('aria-busy');
  }
});

/* ---------------- 新增 Key ---------------- */

function currentEndpoint() {
  if (dom.fEndpointSelect.value === '__custom__') return dom.fEndpoint.value.trim();
  return dom.fEndpointSelect.value;
}

function markField(field, invalid) {
  field.classList.toggle('is-invalid', invalid);
  const control = field.querySelector('input, select');
  if (!control) return;
  if (invalid) control.setAttribute('aria-invalid', 'true');
  else control.removeAttribute('aria-invalid');
}

async function addKey(event) {
  event.preventDefault();
  if (dom.addBtn.disabled) return;

  const name = dom.fName.value.trim();
  const endpoint = currentEndpoint();
  const authKey = dom.fAuth.value.trim();
  const provider = dom.fProvider.value;
  const quota = dom.fQuota.value;

  // 自动识别依赖 Key 后缀（:fx 官方免费 / :dp 第三方，其余按官方付费）；
  // deeplx 没有可推断的规则，必须显式给 Endpoint。
  let endpointError = '';
  if (!endpoint) endpointError = '请填写 Endpoint';
  else if (endpoint === 'auto' && provider === 'deeplx') endpointError = 'deeplx 需要显式选择 Endpoint';

  const nameInvalid = !name;
  const endpointInvalid = Boolean(endpointError);
  markField(dom.nameField, nameInvalid);
  markField(dom.endpointField, endpointInvalid);
  dom.addError.textContent = '';

  if (nameInvalid || endpointInvalid) {
    dom.addError.textContent = nameInvalid ? '请填写 Name' : endpointError;
    (nameInvalid ? dom.fName : dom.fAuth).focus();
    return;
  }

  setBusy(dom.addBtn, true, '提交中…');
  try {
    await api(API.keys, {
      method: 'POST',
      body: JSON.stringify({ name, auth_key: authKey, endpoint, provider, quota_period: quota })
    });
    dom.fName.value = '';
    dom.fAuth.value = '';
    dom.addError.textContent = '';
    dom.fEndpointSelect.value = 'auto';
    dom.fQuota.value = 'auto';
    notify(`已新增「${name}」`, 'ok');
    await loadKeys({ silent: true });
  } catch (err) {
    dom.addError.textContent = `新增失败：${err.message}`;
  } finally {
    setBusy(dom.addBtn, false);
  }
}

/* ---------------- 缓存操作 ---------------- */

dom.cacheTb.addEventListener('click', async (event) => {
  const button = event.target.closest('button[data-action="delete-cache"]');
  if (!button) return;
  const key = button.dataset.key || '';
  const preview = button.dataset.preview || key.slice(0, 32);

  const ok = await confirmAction({
    title: '删除缓存条目',
    body: `确定删除「${preview}…」？删除后该段翻译需要重新请求。`,
    confirmText: '删除'
  });
  if (!ok) return;

  await withBusy(button, async () => {
    await api(`${API.cache}?key=${encodeURIComponent(key)}`, { method: 'DELETE' });
    notify('已删除缓存条目', 'ok');
    await loadCache({ silent: true });
  }, '删除失败');
});

dom.cacheDetails.addEventListener('toggle', () => {
  if (dom.cacheDetails.open) loadCache();
});

dom.pageSize.addEventListener('change', () => {
  state.page.limit = Number(dom.pageSize.value) || 30;
  state.page.offset = 0;
  loadCache();
});

dom.cachePrev.addEventListener('click', () => {
  state.page.offset = Math.max(0, state.page.offset - state.page.limit);
  loadCache();
});

dom.cacheNext.addEventListener('click', () => {
  state.page.offset += state.page.limit;
  loadCache();
});

/* ---------------- 登录 / 会话 ---------------- */

async function login(event) {
  event.preventDefault();
  if (dom.loginBtn.disabled) return;

  const token = dom.token.value.trim();
  dom.loginError.textContent = '';
  dom.token.removeAttribute('aria-invalid');
  dom.tokenField.classList.remove('is-invalid');

  if (!token) {
    dom.loginError.textContent = '请输入管理员令牌';
    dom.token.setAttribute('aria-invalid', 'true');
    dom.tokenField.classList.add('is-invalid');
    dom.token.focus();
    return;
  }

  setBusy(dom.loginBtn, true, '验证中…');
  try {
    await api(API.login, { method: 'POST', body: JSON.stringify({ admin_token: token }) });
    showApp();
    await refreshAll();
    dom.main.focus({ preventScroll: true });
  } catch (err) {
    dom.loginError.textContent = err.status === 401 ? '令牌不正确，请重试' : err.message;
    dom.token.setAttribute('aria-invalid', 'true');
    dom.tokenField.classList.add('is-invalid');
    dom.token.focus();
    dom.token.select();
  } finally {
    setBusy(dom.loginBtn, false);
  }
}

/** 首次初始化：写入管理员令牌后直接进入控制台。 */
async function setup(event) {
  event.preventDefault();
  if (dom.setupBtn.disabled) return;

  const admin = dom.setupAdmin.value.trim();
  const gateway = dom.setupGateway.value.trim();
  dom.setupError.textContent = '';
  dom.setupAdmin.removeAttribute('aria-invalid');

  if (admin.length < 8) {
    dom.setupError.textContent = '管理员令牌至少 8 个字符';
    dom.setupAdmin.setAttribute('aria-invalid', 'true');
    dom.setupAdmin.focus();
    return;
  }

  setBusy(dom.setupBtn, true, '初始化中…');
  try {
    await api(API.setup, {
      method: 'POST',
      body: JSON.stringify({ admin_token: admin, gateway_token: gateway })
    });
    dom.setupAdmin.value = '';
    dom.setupGateway.value = '';
    showApp();
    await refreshAll();
    dom.main.focus({ preventScroll: true });
  } catch (err) {
    dom.setupError.textContent = err.message;
    dom.setupAdmin.setAttribute('aria-invalid', 'true');
    dom.setupAdmin.focus();
  } finally {
    setBusy(dom.setupBtn, false);
  }
}

async function boot() {
  try {
    const data = await api(API.session);
    if (data && data.setup_required) {
      showSetup();
      return;
    }
    if (data && data.authenticated) {
      showApp();
      await refreshAll();
      return;
    }
    showLogin();
  } catch (err) {
    if (err.status === 401) showLogin(err.message);
    else showLogin(`无法连接服务：${err.message}`);
  }
}

/* ---------------- 设置 ---------------- */

function fillSettings(settings) {
  dom.sAdminToken.value = '';
  dom.sGatewayToken.value = settings.gateway_token || '';
  dom.sSessionTTL.value = settings.session_ttl;
  dom.sTTLDays.value = settings.cache_ttl_days;
  dom.sCleanupInterval.value = settings.cleanup_interval;
  dom.sCleanupBatch.value = settings.cleanup_batch_size;
  dom.sCleanupRounds.value = settings.cleanup_max_rounds;
  dom.sLegacyOff.checked = !!settings.legacy_key_disabled;

  const wh = settings.webhook || {};
  dom.wEnabled.checked = !!wh.enabled;
  dom.wFormat.value = wh.format || 'generic';
  dom.wURL.value = wh.url || '';
  dom.wNickname.value = wh.nickname || '';
  dom.wTitlePrefix.value = wh.title_prefix || '';
  dom.wTimeout.value = wh.timeout_seconds;
  dom.wMinInterval.value = wh.min_interval_seconds;
  dom.wNotifyKeyDisabled.checked = !!wh.notify_key_disabled;
  dom.wNotifyTemporary.checked = !!wh.notify_temporary;
  dom.wNotifyNoKeys.checked = !!wh.notify_no_keys;
  dom.webhookTestResult.textContent = '';
  syncWebhookFields();
}

function syncWebhookFields() {
  const meow = dom.wFormat.value === 'meow';
  // meow 走固定的服务地址与昵称，不需要用户填 URL；其它格式必须填。
  dom.wNicknameField.hidden = !meow;
  dom.wURLField.hidden = meow;
  dom.wURL.required = !meow;
}

async function openSettings() {
  dom.settingsError.textContent = '';
  dom.settingsBtn.setAttribute('aria-busy', 'true');
  try {
    const data = await api(API.settings);
    if (!data || !data.settings) throw new ApiError('设置读取失败');
    fillSettings(data.settings);
    state.settingsBaseline = settingsBudget();
    state.settingsDirty = false;
    dom.settingsDialog.showModal();
    requestAnimationFrame(() => dom.sAdminToken.focus());
  } catch (err) {
    notify(`读取设置失败：${err.message}`, 'error');
  } finally {
    dom.settingsBtn.removeAttribute('aria-busy');
  }
}

function settingsBudget() {
  return {
    admin_token: dom.sAdminToken.value.trim(),
    gateway_token: dom.sGatewayToken.value.trim(),
    session_ttl: dom.sSessionTTL.value,
    cache_ttl_days: dom.sTTLDays.value,
    cleanup_interval: dom.sCleanupInterval.value,
    cleanup_batch_size: dom.sCleanupBatch.value,
    cleanup_max_rounds: dom.sCleanupRounds.value,
    legacy_key_disabled: dom.sLegacyOff.checked ? 'true' : 'false',
    webhook: {
      enabled: dom.wEnabled.checked ? 'true' : 'false',
      format: dom.wFormat.value,
      url: dom.wURL.value.trim(),
      nickname: dom.wNickname.value.trim(),
      title_prefix: dom.wTitlePrefix.value.trim(),
      timeout_seconds: dom.wTimeout.value,
      min_interval_seconds: dom.wMinInterval.value,
      notify_key_disabled: dom.wNotifyKeyDisabled.checked ? 'true' : 'false',
      notify_temporary: dom.wNotifyTemporary.checked ? 'true' : 'false',
      notify_no_keys: dom.wNotifyNoKeys.checked ? 'true' : 'false'
    }
  };
}

async function saveSettings(event) {
  event.preventDefault();
  if (dom.settingsSave.disabled || !state.settingsBaseline) return;
  dom.settingsError.textContent = '';

  const { webhook, ...current } = settingsBudget();
  const { webhook: previousWebhook, ...previous } = state.settingsBaseline;
  const diff = (values, baseline) => Object.fromEntries(
    Object.entries(values).filter(([key, value]) => value !== baseline[key])
  );
  const patch = diff(current, previous);
  const webhookPatch = diff(webhook, previousWebhook);
  if (Object.keys(webhookPatch).length) patch.webhook = webhookPatch;
  if (!Object.keys(patch).length) {
    dom.settingsDialog.close('cancel');
    return;
  }

  setBusy(dom.settingsSave, true, '保存中…');
  try {
    const data = await api(API.settings, { method: 'PUT', body: JSON.stringify(patch) });
    if (data && data.settings) fillSettings(data.settings);
    state.settingsDirty = false;
    dom.settingsDialog.close('saved');
    notify('设置已保存', 'ok');
  } catch (err) {
    dom.settingsError.textContent = err.message;
    dom.settingsError.focus();
  } finally {
    setBusy(dom.settingsSave, false);
  }
}

async function rotateSecret() {
  const ok = await confirmAction({
    title: '轮换会话密钥',
    body: '所有已登录的会话都会失效，需要重新登录。继续吗？',
    confirmText: '轮换'
  });
  if (!ok) return;
  await withBusy(dom.rotateSecretBtn, async () => {
    await api(API.rotateSecret, { method: 'POST' });
    showLogin('会话密钥已轮换，请重新登录。');
  }, '轮换会话密钥失败');
}

async function testWebhook() {
  dom.webhookTestResult.textContent = '';
  await withBusy(dom.webhookTestBtn, async () => {
    try {
      const budget = settingsBudget();
      await api(API.webhookTest, { method: 'POST', body: JSON.stringify(budget.webhook) });
      dom.webhookTestResult.textContent = '已发送测试通知';
    } catch (err) {
      dom.webhookTestResult.textContent = `发送失败：${err.message}`;
    }
  }, '发送测试通知失败');
}

/* ---------------- 事件绑定 ---------------- */

dom.loginForm.addEventListener('submit', (event) => {
  login(event).catch((err) => {
    dom.loginError.textContent = err.message;
  });
});

dom.setupForm.addEventListener('submit', (event) => {
  setup(event).catch((err) => {
    dom.setupError.textContent = err.message;
  });
});

dom.settingsBtn.addEventListener('click', () => {
  openSettings();
});

dom.settingsForm.addEventListener('submit', saveSettings);

dom.settingsForm.addEventListener('input', () => {
  state.settingsDirty = true;
  dom.settingsError.textContent = '';
});

dom.settingsForm.addEventListener('change', () => {
  state.settingsDirty = true;
});

dom.settingsClose.addEventListener('click', () => {
  dom.settingsDialog.close('cancel');
});

/* Esc / 返回手势：有未保存修改时先拦下来，提示用户明确选择 */
dom.settingsDialog.addEventListener('cancel', (event) => {
  if (!state.settingsDirty) return;
  event.preventDefault();
  dom.settingsError.textContent = '修改尚未保存：点「保存」写回数据库，或点「取消」放弃。';
  dom.settingsSave.focus();
});

dom.settingsDialog.addEventListener('close', () => {
  dom.settingsDialog.returnValue = '';
  state.settingsBaseline = null;
  state.settingsDirty = false;
  dom.settingsError.textContent = '';
});

dom.settingsCancel.addEventListener('click', () => {
  dom.settingsDialog.close('cancel');
});

dom.rotateSecretBtn.addEventListener('click', () => {
  rotateSecret().catch((err) => {
    dom.settingsError.textContent = err.message;
  });
});

dom.webhookTestBtn.addEventListener('click', () => {
  testWebhook();
});

dom.wFormat.addEventListener('change', syncWebhookFields);

dom.addForm.addEventListener('submit', (event) => {
  addKey(event).catch((err) => {
    dom.addError.textContent = `新增失败：${err.message}`;
  });
});

dom.fEndpointSelect.addEventListener('change', () => {
  const custom = dom.fEndpointSelect.value === '__custom__';
  dom.fEndpoint.hidden = !custom;
  dom.fEndpoint.required = custom;
  markField(dom.endpointField, false);
  if (custom) dom.fEndpoint.focus();
});

dom.reload.addEventListener('click', () => {
  withBusy(dom.reload, refreshAll, '刷新失败');
});

dom.refresh.addEventListener('click', () => {
  withBusy(dom.refresh, async () => {
    await api(API.usage, { method: 'POST' });
    notify('已刷新用量', 'ok');
    await loadKeys({ silent: true });
  }, '刷新用量失败');
});

dom.logout.addEventListener('click', () => {
  withBusy(dom.logout, async () => {
    try {
      await api(API.logout, { method: 'POST' });
    } catch (err) {
      if (err.status !== 401) throw err;
    }
    showLogin();
  }, '退出失败');
});

/* 倒计时每 15 秒刷新一次；页面重新可见时立即校准 */
setInterval(() => {
  document.querySelectorAll('[data-until]').forEach((node) => {
    const until = Number(node.getAttribute('data-until'));
    if (until) node.textContent = countdownText(until);
  });
}, 15000);

document.addEventListener('visibilitychange', () => {
  if (!document.hidden) {
    document.querySelectorAll('[data-until]').forEach((node) => {
      const until = Number(node.getAttribute('data-until'));
      if (until) node.textContent = countdownText(until);
    });
  }
});

boot();
