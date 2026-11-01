const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const rows = byId('file-rows');

let currentPath = '';

async function api(method, path, payload) {
  const options = { method };
  if (payload !== undefined) {
    options.headers = { 'Content-Type': 'application/json' };
    options.body = JSON.stringify(payload);
  }
  const response = await fetch(path, options);
  if (response.status === 401) {
    window.location.href = '/login';
    throw new Error('signed out');
  }
  const body = response.status === 204 ? {} : await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || 'Something went wrong.');
  return body;
}

function el(tag, text, className) {
  const node = document.createElement(tag);
  if (text !== undefined) node.textContent = text;
  if (className) node.className = className;
  return node;
}

function button(label, onClick, className) {
  const node = el('button', label, className);
  node.type = 'button';
  node.addEventListener('click', onClick);
  return node;
}

function showError(message) {
  statusBox.textContent = message;
}

function join(parent, name) {
  return parent === '' ? name : `${parent}/${name}`;
}

function parentOf(path) {
  const index = path.lastIndexOf('/');
  return index < 0 ? '' : path.slice(0, index);
}

function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes;
  let unit = -1;
  do {
    value /= 1024;
    unit += 1;
  } while (value >= 1024 && unit < units.length - 1);
  return `${value.toFixed(value < 10 ? 1 : 0)} ${units[unit]}`;
}

function pathFromHash() {
  try {
    return decodeURIComponent(window.location.hash.slice(1));
  } catch {
    return '';
  }
}

function go(path) {
  const hash = path === '' ? '' : `#${encodeURIComponent(path)}`;
  if (window.location.hash === hash || (hash === '' && window.location.hash === '')) {
    load();
  } else {
    window.location.hash = hash;
  }
}

async function load() {
  currentPath = pathFromHash();
  try {
    const { entries } = await api('GET', `/api/files?path=${encodeURIComponent(currentPath)}`);
    showError('');
    renderCrumbs();
    render(entries);
  } catch (error) {
    if (error.message === 'signed out') return;
    showError(error.message);
    renderCrumbs();
    rows.replaceChildren();
    byId('empty').hidden = true;
  }
}

function renderCrumbs() {
  const crumbs = byId('crumbs');
  const parts = currentPath === '' ? [] : currentPath.split('/');
  const nodes = [button('Files', () => go(''), 'link')];
  parts.forEach((part, index) => {
    nodes.push(el('span', '/', 'sep'));
    nodes.push(button(part, () => go(parts.slice(0, index + 1).join('/')), 'link'));
  });
  crumbs.replaceChildren(...nodes);
}

function render(entries) {
  rows.replaceChildren(...entries.map(fileRow));
  byId('empty').hidden = entries.length > 0;
}

function fileRow(entry) {
  const path = join(currentPath, entry.name);
  const row = el('tr');
  const nameCell = el('td', undefined, 'name');
  if (entry.isDir) {
    nameCell.append(button(`${entry.name}/`, () => go(path), 'folder'));
  } else {
    nameCell.textContent = entry.name;
  }
  const actions = el('td', undefined, 'row-actions');
  row.append(
    nameCell,
    el('td', entry.isDir ? '' : formatSize(entry.size), 'meta'),
    el('td', new Date(entry.modified).toLocaleString(), 'meta'),
    actions,
  );
  showActions(actions, entry, path);
  return row;
}

function showActions(cell, entry, path) {
  const nodes = [];
  if (entry.isDir) {
    const zipLink = el('a', 'Download zip', 'link');
    zipLink.href = `/api/files/zip?path=${encodeURIComponent(path)}`;
    zipLink.download = `${entry.name}.zip`;
    nodes.push(zipLink);
  } else {
    const link = el('a', 'Download', 'link');
    link.href = `/api/files/download?path=${encodeURIComponent(path)}`;
    link.download = entry.name;
    nodes.push(link);
  }
  nodes.push(
    button('Rename', () => showRenameForm(cell, entry, path), 'link'),
    button('Delete', () => showDeleteConfirm(cell, entry, path), 'link danger'),
  );
  cell.replaceChildren(...nodes);
}

function showRenameForm(cell, entry, path) {
  const input = el('input');
  input.type = 'text';
  input.value = entry.name;
  input.maxLength = 255;
  input.required = true;
  input.spellcheck = false;
  input.setAttribute('aria-label', `New name for ${entry.name}`);

  const form = el('form', undefined, 'inline');
  const message = el('span', undefined, 'error');
  const save = el('button', 'Save');
  save.type = 'submit';
  form.append(input, save, button('Cancel', () => showActions(cell, entry, path), 'link'), message);

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    save.disabled = true;
    try {
      await api('POST', '/api/files/rename', { from: path, to: join(currentPath, input.value) });
      await load();
    } catch (error) {
      message.textContent = error.message;
      save.disabled = false;
    }
  });

  cell.replaceChildren(form);
  input.focus();
  input.select();
}

function showDeleteConfirm(cell, entry, path) {
  const message = el('span', undefined, 'error');
  const what = entry.isDir ? `the folder ${entry.name}` : entry.name;
  cell.replaceChildren(
    el('span', `This deletes ${what} for good.`, 'error'),
    button('Delete', async () => {
      try {
        await api('DELETE', `/api/files?path=${encodeURIComponent(path)}`);
        await load();
      } catch (error) {
        message.textContent = error.message;
      }
    }, 'danger-solid'),
    button('Cancel', () => showActions(cell, entry, path), 'link'),
    message,
  );
}

byId('folder-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const errorBox = byId('folder-error');
  const input = byId('folder-name');
  const submit = event.target.querySelector('button[type="submit"]');
  errorBox.textContent = '';
  submit.disabled = true;
  try {
    await api('POST', '/api/files/folder', { path: join(currentPath, input.value) });
    input.value = '';
    await load();
  } catch (error) {
    errorBox.textContent = error.message;
  } finally {
    submit.disabled = false;
  }
});

const CHUNK_SIZE = 8 * 1024 * 1024;
const SINGLE_REQUEST_LIMIT = 8 * 1024 * 1024;
const EXISTS_BATCH = 500;
const MAX_RETRIES = 10;
const RESUME_PREFIX = 'filebox-upload:';

const fileInput = byId('upload-input');
const folderInput = byId('upload-folder');
const dropZone = byId('drop-zone');
const uploadTotal = byId('upload-total');
const uploadProgress = byId('upload-progress');
const uploadDetail = byId('upload-detail');
const uploadSummary = byId('upload-summary');
const conflictBox = byId('conflicts');
const conflictList = byId('conflict-list');

let uploading = false;

class NetworkError extends Error {}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function call(method, path, payload) {
  const options = { method };
  if (payload !== undefined) {
    options.headers = { 'Content-Type': 'application/json' };
    options.body = JSON.stringify(payload);
  }
  let response;
  try {
    response = await fetch(path, options);
  } catch {
    throw new NetworkError('The connection was lost.');
  }
  if (response.status === 401) {
    window.location.href = '/login';
    throw new Error('signed out');
  }
  const body = response.status === 204 ? {} : await response.json().catch(() => ({}));
  return { status: response.status, body };
}

function sendBlob(url, blob, headers, onProgress) {
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    request.open('PUT', url);
    request.setRequestHeader('Content-Type', 'application/octet-stream');
    Object.entries(headers).forEach(([name, value]) => request.setRequestHeader(name, value));
    request.upload.addEventListener('progress', (event) => {
      if (event.lengthComputable) onProgress(event.loaded);
    });
    request.addEventListener('load', () => {
      if (request.status === 401) {
        window.location.href = '/login';
        reject(new Error('signed out'));
        return;
      }
      let body = {};
      try {
        body = JSON.parse(request.responseText);
      } catch {
        body = {};
      }
      resolve({ status: request.status, body });
    });
    request.addEventListener('error', () => reject(new NetworkError('The connection was lost.')));
    request.addEventListener('abort', () => reject(new NetworkError('The connection was lost.')));
    request.send(blob);
  });
}

async function withRetry(task) {
  for (let attempt = 0; ; attempt += 1) {
    try {
      return await task();
    } catch (error) {
      if (!(error instanceof NetworkError) || attempt >= MAX_RETRIES) throw error;
      uploadDetail.textContent = `Connection lost, trying again (${attempt + 1} of ${MAX_RETRIES})...`;
      await sleep(Math.min(1000 * 2 ** attempt, 15000));
    }
  }
}

function resumeKey(item) {
  return `${RESUME_PREFIX}${item.target}|${item.file.size}|${item.file.lastModified}`;
}

function readSaved(key) {
  try {
    return JSON.parse(localStorage.getItem(key));
  } catch {
    return null;
  }
}

function saveSession(key, value) {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch {
    return;
  }
}

function forgetSession(key) {
  try {
    localStorage.removeItem(key);
  } catch {
    return;
  }
}

async function readEntries(reader) {
  const all = [];
  for (;;) {
    const batch = await new Promise((resolve, reject) => reader.readEntries(resolve, reject));
    if (batch.length === 0) return all;
    all.push(...batch);
  }
}

function entryFile(entry) {
  return new Promise((resolve, reject) => entry.file(resolve, reject));
}

async function walkEntry(entry, prefix, found) {
  const rel = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
  if (entry.isFile) {
    found.files.push({ file: await entryFile(entry), rel });
    return;
  }
  found.dirs.push(rel);
  for (const child of await readEntries(entry.createReader())) {
    await walkEntry(child, rel, found);
  }
}

async function collectDropped(entries) {
  const found = { files: [], dirs: [] };
  for (const entry of entries) {
    await walkEntry(entry, '', found);
  }
  return found;
}

function collectPicked(fileList) {
  const found = { files: [], dirs: [] };
  Array.from(fileList).forEach((file) => {
    found.files.push({ file, rel: file.webkitRelativePath || file.name });
  });
  return found;
}

function folderPaths(base, found) {
  const dirs = new Set(found.dirs.map((rel) => join(base, rel)));
  found.files.forEach(({ rel }) => {
    const parts = rel.split('/').slice(0, -1);
    parts.forEach((_, index) => dirs.add(join(base, parts.slice(0, index + 1).join('/'))));
  });
  return Array.from(dirs).sort((a, b) => a.split('/').length - b.split('/').length || a.localeCompare(b));
}

async function findConflicts(items) {
  const existing = new Map();
  for (let start = 0; start < items.length; start += EXISTS_BATCH) {
    const paths = items.slice(start, start + EXISTS_BATCH).map((item) => item.target);
    const { status, body } = await withRetry(() => call('POST', '/api/files/exists', { paths }));
    if (status !== 200) throw new Error(body.error || 'Could not check for existing files.');
    body.existing.forEach((entry) => existing.set(entry.path, entry));
  }
  return items.filter((item) => existing.has(item.target)).map((item) => ({ item, isDir: existing.get(item.target).isDir }));
}

function conflictRow(item, isDir, choices) {
  const group = el('span', undefined, 'conflict-choice');
  const radios = {};
  ['skip', 'replace'].forEach((value) => {
    const label = el('label', undefined, 'check');
    const radio = document.createElement('input');
    radio.type = 'radio';
    radio.name = `conflict-${item.target}`;
    radio.value = value;
    radio.checked = value === 'skip';
    radio.disabled = isDir && value === 'replace';
    radio.addEventListener('change', () => choices.set(item.target, value));
    radios[value] = radio;
    label.append(radio, value === 'skip' ? ' Skip' : ' Replace');
    group.append(label);
  });
  choices.set(item.target, 'skip');
  const row = el('li');
  row.append(el('span', item.target, 'conflict-name'), group);
  if (isDir) row.append(el('span', 'A folder with this name exists.', 'detail'));
  row.radios = radios;
  return row;
}

function askAboutConflicts(conflicts) {
  return new Promise((resolve) => {
    const choices = new Map();
    const rowsToShow = conflicts.map(({ item, isDir }) => conflictRow(item, isDir, choices));
    const setAll = (value) => rowsToShow.forEach((row) => {
      if (row.radios[value].disabled) return;
      row.radios[value].checked = true;
      row.radios[value].dispatchEvent(new Event('change'));
    });

    byId('conflict-title').textContent = `${conflicts.length} ${conflicts.length === 1 ? 'file already exists' : 'files already exist'}`;
    byId('conflict-skip-all').onclick = () => setAll('skip');
    byId('conflict-replace-all').onclick = () => setAll('replace');
    byId('conflict-continue').onclick = () => {
      conflictBox.hidden = true;
      conflictList.replaceChildren();
      resolve(choices);
    };
    conflictList.replaceChildren(...rowsToShow);
    conflictBox.hidden = false;
    byId('conflict-continue').focus();
  });
}

async function makeFolders(paths) {
  for (const path of paths) {
    const { status, body } = await withRetry(() => call('POST', '/api/files/folder', { path }));
    if (status !== 201 && status !== 409) throw new Error(body.error || `Could not create the folder ${path}.`);
  }
}

async function sendSmallFile(item, report) {
  const overwrite = item.overwrite ? '&overwrite=1' : '';
  const url = `/api/files/content?path=${encodeURIComponent(item.target)}${overwrite}`;
  const { status, body } = await withRetry(() => sendBlob(url, item.file, {}, report));
  if (status !== 204) throw new Error(body.error || 'The upload failed.');
}

async function resumeSession(item, key) {
  const saved = readSaved(key);
  if (!saved || typeof saved.id !== 'string' || saved.overwrite !== item.overwrite) return null;
  const { status, body } = await withRetry(() => call('GET', `/api/uploads/${encodeURIComponent(saved.id)}`));
  if (status === 200 && body.path === item.target && body.size === item.file.size) {
    return { id: saved.id, offset: body.offset, resumed: body.offset > 0 };
  }
  forgetSession(key);
  return null;
}

async function createSession(item, key) {
  const payload = { path: item.target, size: item.file.size, overwrite: item.overwrite };
  const { status, body } = await withRetry(() => call('POST', '/api/uploads', payload));
  if (status !== 201) throw new Error(body.error || 'The upload could not be started.');
  saveSession(key, { id: body.id, overwrite: item.overwrite });
  return { id: body.id, offset: 0, resumed: false };
}

function sendChunk(item, session, from, report) {
  const size = item.file.size;
  const end = Math.min(from + CHUNK_SIZE, size);
  const headers = { 'Content-Range': `bytes ${from}-${end - 1}/${size}` };
  return withRetry(() => sendBlob(`/api/uploads/${session.id}`, item.file.slice(from, end), headers, (loaded) => report(from + loaded)));
}

async function sendLargeFile(item, report) {
  const key = resumeKey(item);
  const size = item.file.size;
  let session = (await resumeSession(item, key)) || (await createSession(item, key));
  let restarted = false;
  let stalls = 0;
  let offset = session.offset;
  if (session.resumed) uploadDetail.textContent = `Resuming at ${formatSize(offset)}...`;

  for (let finishTries = 0; finishTries < 3; finishTries += 1) {
    while (offset < size) {
      const from = offset;
      const { status, body } = await sendChunk(item, session, from, report);
      if (status === 200) {
        offset = body.offset;
        stalls = 0;
      } else if ((status === 409 || status === 400) && typeof body.offset === 'number') {
        stalls = body.offset === from ? stalls + 1 : 0;
        if (stalls >= 3) throw new Error(body.error || 'The upload failed.');
        offset = body.offset;
      } else if (status === 404 && !restarted) {
        restarted = true;
        forgetSession(key);
        session = await createSession(item, key);
        offset = 0;
      } else {
        throw new Error(body.error || 'The upload failed.');
      }
      report(offset);
    }

    const { status, body } = await withRetry(() => call('POST', `/api/uploads/${session.id}/finish`));
    if (status === 204 || (status === 404 && await alreadyThere(item))) {
      forgetSession(key);
      return;
    }
    if (status !== 409 || typeof body.offset !== 'number') throw new Error(body.error || 'The upload could not be completed.');
    offset = body.offset;
  }
  throw new Error('The upload could not be completed.');
}

async function alreadyThere(item) {
  const { status, body } = await withRetry(() => call('POST', '/api/files/exists', { paths: [item.target] }));
  return status === 200 && body.existing.some((entry) => entry.path === item.target && !entry.isDir);
}

function setProgress(item, position, count, fileBytes, doneBytes, totalBytes) {
  uploadProgress.max = Math.max(item.file.size, 1);
  uploadProgress.value = fileBytes;
  uploadTotal.max = Math.max(totalBytes, 1);
  uploadTotal.value = doneBytes + fileBytes;
  uploadDetail.textContent = `${item.target} (${position} of ${count}): ${formatSize(fileBytes)} of ${formatSize(item.file.size)}. Overall ${formatSize(doneBytes + fileBytes)} of ${formatSize(totalBytes)}.`;
}

function setBusy(busy) {
  uploading = busy;
  fileInput.disabled = busy;
  folderInput.disabled = busy;
  dropZone.classList.toggle('busy', busy);
  if (!busy) showProgress(false);
}

function showProgress(visible) {
  uploadProgress.hidden = !visible;
  uploadTotal.hidden = !visible;
}

function summarize(done, skipped, failed, stopped) {
  const parts = [`Uploaded ${done} ${done === 1 ? 'file' : 'files'}.`];
  if (skipped > 0) parts.push(`Skipped ${skipped}.`);
  if (stopped) parts.push(stopped);
  uploadSummary.textContent = parts.join(' ');
  uploadDetail.textContent = '';
  byId('upload-failures').replaceChildren(...failed.map(({ item, message }) => el('li', `${item.target}: ${message}`, 'error')));
}

async function decideConflicts(items) {
  const conflicts = await findConflicts(items);
  if (conflicts.length === 0) return;
  uploadSummary.textContent = 'Some files already exist. Choose what to do with each.';
  const choices = await askAboutConflicts(conflicts);
  conflicts.forEach(({ item }) => {
    item.overwrite = choices.get(item.target) === 'replace';
    item.skip = !item.overwrite;
  });
}

async function runUploads(found, base) {
  const items = found.files.map(({ file, rel }) => ({ file, target: join(base, rel), overwrite: false, skip: false }));
  if (items.length === 0 && found.dirs.length === 0) return;
  setBusy(true);
  uploadSummary.textContent = 'Checking for files that already exist...';
  byId('upload-failures').replaceChildren();
  const failed = [];
  let skipped = 0;
  let done = 0;
  let stopped = '';
  try {
    await decideConflicts(items);
    const queue = items.filter((item) => !item.skip);
    skipped = items.length - queue.length;
    await makeFolders(folderPaths(base, found));
    uploadSummary.textContent = 'Uploading...';
    showProgress(true);
    const totalBytes = queue.reduce((sum, item) => sum + item.file.size, 0);
    let doneBytes = 0;
    for (const [index, item] of queue.entries()) {
      const report = (bytes) => setProgress(item, index + 1, queue.length, bytes, doneBytes, totalBytes);
      report(0);
      try {
        if (item.file.size > SINGLE_REQUEST_LIMIT) await sendLargeFile(item, report);
        else await sendSmallFile(item, report);
        done += 1;
        doneBytes += item.file.size;
      } catch (error) {
        if (error.message === 'signed out') return;
        failed.push({ item, message: error.message });
        if (error instanceof NetworkError) {
          stopped = 'Stopped because the connection was lost. Choose the same files again to continue where they stopped.';
          break;
        }
      }
    }
  } catch (error) {
    if (error.message === 'signed out') return;
    stopped = error instanceof NetworkError ? 'Stopped because the connection was lost.' : error.message;
  } finally {
    setBusy(false);
    conflictBox.hidden = true;
  }
  summarize(done, skipped, failed, stopped);
  await load();
}

async function startUpload(collect) {
  if (uploading) return;
  const base = currentPath;
  let found;
  try {
    found = await collect();
  } catch {
    uploadSummary.textContent = 'Could not read what you selected.';
    return;
  }
  if (uploading) return;
  showPreview(found, base);
}

const PREVIEW_THUMBS = 200;
const THUMB_SIZE = 96;
const IMAGE_EXTENSIONS = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'bmp']);

const previewBox = byId('preview');
const previewList = byId('preview-list');
const previewTitle = byId('preview-title');
const previewUpload = byId('preview-upload');

let pending = null;

function extensionOf(name) {
  const dot = name.lastIndexOf('.');
  return dot <= 0 ? '' : name.slice(dot + 1).toLowerCase();
}

function canThumbnail(file) {
  if (IMAGE_EXTENSIONS.has(extensionOf(file.name))) return true;
  return /^image\//.test(file.type) && !/hei[cf]|svg/.test(file.type);
}

function badge(name) {
  const label = extensionOf(name).slice(0, 4).toUpperCase();
  return el('span', label || 'FILE', 'thumb');
}

async function drawThumbnail(file) {
  const bitmap = await createImageBitmap(file, { resizeWidth: THUMB_SIZE, resizeQuality: 'medium' });
  try {
    const canvas = document.createElement('canvas');
    canvas.width = THUMB_SIZE;
    canvas.height = THUMB_SIZE;
    canvas.className = 'thumb';
    const scale = Math.max(THUMB_SIZE / bitmap.width, THUMB_SIZE / bitmap.height);
    const width = bitmap.width * scale;
    const height = bitmap.height * scale;
    canvas.getContext('2d').drawImage(bitmap, (THUMB_SIZE - width) / 2, (THUMB_SIZE - height) / 2, width, height);
    return canvas;
  } finally {
    bitmap.close();
  }
}

async function fillThumbnails(session, jobs) {
  for (const { file, holder } of jobs) {
    if (pending !== session) return;
    if (!holder.isConnected) continue;
    try {
      const canvas = await drawThumbnail(file);
      if (pending !== session) return;
      holder.replaceChildren(canvas);
    } catch {
      continue;
    }
  }
}

function updatePreviewTotals() {
  const { found, base } = pending;
  const count = found.files.length;
  const bytes = found.files.reduce((sum, { file }) => sum + file.size, 0);
  const where = base === '' ? 'the top folder' : base;
  previewTitle.textContent = count === 0
    ? `${found.dirs.length} empty ${found.dirs.length === 1 ? 'folder' : 'folders'} to create in ${where}`
    : `${count} ${count === 1 ? 'file' : 'files'}, ${formatSize(bytes)}, to upload to ${where}`;
}

function closePreview() {
  pending = null;
  previewBox.hidden = true;
  previewList.replaceChildren();
}

function previewRow(session, entry, withThumb) {
  const holder = el('span', undefined, 'thumb-holder');
  holder.append(badge(entry.file.name));
  const row = el('li');
  const remove = button('Remove', () => {
    session.found.files = session.found.files.filter((candidate) => candidate !== entry);
    row.remove();
    if (session.found.files.length === 0 && session.found.dirs.length === 0) {
      closePreview();
      return;
    }
    updatePreviewTotals();
    previewUpload.focus();
  }, 'link');
  remove.setAttribute('aria-label', `Remove ${entry.rel}`);
  row.append(holder, el('span', entry.rel, 'preview-name'), el('span', formatSize(entry.file.size), 'detail'), remove);
  return { row, job: withThumb && canThumbnail(entry.file) ? { file: entry.file, holder } : null };
}

function showPreview(found, base) {
  if (found.files.length === 0 && found.dirs.length === 0) return;
  closePreview();
  uploadSummary.textContent = '';
  uploadDetail.textContent = '';
  byId('upload-failures').replaceChildren();
  const session = { found, base };
  pending = session;
  const built = found.files.map((entry, index) => previewRow(session, entry, index < PREVIEW_THUMBS));
  previewList.replaceChildren(...built.map(({ row }) => row));
  updatePreviewTotals();
  previewBox.hidden = false;
  previewUpload.focus();
  fillThumbnails(session, built.map(({ job }) => job).filter(Boolean));
}

previewUpload.addEventListener('click', () => {
  if (!pending || uploading) return;
  const { found, base } = pending;
  closePreview();
  runUploads(found, base);
});

byId('preview-cancel').addEventListener('click', closePreview);

fileInput.addEventListener('change', () => {
  const picked = collectPicked(fileInput.files);
  fileInput.value = '';
  startUpload(async () => picked);
});

folderInput.addEventListener('change', () => {
  const picked = collectPicked(folderInput.files);
  folderInput.value = '';
  startUpload(async () => picked);
});

['dragenter', 'dragover'].forEach((name) => dropZone.addEventListener(name, (event) => {
  event.preventDefault();
  if (!uploading) dropZone.classList.add('over');
}));
['dragleave', 'drop'].forEach((name) => dropZone.addEventListener(name, () => dropZone.classList.remove('over')));
dropZone.addEventListener('drop', (event) => {
  event.preventDefault();
  const entries = Array.from(event.dataTransfer.items || [])
    .filter((item) => item.kind === 'file')
    .map((item) => item.webkitGetAsEntry())
    .filter(Boolean);
  startUpload(() => collectDropped(entries));
});
window.addEventListener('dragover', (event) => event.preventDefault());
window.addEventListener('drop', (event) => event.preventDefault());

window.addEventListener('beforeunload', (event) => {
  if (uploading) event.preventDefault();
});

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

window.addEventListener('hashchange', load);
load();
