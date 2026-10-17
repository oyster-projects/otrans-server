'use strict';

const STORAGE_KEY = 'otrans.web.device.v1';
const MAX_FILES = 9;
const MAX_FILE_BYTES = 128 * 1024 * 1024;
const apiRoot = new URL('../api/v1/', window.location.href);

const elements = {
  pairView: document.querySelector('#pair-view'), appView: document.querySelector('#app-view'),
  pairForm: document.querySelector('#pair-form'), pairButton: document.querySelector('#pair-button'),
  pairError: document.querySelector('#pair-error'), pairCode: document.querySelector('#pair-code'),
  deviceName: document.querySelector('#device-name'), remember: document.querySelector('#remember-device'),
  connection: document.querySelector('#connection-state'), currentDevice: document.querySelector('#current-device'),
  serverIdentity: document.querySelector('#server-identity'), notificationButton: document.querySelector('#notification-button'),
  disconnectButton: document.querySelector('#disconnect-button'), text: document.querySelector('#text-content'),
  clipboardButton: document.querySelector('#clipboard-button'), sendTextButton: document.querySelector('#send-text-button'),
  fileInput: document.querySelector('#file-input'), dropZone: document.querySelector('#drop-zone'),
  selectedFiles: document.querySelector('#selected-files'), uploadStatus: document.querySelector('#upload-status'),
  sendFilesButton: document.querySelector('#send-files-button'), refreshButton: document.querySelector('#refresh-button'),
  recordsStatus: document.querySelector('#records-status'), recordsList: document.querySelector('#records-list'),
  toast: document.querySelector('#toast')
};

let session = loadSession();
let device = null;
let selectedFiles = [];
let transfers = new Map();
let socket = null;
let socketGeneration = 0;
let reconnectAttempt = 0;
let reconnectTimer = 0;
let toastTimer = 0;

elements.pairForm.addEventListener('submit', pairDevice);
elements.disconnectButton.addEventListener('click', disconnect);
elements.clipboardButton.addEventListener('click', readClipboard);
elements.sendTextButton.addEventListener('click', sendText);
elements.dropZone.addEventListener('click', () => elements.fileInput.click());
elements.fileInput.addEventListener('change', () => addFiles(elements.fileInput.files));
elements.sendFilesButton.addEventListener('click', sendFiles);
elements.refreshButton.addEventListener('click', () => refreshAll(true));
elements.notificationButton.addEventListener('click', enableNotifications);
elements.selectedFiles.addEventListener('click', removeSelectedFile);
elements.recordsList.addEventListener('click', handleRecordAction);

for (const eventName of ['dragenter', 'dragover']) {
  elements.dropZone.addEventListener(eventName, (event) => {
    event.preventDefault();
    elements.dropZone.classList.add('dragging');
  });
}
for (const eventName of ['dragleave', 'drop']) {
  elements.dropZone.addEventListener(eventName, (event) => {
    event.preventDefault();
    elements.dropZone.classList.remove('dragging');
  });
}
elements.dropZone.addEventListener('drop', (event) => addFiles(event.dataTransfer.files));
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && session?.token) {
    void refreshAll(false);
    connectRealtime();
  }
});

if (session?.token) {
  void startSession();
} else {
  showPairView();
}

async function pairDevice(event) {
  event.preventDefault();
  setBusy(elements.pairButton, true, '正在配对…');
  elements.pairError.hidden = true;
  try {
    const payload = await request('pair', {
      method: 'POST', authenticated: false,
      json: { code: elements.pairCode.value.trim(), name: elements.deviceName.value.trim(), platform: 'web' }
    });
    const serverInfo = await request('server-info', { authenticated: false });
    session = {
      token: payload.token, deviceName: payload.device.name, deviceId: payload.device.id,
      cursor: Number(payload.device.cursor) || 0, serverId: serverInfo.serverId,
      persistent: elements.remember.checked
    };
    saveSession();
    elements.pairCode.value = '';
    await startSession();
    showToast('网页版已配对');
  } catch (error) {
    elements.pairError.textContent = errorMessage(error);
    elements.pairError.hidden = false;
  } finally {
    setBusy(elements.pairButton, false, '连接中转站');
  }
}

async function startSession() {
  setConnection('connecting', '正在连接');
  try {
    const [serverInfo, currentDevice] = await Promise.all([
      request('server-info', { authenticated: false }), request('devices/me')
    ]);
    if (session.serverId && serverInfo.serverId !== session.serverId) {
      throw new Error('当前网页不属于已配对的飞牛服务器');
    }
    session.serverId = serverInfo.serverId;
    device = currentDevice;
    session.cursor = Math.max(Number(session.cursor) || 0, Number(device.cursor) || 0);
    session.deviceId = device.id;
    session.deviceName = device.name;
    saveSession();
    elements.currentDevice.textContent = device.name;
    elements.serverIdentity.textContent = `server ${serverInfo.serverId.slice(0, 8)} · protocol ${serverInfo.protocolVersion}`;
    elements.pairView.hidden = true;
    elements.appView.hidden = false;
    updateNotificationButton();
    await refreshAll(false);
    connectRealtime();
  } catch (error) {
    setConnection('offline', '连接失败');
    if (String(errorMessage(error)).includes('令牌无效')) clearSession();
    showPairView(errorMessage(error));
  }
}

async function refreshAll(feedback) {
  if (!session?.token) return;
  try {
    await syncEvents();
    const payload = await request('transfers?limit=100');
    renderTransfers((payload.transfers || []).filter((transfer) => !transfer.deletedAt));
    if (feedback) showToast(`已刷新 ${(payload.transfers || []).length} 条记录`);
  } catch (error) {
    elements.recordsStatus.textContent = errorMessage(error);
    if (feedback) showToast(errorMessage(error));
  }
}

async function syncEvents() {
  let received = 0;
  while (true) {
    const payload = await request(`events?after=${encodeURIComponent(session.cursor || 0)}&limit=100`);
    const events = payload.events || [];
    for (const event of events) {
      session.cursor = Math.max(Number(session.cursor) || 0, Number(event.sequence) || 0);
      if (event.type === 'transfer.created' && event.payload?.sourceDeviceId !== device?.id) received += 1;
    }
    saveSession();
    if (events.length > 0) {
      await request('devices/me/cursor', { method: 'PUT', json: { cursor: session.cursor } });
    }
    if (events.length < 100) break;
  }
  if (received > 0) notify('OTrans 收到新内容', `新增 ${received} 条中转记录`);
}

function connectRealtime() {
  if (!session?.token || document.hidden) return;
  const generation = ++socketGeneration;
  if (socket) socket.close();
  window.clearTimeout(reconnectTimer);
  setConnection('connecting', '正在连接');
  void request('ws-ticket', { method: 'POST' }).then((payload) => {
    if (generation !== socketGeneration) return;
    const url = new URL('ws', apiRoot);
    url.protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    url.searchParams.set('ticket', payload.ticket);
    socket = new WebSocket(url);
    socket.addEventListener('open', () => {
      if (generation !== socketGeneration) return;
      reconnectAttempt = 0;
      setConnection('online', '实时已连接');
    });
    socket.addEventListener('message', (event) => {
      if (generation !== socketGeneration) return;
      try {
        const message = JSON.parse(event.data);
        if (message.type === 'events.available') void refreshAll(false);
      } catch (_) { /* 忽略未知实时消息。 */ }
    });
    socket.addEventListener('close', () => {
      if (generation !== socketGeneration) return;
      socket = null;
      scheduleReconnect();
    });
    socket.addEventListener('error', () => socket?.close());
  }).catch(() => {
    if (generation === socketGeneration) scheduleReconnect();
  });
}

function scheduleReconnect() {
  setConnection('connecting', '正在重连');
  window.clearTimeout(reconnectTimer);
  const delay = Math.min(30000, 1000 * (2 ** Math.min(reconnectAttempt, 5))) + Math.random() * 400;
  reconnectAttempt += 1;
  reconnectTimer = window.setTimeout(connectRealtime, delay);
}

async function sendText() {
  const text = elements.text.value;
  if (!text.trim()) return showToast('请输入要发送的文本');
  setBusy(elements.sendTextButton, true, '发送中…');
  try {
    const created = await request('transfers', {
      method: 'POST', json: {
        clientTransferId: OTransCrypto.clientTransferId(), title: '来自网页版的文本',
        items: [{ kind: 'text', text, mimeType: 'text/plain' }]
      }
    });
    await request(`transfers/${encodeURIComponent(created.transfer.id)}/commit`, { method: 'POST' });
    elements.text.value = '';
    showToast('文本已发送');
    await refreshAll(false);
  } catch (error) {
    showToast(errorMessage(error));
  } finally {
    setBusy(elements.sendTextButton, false, '发送文本');
  }
}

async function sendFiles() {
  if (selectedFiles.length === 0) return;
  setBusy(elements.sendFilesButton, true, '正在校验…');
  try {
    const prepared = [];
    for (let index = 0; index < selectedFiles.length; index += 1) {
      const file = selectedFiles[index];
      elements.uploadStatus.textContent = `正在校验 ${index + 1}/${selectedFiles.length}：${file.name}`;
      prepared.push({ file, sha256: await sha256(file) });
    }
    const title = prepared.length === 1 ? `来自网页版：${prepared[0].file.name}` : `来自网页版的 ${prepared.length} 个文件`;
    const created = await request('transfers', {
      method: 'POST', json: {
        clientTransferId: OTransCrypto.clientTransferId(), title,
        items: prepared.map(({ file, sha256: digest }) => ({
          kind: 'file', name: file.name, mimeType: file.type || 'application/octet-stream', size: file.size, sha256: digest
        }))
      }
    });
    const missing = new Set(created.missingBlobs || []);
    const uploaded = new Set();
    for (let index = 0; index < prepared.length; index += 1) {
      const entry = prepared[index];
      if (!missing.has(entry.sha256) || uploaded.has(entry.sha256)) continue;
      elements.uploadStatus.textContent = `正在上传 ${index + 1}/${prepared.length}：${entry.file.name}`;
      await request(`blobs/${entry.sha256}`, {
        method: 'PUT', body: entry.file, contentType: entry.file.type || 'application/octet-stream', expectJSON: false
      });
      uploaded.add(entry.sha256);
    }
    await request(`transfers/${encodeURIComponent(created.transfer.id)}/commit`, { method: 'POST' });
    selectedFiles = [];
    renderSelectedFiles();
    showToast('文件已发送');
    await refreshAll(false);
  } catch (error) {
    elements.uploadStatus.textContent = errorMessage(error);
    showToast(errorMessage(error));
  } finally {
    setBusy(elements.sendFilesButton, false, '上传所选内容');
    elements.sendFilesButton.disabled = selectedFiles.length === 0;
  }
}

function addFiles(fileList) {
  const next = [...selectedFiles];
  for (const file of Array.from(fileList || [])) {
    if (next.length >= MAX_FILES) break;
    if (file.size > MAX_FILE_BYTES) {
      showToast(`${file.name} 超过 128 MiB`);
      continue;
    }
    if (!next.some((known) => known.name === file.name && known.size === file.size && known.lastModified === file.lastModified)) next.push(file);
  }
  selectedFiles = next;
  elements.fileInput.value = '';
  renderSelectedFiles();
}

function renderSelectedFiles() {
  elements.selectedFiles.replaceChildren();
  selectedFiles.forEach((file, index) => {
    const row = document.createElement('div'); row.className = 'selected-file';
    const name = document.createElement('span'); name.textContent = file.name;
    const size = document.createElement('small'); size.textContent = formatBytes(file.size);
    const remove = document.createElement('button'); remove.className = 'remove-file'; remove.type = 'button';
    remove.dataset.index = String(index); remove.textContent = '移除';
    row.append(name, size, remove); elements.selectedFiles.append(row);
  });
  elements.uploadStatus.textContent = selectedFiles.length ? `已选择 ${selectedFiles.length} 项` : '尚未选择文件';
  elements.sendFilesButton.disabled = selectedFiles.length === 0;
}

function removeSelectedFile(event) {
  const button = event.target.closest('button[data-index]');
  if (!button) return;
  selectedFiles.splice(Number(button.dataset.index), 1);
  renderSelectedFiles();
}

function renderTransfers(items) {
  transfers = new Map(items.map((transfer) => [transfer.id, transfer]));
  elements.recordsList.replaceChildren();
  elements.recordsStatus.textContent = items.length ? `${items.length} 条有效记录，最新在前` : '暂时没有中转记录';
  if (items.length === 0) {
    const empty = document.createElement('div'); empty.className = 'empty-state'; empty.textContent = '从手机、电脑或本页发送内容后，会显示在这里。';
    elements.recordsList.append(empty); return;
  }
  for (const transfer of items) {
    const card = document.createElement('article'); card.className = 'record'; card.dataset.transfer = transfer.id;
    const head = document.createElement('div'); head.className = 'record-head';
    const titleWrap = document.createElement('div'); titleWrap.className = 'item-main';
    const title = document.createElement('h3'); title.className = 'record-title'; title.textContent = transfer.title || '未命名中转内容';
    const meta = document.createElement('p'); meta.className = 'record-meta';
    meta.textContent = `${transfer.sourceDeviceId === device?.id ? '来自本页' : '来自其他设备'} · ${formatTime(transfer.committedAt || transfer.createdAt)}`;
    titleWrap.append(title, meta);
    const remove = document.createElement('button'); remove.className = 'delete-record'; remove.type = 'button'; remove.dataset.action = 'delete'; remove.dataset.transfer = transfer.id; remove.textContent = '全局删除';
    head.append(titleWrap, remove); card.append(head);
    (transfer.items || []).forEach((item, index) => card.append(renderItem(transfer.id, item, index)));
    elements.recordsList.append(card);
  }
}

function renderItem(transferId, item, index) {
  const row = document.createElement('div'); row.className = 'record-item';
  const main = document.createElement('div'); main.className = 'item-main';
  const title = document.createElement('strong'); title.textContent = item.kind === 'text' ? '文本' : (item.name || '未命名文件');
  const detail = document.createElement('p'); detail.textContent = item.kind === 'text' ? (item.text || '') : formatBytes(item.size || 0);
  main.append(title, detail);
  const action = document.createElement('button'); action.className = 'item-action'; action.type = 'button';
  action.dataset.action = item.kind === 'text' ? 'copy' : 'download'; action.dataset.transfer = transferId; action.dataset.item = String(index);
  action.textContent = item.kind === 'text' ? '复制' : '下载'; row.append(main, action); return row;
}

async function handleRecordAction(event) {
  const button = event.target.closest('button[data-action]');
  if (!button) return;
  const transfer = transfers.get(button.dataset.transfer);
  if (!transfer) return;
  if (button.dataset.action === 'delete') {
    if (!window.confirm('这会从飞牛和所有客户端中同步删除该记录，确定继续吗？')) return;
    setBusy(button, true, '删除中…');
    try {
      await request(`transfers/${encodeURIComponent(transfer.id)}`, { method: 'DELETE', expectJSON: false });
      showToast('已全局删除'); await refreshAll(false);
    } catch (error) { showToast(errorMessage(error)); }
    finally { setBusy(button, false, '全局删除'); }
    return;
  }
  const item = transfer.items?.[Number(button.dataset.item)];
  if (!item) return;
  if (button.dataset.action === 'copy') {
    await writeClipboard(item.text || ''); showToast('文本已复制'); return;
  }
  setBusy(button, true, '校验中…');
  try {
    const response = await authorizedFetch(`blobs/${encodeURIComponent(item.sha256)}?name=${encodeURIComponent(item.name || 'file')}`);
    if (!response.ok) throw await responseError(response);
    const blob = await response.blob();
    if (blob.size !== Number(item.size) || await sha256(blob) !== String(item.sha256).toLowerCase()) throw new Error('文件完整性校验失败');
    const url = URL.createObjectURL(blob); const link = document.createElement('a'); link.href = url; link.download = item.name || 'download'; link.click();
    window.setTimeout(() => URL.revokeObjectURL(url), 30000);
  } catch (error) { showToast(errorMessage(error)); }
  finally { setBusy(button, false, '下载'); }
}

async function readClipboard() {
  try { elements.text.value = await navigator.clipboard.readText(); showToast('已读取当前剪贴板'); }
  catch (_) { showToast('浏览器未允许读取剪贴板，请直接粘贴'); }
}

async function writeClipboard(text) {
  try { await navigator.clipboard.writeText(text); }
  catch (_) {
    const area = document.createElement('textarea'); area.value = text; area.style.position = 'fixed'; area.style.opacity = '0';
    document.body.append(area); area.select(); document.execCommand('copy'); area.remove();
  }
}

async function enableNotifications() {
  if (!('Notification' in window)) return showToast('当前浏览器不支持系统通知');
  if (!window.isSecureContext) return showToast('浏览器通知需要 HTTPS 访问');
  try {
    const permission = await Notification.requestPermission(); updateNotificationButton();
    showToast(permission === 'granted' ? '浏览器通知已开启' : '未获得通知权限');
  } catch (_) { showToast('无法开启浏览器通知'); }
}

function notify(title, body) {
  if ('Notification' in window && Notification.permission === 'granted') new Notification(title, { body });
  else showToast(body);
}

function updateNotificationButton() {
  const granted = 'Notification' in window && Notification.permission === 'granted';
  elements.notificationButton.textContent = granted ? '浏览器通知已开启' : '开启浏览器通知';
  elements.notificationButton.disabled = granted;
}

function disconnect() {
  if (!window.confirm('只断开这台浏览器，飞牛中的中转记录不会删除。确定继续吗？')) return;
  socketGeneration += 1; if (socket) socket.close(); socket = null; clearSession(); showPairView(); showToast('已断开网页版');
}

function showPairView(message = '') {
  elements.appView.hidden = true; elements.pairView.hidden = false; setConnection('offline', '未配对');
  if (message) { elements.pairError.textContent = message; elements.pairError.hidden = false; }
}

async function request(path, options = {}) {
  const headers = {};
  if (options.authenticated !== false) {
    if (!session?.token) throw new Error('尚未配对');
    headers.Authorization = `Bearer ${session.token}`;
  }
  let body = options.body;
  if (options.json !== undefined) { headers['Content-Type'] = 'application/json'; body = JSON.stringify(options.json); }
  if (options.contentType) headers['Content-Type'] = options.contentType;
  const response = await fetch(new URL(path.replace(/^\//, ''), apiRoot), { method: options.method || 'GET', headers, body, cache: 'no-store' });
  if (!response.ok) throw await responseError(response);
  if (response.status === 204 || options.expectJSON === false) return null;
  return response.json();
}

function authorizedFetch(path) {
  return fetch(new URL(path.replace(/^\//, ''), apiRoot), { headers: { Authorization: `Bearer ${session.token}` }, cache: 'no-store' });
}

async function responseError(response) {
  let message = `服务返回 ${response.status}`;
  try { const payload = await response.json(); if (payload.error) message = payload.error; } catch (_) { /* 使用状态码。 */ }
  if (response.status === 401) message = '设备令牌无效，请重新配对';
  return new Error(message);
}

async function sha256(blob) {
  return OTransCrypto.sha256Blob(blob);
}

function loadSession() {
  for (const storage of [sessionStorage, localStorage]) {
    try { const value = JSON.parse(storage.getItem(STORAGE_KEY)); if (value?.token) return value; } catch (_) { /* 忽略损坏状态。 */ }
  }
  return null;
}

function saveSession() {
  if (!session) return;
  const target = session.persistent ? localStorage : sessionStorage;
  const other = session.persistent ? sessionStorage : localStorage;
  other.removeItem(STORAGE_KEY); target.setItem(STORAGE_KEY, JSON.stringify(session));
}

function clearSession() {
  localStorage.removeItem(STORAGE_KEY); sessionStorage.removeItem(STORAGE_KEY); session = null; device = null;
}

function setConnection(kind, text) {
  elements.connection.className = `status ${kind === 'online' ? '' : kind}`.trim();
  elements.connection.lastChild.textContent = text;
}

function setBusy(button, busy, label) { button.disabled = busy; button.textContent = label; }
function errorMessage(error) { return error instanceof Error ? error.message : String(error || '操作失败'); }
function showToast(message) {
  elements.toast.textContent = message; elements.toast.hidden = false; window.clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => { elements.toast.hidden = true; }, 2600);
}
function formatBytes(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
}
function formatTime(value) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : new Intl.DateTimeFormat('zh-CN', {
    month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false
  }).format(date);
}
