const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const card = byId('receive-card');
const senderInput = byId('sender');
const fileInput = byId('receive-input');
const dropZone = byId('receive-drop');
const summary = byId('receive-summary');
const sentList = byId('receive-sent');
const sentRows = byId('sent-rows');
const linkBase = `/${encodeURIComponent(window.location.pathname.split('/')[1] || '')}`;
const NO_ROOM_CODES = ['quota_exceeded', 'disk_full'];
const BUSY_WAIT_MS = 60000;

const waiting = [];
let visit = '';
let sending = false;
let gone = false;
let sentCount = 0;

class LinkGone extends Error {}

function el(tag, text, className) {
  const node = document.createElement(tag);
  if (text !== undefined) node.textContent = text;
  if (className) node.className = className;
  return node;
}

// A visitor cannot free space, so a full disk or quota is told as something to pass on, not to fix.
function visitorFailure(body, fallbackKey) {
  if (body && NO_ROOM_CODES.includes(body.code)) return i18n.problem('receive.noRoom');
  return i18n.failure(body, fallbackKey);
}

function showGone() {
  gone = true;
  card.hidden = true;
  i18n.msg(statusBox, () => t('share.unavailable'));
}

async function whenNotBusy(task) {
  for (let attempt = 0; ; attempt += 1) {
    const answer = await task();
    if (answer.status !== 429 || attempt >= MAX_RETRIES) return answer;
    uploadHooks.note(() => t('receive.slowDown'));
    await pause(BUSY_WAIT_MS);
  }
}

function fileRow(file) {
  const item = el('li');
  const state = el('span', undefined, 'detail');
  const bar = el('progress');
  bar.max = 100;
  bar.value = 0;
  bar.hidden = true;
  bar.setAttribute('data-i18n-attr', 'aria-label:upload.fileProgress');
  bar.setAttribute('aria-label', t('upload.fileProgress'));
  item.append(el('span', file.name, 'preview-name'), state, bar);
  sentRows.append(item);
  sentList.hidden = false;
  i18n.msg(state, () => t('receive.waiting'));
  return { state, bar };
}

function showProgress(row, file, sent) {
  row.bar.hidden = false;
  row.bar.value = file.size === 0 ? 100 : Math.min(100, (sent / file.size) * 100);
  i18n.msg(row.state, () => t('receive.progress', { sent: i18n.size(sent), size: i18n.size(file.size) }));
}

async function startUpload(file) {
  const payload = { name: file.name, size: file.size, sender: senderInput.value.trim(), visit };
  const { status, body } = await whenNotBusy(() => withRetry(() => call('POST', `${linkBase}/uploads`, payload)));
  if (status === 404) throw new LinkGone();
  if (status !== 201) throw visitorFailure(body, 'upload.startFailed');
  if (typeof body.visit === 'string' && body.visit !== '') visit = body.visit;
  return body.id;
}

async function sendChunks(file, id, row) {
  let offset = 0;
  let stalls = 0;
  while (offset < file.size) {
    const from = offset;
    const end = Math.min(from + CHUNK_SIZE, file.size);
    const headers = { 'Content-Range': `bytes ${from}-${end - 1}/${file.size}` };
    const send = () => sendBlob(`${linkBase}/uploads/${id}`, file.slice(from, end), headers, (loaded) => showProgress(row, file, from + loaded));
    const { status, body } = await whenNotBusy(() => withRetry(send));
    if (status === 200) {
      offset = body.offset;
      stalls = 0;
    } else if ((status === 409 || status === 400) && typeof body.offset === 'number') {
      stalls = body.offset === from ? stalls + 1 : 0;
      if (stalls >= 3) throw visitorFailure(body, 'upload.failed');
      offset = body.offset;
    } else if (status === 404) {
      throw new LinkGone();
    } else {
      throw visitorFailure(body, 'upload.failed');
    }
    showProgress(row, file, offset);
  }
}

// When the answer to a finish is lost on the way, the upload may have landed already, and the retry then finds nothing.
async function finishUpload(id) {
  let interrupted = false;
  for (let attempt = 0; ; attempt += 1) {
    try {
      const { status, body } = await whenNotBusy(() => call('POST', `${linkBase}/uploads/${id}/finish`));
      if (status === 204 || (status === 404 && interrupted)) return;
      if (status === 404) throw new LinkGone();
      throw visitorFailure(body, 'upload.incomplete');
    } catch (error) {
      if (!(error instanceof NetworkError) || attempt >= MAX_RETRIES) throw error;
      interrupted = true;
      uploadHooks.note(() => t('upload.retrying', { attempt: attempt + 1, max: MAX_RETRIES }));
      await pause(Math.min(1000 * 2 ** attempt, 15000));
    }
  }
}

async function sendFile(file, row) {
  uploadHooks.note = (producer) => i18n.msg(row.state, producer);
  showProgress(row, file, 0);
  const id = await startUpload(file);
  await sendChunks(file, id, row);
  await finishUpload(id);
  row.bar.value = 100;
  i18n.msg(row.state, () => t('receive.sent'));
}

async function sendWaiting() {
  if (sending) return;
  sending = true;
  senderInput.disabled = true;
  while (waiting.length > 0 && !gone) {
    const { file, row } = waiting.shift();
    try {
      await sendFile(file, row);
      sentCount += 1;
      i18n.msg(summary, () => t('receive.done', { count: sentCount }));
    } catch (error) {
      if (error instanceof LinkGone) {
        showGone();
        break;
      }
      row.state.className = 'error';
      i18n.report(row.state, error);
    }
  }
  sending = false;
}

function addFiles(files) {
  if (gone) return;
  files.forEach((file) => waiting.push({ file, row: fileRow(file) }));
  sendWaiting();
}

fileInput.addEventListener('change', () => {
  const picked = Array.from(fileInput.files);
  fileInput.value = '';
  addFiles(picked);
});

['dragenter', 'dragover'].forEach((name) => dropZone.addEventListener(name, (event) => {
  event.preventDefault();
  dropZone.classList.add('over');
}));
['dragleave', 'drop'].forEach((name) => dropZone.addEventListener(name, () => dropZone.classList.remove('over')));
dropZone.addEventListener('drop', (event) => {
  event.preventDefault();
  const items = Array.from(event.dataTransfer.items || []).filter((item) => item.kind === 'file');
  const folders = items.filter((item) => item.webkitGetAsEntry?.()?.isDirectory);
  const files = items.filter((item) => !folders.includes(item)).map((item) => item.getAsFile()).filter(Boolean);
  if (folders.length > 0) i18n.msg(statusBox, () => t('receive.noFolders'));
  addFiles(files);
});
window.addEventListener('dragover', (event) => event.preventDefault());
window.addEventListener('drop', (event) => event.preventDefault());

window.addEventListener('beforeunload', (event) => {
  if (sending) event.preventDefault();
});

async function openLink() {
  try {
    const response = await fetch(`${linkBase}/info`);
    if (response.status === 429) {
      i18n.msg(statusBox, () => t('error.rate_limited'));
      return;
    }
    if (!response.ok) {
      showGone();
      return;
    }
    const { folder } = await response.json();
    i18n.msg(byId('receive-into'), () => t('receive.into', { folder }));
    card.hidden = false;
  } catch {
    i18n.msg(statusBox, () => t('error.network'));
  }
}

openLink();
