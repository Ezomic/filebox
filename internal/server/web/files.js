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
  if (!response.ok) throw i18n.failure(body, 'error.generic');
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

function showError(error) {
  if (error) i18n.report(statusBox, error);
  else i18n.msg(statusBox);
}

function join(parent, name) {
  return parent === '' ? name : `${parent}/${name}`;
}

function parentOf(path) {
  const index = path.lastIndexOf('/');
  return index < 0 ? '' : path.slice(0, index);
}

const formatSize = (bytes) => i18n.size(bytes);

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
  closeViewer();
  currentPath = pathFromHash();
  try {
    const { entries } = await api('GET', `/api/files?path=${encodeURIComponent(currentPath)}`);
    showError(null);
    renderCrumbs();
    render(entries);
  } catch (error) {
    if (error.message === 'signed out') return;
    showError(error);
    renderCrumbs();
    rows.replaceChildren();
    byId('empty').hidden = true;
  }
}

function renderCrumbs() {
  const crumbs = byId('crumbs');
  const parts = currentPath === '' ? [] : currentPath.split('/');
  const nodes = [button(t('nav.files'), () => go(''), 'link')];
  parts.forEach((part, index) => {
    nodes.push(el('span', '/', 'sep'));
    nodes.push(button(part, () => go(parts.slice(0, index + 1).join('/')), 'link'));
  });
  crumbs.replaceChildren(...nodes);
}

let previewable = [];
let shown = null;

function render(entries) {
  closeViewer();
  shown = entries;
  previewable = entries.filter((entry) => !entry.isDir && viewKind(entry.name) !== null);
  rows.replaceChildren(...entries.map(fileRow));
  byId('empty').hidden = entries.length > 0;
}

function openPreview(entry, opener) {
  const items = previewable.map((candidate) => viewItem(join(currentPath, candidate.name), candidate.name, candidate.size));
  openViewer(items, previewable.indexOf(entry), opener);
}

function fileRow(entry) {
  const path = join(currentPath, entry.name);
  const row = el('tr');
  const nameCell = el('td', undefined, 'name');
  if (entry.isDir) {
    nameCell.append(button(`${entry.name}/`, () => go(path), 'folder'));
  } else if (viewKind(entry.name) !== null) {
    nameCell.append(button(entry.name, (event) => openPreview(entry, event.currentTarget), 'folder file'));
  } else {
    nameCell.textContent = entry.name;
  }
  const actions = el('td', undefined, 'row-actions');
  row.append(
    nameCell,
    el('td', entry.isDir ? '' : formatSize(entry.size), 'meta'),
    el('td', i18n.dateTime(entry.modified), 'meta'),
    actions,
  );
  showActions(actions, entry, path);
  return row;
}

function showActions(cell, entry, path) {
  const nodes = [];
  if (entry.isDir) {
    const zipLink = el('a', t('files.downloadZip'), 'link');
    zipLink.href = `/api/files/zip?path=${encodeURIComponent(path)}`;
    zipLink.download = `${entry.name}.zip`;
    nodes.push(zipLink);
  } else {
    const link = el('a', t('common.download'), 'link');
    link.href = `/api/files/download?path=${encodeURIComponent(path)}`;
    link.download = entry.name;
    nodes.push(link);
  }
  nodes.push(
    button(t('files.rename'), () => showRenameForm(cell, entry, path), 'link'),
    button(t('common.delete'), () => deleteEntry(path), 'link danger'),
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
  input.setAttribute('aria-label', t('files.newNameFor', { name: entry.name }));

  const form = el('form', undefined, 'inline');
  const message = el('span', undefined, 'error');
  const save = el('button', t('common.save'));
  save.type = 'submit';
  form.append(input, save, button(t('common.cancel'), () => showActions(cell, entry, path), 'link'), message);

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    save.disabled = true;
    try {
      await api('POST', '/api/files/rename', { from: path, to: join(currentPath, input.value) });
      await load();
    } catch (error) {
      i18n.report(message, error);
      save.disabled = false;
    }
  });

  cell.replaceChildren(form);
  input.focus();
  input.select();
}

const UNDO_VISIBLE_MS = 30000;
const undoBar = el('p', undefined, 'notice');
const undoText = el('span');
let undoTimer = null;
let undoId;
const undoButton = i18n.label(button('', () => undoDelete(undoId), 'link'), 'common.undo');
undoBar.hidden = true;
statusBox.after(undoBar);

function hideUndo() {
  clearTimeout(undoTimer);
  undoBar.hidden = true;
}

function showUndo(producer, id) {
  clearTimeout(undoTimer);
  i18n.msg(undoText, producer);
  undoId = id;
  undoBar.replaceChildren(undoText);
  if (id !== undefined) undoBar.append(undoButton);
  undoBar.hidden = false;
  undoTimer = setTimeout(hideUndo, UNDO_VISIBLE_MS);
}

async function deleteEntry(path) {
  try {
    const { id } = await api('DELETE', `/api/files?path=${encodeURIComponent(path)}`);
    showError(null);
    showUndo(() => t('files.deleted'), id);
    await load();
    window.refreshStorage?.();
  } catch (error) {
    if (error.message !== 'signed out') showError(error);
  }
}

async function undoDelete(id) {
  try {
    const { path } = await api('POST', `/api/trash/${id}/restore`);
    showError(null);
    showUndo(() => t('files.restored', { path }));
    await load();
    window.refreshStorage?.();
  } catch (error) {
    if (error.message !== 'signed out') showError(error);
  }
}

byId('folder-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const errorBox = byId('folder-error');
  const input = byId('folder-name');
  const submit = event.target.querySelector('button[type="submit"]');
  i18n.msg(errorBox);
  submit.disabled = true;
  try {
    await api('POST', '/api/files/folder', { path: join(currentPath, input.value) });
    input.value = '';
    await load();
  } catch (error) {
    i18n.report(errorBox, error);
  } finally {
    submit.disabled = false;
  }
});

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

uploadHooks.note = (producer) => i18n.msg(uploadDetail, producer);

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
  const existing = await lookupExisting(items.map((item) => item.target));
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
    label.append(radio, ' ', i18n.label(el('span'), value === 'skip' ? 'conflict.skip' : 'conflict.replace'));
    group.append(label);
  });
  choices.set(item.target, 'skip');
  const row = el('li');
  row.append(el('span', item.target, 'conflict-name'), group);
  if (isDir) row.append(i18n.label(el('span', undefined, 'detail'), 'conflict.folderExists'));
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

    i18n.msg(byId('conflict-title'), () => t('conflict.title', { count: conflicts.length }));
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

function setProgress(item, position, count, fileBytes, doneBytes, totalBytes) {
  uploadProgress.max = Math.max(item.file.size, 1);
  uploadProgress.value = fileBytes;
  uploadTotal.max = Math.max(totalBytes, 1);
  uploadTotal.value = doneBytes + fileBytes;
  i18n.msg(uploadDetail, () => t('upload.progress', {
    name: item.target,
    position,
    total: count,
    sent: formatSize(fileBytes),
    size: formatSize(item.file.size),
    overallSent: formatSize(doneBytes + fileBytes),
    overallSize: formatSize(totalBytes),
  }));
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
  i18n.msg(uploadSummary, () => {
    const parts = [t('upload.done', { count: done })];
    if (skipped > 0) parts.push(t('upload.skipped', { count: skipped }));
    if (stopped) parts.push(stopped());
    return parts.join(' ');
  });
  i18n.msg(uploadDetail);
  const lines = failed.map(({ item, error }) => {
    const line = el('li', undefined, 'error');
    i18n.msg(line, () => `${item.target}: ${i18n.describe(error)}`);
    return line;
  });
  byId('upload-failures').replaceChildren(...lines);
}

async function decideConflicts(items) {
  const conflicts = await findConflicts(items);
  if (conflicts.length === 0) return;
  i18n.msg(uploadSummary, () => t('conflict.intro'));
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
  i18n.msg(uploadSummary, () => t('upload.checking'));
  byId('upload-failures').replaceChildren();
  const failed = [];
  let skipped = 0;
  let done = 0;
  let stopped = null;
  try {
    await decideConflicts(items);
    const queue = items.filter((item) => !item.skip);
    skipped = items.length - queue.length;
    await makeFolders(folderPaths(base, found));
    i18n.msg(uploadSummary, () => t('upload.uploading'));
    showProgress(true);
    const totalBytes = queue.reduce((sum, item) => sum + item.file.size, 0);
    let doneBytes = 0;
    for (const [index, item] of queue.entries()) {
      const report = (bytes) => setProgress(item, index + 1, queue.length, bytes, doneBytes, totalBytes);
      report(0);
      try {
        await sendItem(item, report);
        done += 1;
        doneBytes += item.file.size;
      } catch (error) {
        if (error.message === 'signed out') return;
        failed.push({ item, error });
        if (error instanceof DiskFullError) {
          stopped = () => t('upload.stoppedDisk');
          break;
        }
        if (error instanceof NetworkError) {
          stopped = () => t('upload.stoppedResume');
          break;
        }
      }
    }
  } catch (error) {
    if (error.message === 'signed out') return;
    stopped = error instanceof NetworkError ? () => t('upload.stoppedNetwork') : () => i18n.describe(error);
  } finally {
    setBusy(false);
    conflictBox.hidden = true;
  }
  summarize(done, skipped, failed, stopped);
  await load();
  window.refreshStorage?.();
}

async function startUpload(collect) {
  if (uploading) return;
  const base = currentPath;
  let found;
  try {
    found = await collect();
  } catch {
    i18n.msg(uploadSummary, () => t('upload.readFailed'));
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

function canThumbnail(file) {
  if (IMAGE_EXTENSIONS.has(extensionOf(file.name))) return true;
  return /^image\//.test(file.type) && !/hei[cf]|svg/.test(file.type);
}

function badge(name) {
  const label = extensionOf(name).slice(0, 4).toUpperCase();
  return el('span', label || t('preview.badge'), 'thumb');
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
  const where = base === '' ? t('preview.topFolder') : base;
  previewTitle.textContent = count === 0
    ? t('preview.emptyFolders', { count: found.dirs.length, where })
    : t('preview.files', { count, size: formatSize(bytes), where });
}

function relabelPreview() {
  updatePreviewTotals();
  for (const row of previewList.children) {
    row.sizeLabel.textContent = formatSize(row.entry.file.size);
    row.removeButton.textContent = t('preview.remove');
    row.removeButton.setAttribute('aria-label', t('preview.removeFile', { name: row.entry.rel }));
  }
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
  const remove = button(t('preview.remove'), () => {
    session.found.files = session.found.files.filter((candidate) => candidate !== entry);
    row.remove();
    if (session.found.files.length === 0 && session.found.dirs.length === 0) {
      closePreview();
      return;
    }
    updatePreviewTotals();
    previewUpload.focus();
  }, 'link');
  remove.setAttribute('aria-label', t('preview.removeFile', { name: entry.rel }));
  const sizeLabel = el('span', formatSize(entry.file.size), 'detail');
  row.append(holder, el('span', entry.rel, 'preview-name'), sizeLabel, remove);
  Object.assign(row, { entry, sizeLabel, removeButton: remove });
  return { row, job: withThumb && canThumbnail(entry.file) ? { file: entry.file, holder } : null };
}

function showPreview(found, base) {
  if (found.files.length === 0 && found.dirs.length === 0) return;
  closePreview();
  i18n.msg(uploadSummary);
  i18n.msg(uploadDetail);
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

i18n.onChange(() => {
  renderCrumbs();
  if (shown && !document.querySelector('#file-rows form')) render(shown);
  if (pending) relabelPreview();
});

window.addEventListener('hashchange', load);
load();
