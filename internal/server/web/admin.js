const form = document.querySelector('#pair-form');
const tokenInput = document.querySelector('#admin-token');
const toggleButton = document.querySelector('#toggle-token');
const generateButton = document.querySelector('#generate');
const errorBox = document.querySelector('#error');
const resultCard = document.querySelector('#result');
const codeOutput = document.querySelector('#pair-code');
const countdown = document.querySelector('#countdown');
const copyButton = document.querySelector('#copy-code');
const health = document.querySelector('#health');
const serverAddress = document.querySelector('#server-address');
const refreshTransfersButton = document.querySelector('#refresh-transfers');
const recordsStatus = document.querySelector('#records-status');
const recordsList = document.querySelector('#records-list');

let countdownTimer = 0;

serverAddress.textContent = window.location.origin;

fetch('/healthz', { cache: 'no-store' })
  .then((response) => {
    if (!response.ok) throw new Error('offline');
    health.textContent = '服务正常';
  })
  .catch(() => {
    health.textContent = '服务异常';
    health.classList.add('offline');
  });

toggleButton.addEventListener('click', () => {
  const show = tokenInput.type === 'password';
  tokenInput.type = show ? 'text' : 'password';
  toggleButton.textContent = show ? '隐藏' : '显示';
});

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  errorBox.hidden = true;
  generateButton.disabled = true;
  generateButton.textContent = '正在生成…';
  try {
    const response = await fetch('/api/v1/admin/pair-codes', {
      method: 'POST',
      headers: { 'X-OTrans-Admin-Token': tokenInput.value.trim() }
    });
    const payload = await response.json();
    if (!response.ok) {
      throw new Error(payload.error === 'invalid admin token' ? '管理员令牌不正确' : (payload.error || '生成失败'));
    }
    showPairCode(payload.code, payload.expiresAt);
    await loadTransfers();
  } catch (error) {
    errorBox.textContent = error instanceof Error ? error.message : '生成失败';
    errorBox.hidden = false;
  } finally {
    generateButton.disabled = false;
    generateButton.textContent = '生成临时配对码';
  }
});

refreshTransfersButton.addEventListener('click', loadTransfers);

copyButton.addEventListener('click', async () => {
  const code = codeOutput.textContent;
  if (!code) return;
  try {
    await navigator.clipboard.writeText(code);
  } catch (_) {
    const input = document.createElement('textarea');
    input.value = code;
    input.style.position = 'fixed';
    input.style.opacity = '0';
    document.body.appendChild(input);
    input.select();
    document.execCommand('copy');
    input.remove();
  }
  copyButton.textContent = '已复制';
  window.setTimeout(() => { copyButton.textContent = '复制'; }, 1600);
});

function showPairCode(code, expiresAt) {
  codeOutput.textContent = code;
  resultCard.hidden = false;
  resultCard.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  window.clearInterval(countdownTimer);

  const expires = new Date(expiresAt).getTime();
  const update = () => {
    const remaining = Math.max(0, expires - Date.now());
    const minutes = Math.floor(remaining / 60000);
    const seconds = Math.floor((remaining % 60000) / 1000);
    if (remaining === 0) {
      countdown.textContent = '该配对码已过期，请重新生成';
      countdown.classList.add('expired');
      window.clearInterval(countdownTimer);
      return;
    }
    countdown.classList.remove('expired');
    countdown.textContent = `剩余 ${minutes}:${String(seconds).padStart(2, '0')}，成功使用一次后立即失效`;
  };
  update();
  countdownTimer = window.setInterval(update, 1000);
}

async function loadTransfers() {
  const token = tokenInput.value.trim();
  if (!token) {
    recordsStatus.textContent = '请先输入管理员令牌。';
    return;
  }
  refreshTransfersButton.disabled = true;
  refreshTransfersButton.textContent = '刷新中…';
  recordsStatus.textContent = '正在读取中转记录…';
  try {
    const response = await fetch('/api/v1/admin/transfers?includeDeleted=true&limit=100', {
      headers: { 'X-OTrans-Admin-Token': token },
      cache: 'no-store'
    });
    const payload = await response.json();
    if (!response.ok) {
      throw new Error(payload.error === 'invalid admin token' ? '管理员令牌不正确' : (payload.error || '读取失败'));
    }
    renderTransfers(payload.transfers || []);
  } catch (error) {
    recordsList.replaceChildren();
    recordsStatus.textContent = error instanceof Error ? error.message : '读取失败';
  } finally {
    refreshTransfersButton.disabled = false;
    refreshTransfersButton.textContent = '刷新记录';
  }
}

function renderTransfers(transfers) {
  recordsList.replaceChildren();
  if (transfers.length === 0) {
    recordsStatus.textContent = '还没有已提交的中转记录。';
    return;
  }
  recordsStatus.textContent = `共 ${transfers.length} 条，最新记录在前。`;
  for (const transfer of transfers) {
    const card = document.createElement('article');
    card.className = 'record';

    const head = document.createElement('div');
    head.className = 'record-head';
    const titleWrap = document.createElement('div');
    const title = document.createElement('h3');
    title.className = 'record-title';
    title.textContent = transfer.title || '未命名中转记录';
    const meta = document.createElement('p');
    meta.className = 'record-meta';
    const source = transfer.sourceDeviceName || transfer.sourceDeviceId || '未知设备';
    meta.textContent = `${source} · ${formatTime(transfer.committedAt || transfer.createdAt)}`;
    titleWrap.append(title, meta);
    const state = document.createElement('span');
    state.className = transfer.deletedAt ? 'record-state deleted' : 'record-state';
    state.textContent = transfer.deletedAt ? '已删除' : '可用';
    head.append(titleWrap, state);
    card.append(head);

    for (const item of transfer.items || []) {
      const itemBox = document.createElement('div');
      itemBox.className = 'record-item';
      if (item.kind === 'text') {
        const text = document.createElement('pre');
        text.className = 'record-text';
        text.textContent = item.text || '';
        itemBox.append(text);
      } else {
        const file = document.createElement('div');
        file.className = 'record-file';
        const name = document.createElement('span');
        name.textContent = item.name || '未命名文件';
        const size = document.createElement('span');
        size.textContent = formatBytes(item.size || 0);
        file.append(name, size);
        itemBox.append(file);
      }
      card.append(itemBox);
    }
    recordsList.append(card);
  }
}

function formatTime(value) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit'
  }).format(date);
}

function formatBytes(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
  return `${(bytes / 1024 / 1024 / 1024).toFixed(1)} GiB`;
}
