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
  if (!entry.isDir) {
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

const uploadStart = byId('upload-start');
const uploadProgress = byId('upload-progress');
const uploadDetail = byId('upload-detail');

function finishUpload(message) {
  uploadStart.disabled = false;
  uploadProgress.hidden = true;
  uploadDetail.textContent = message;
}

uploadStart.addEventListener('click', () => {
  const file = byId('upload-input').files[0];
  if (!file) {
    uploadDetail.textContent = 'Choose a file first.';
    return;
  }
  const overwrite = byId('upload-overwrite').checked ? '&overwrite=1' : '';
  const target = join(currentPath, file.name);

  const request = new XMLHttpRequest();
  request.open('PUT', `/api/files/content?path=${encodeURIComponent(target)}${overwrite}`);
  request.setRequestHeader('Content-Type', 'application/octet-stream');
  request.upload.addEventListener('progress', (event) => {
    if (!event.lengthComputable) return;
    uploadProgress.value = Math.round((event.loaded / event.total) * 100);
    uploadDetail.textContent = `${formatSize(event.loaded)} of ${formatSize(event.total)}`;
  });
  request.addEventListener('load', async () => {
    if (request.status === 401) {
      window.location.href = '/login';
      return;
    }
    if (request.status >= 200 && request.status < 300) {
      byId('upload-input').value = '';
      finishUpload(`Uploaded ${file.name}.`);
      await load();
      return;
    }
    let message = 'The upload failed.';
    try {
      message = JSON.parse(request.responseText).error || message;
    } catch {
      message = 'The upload failed.';
    }
    finishUpload(message);
  });
  request.addEventListener('error', () => finishUpload('The upload failed, the connection was lost.'));

  uploadStart.disabled = true;
  uploadProgress.value = 0;
  uploadProgress.hidden = false;
  uploadDetail.textContent = 'Starting upload...';
  request.send(file);
});

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

window.addEventListener('hashchange', load);
load();
