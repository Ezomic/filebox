const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const notice = byId('notice');
const rows = byId('file-rows');
const userSelect = byId('trash-user');
const emptyButton = byId('empty-trash');
const emptyConfirm = byId('empty-confirm');
const summary = byId('summary');
const DAY = 24 * 60 * 60 * 1000;

async function api(method, path) {
  const response = await fetch(path, { method });
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

function expiresIn(expires) {
  const days = Math.ceil((new Date(expires).getTime() - Date.now()) / DAY);
  if (days <= 0) return 'expires today';
  return days === 1 ? 'expires in 1 day' : `expires in ${days} days`;
}

function scope() {
  return userSelect && userSelect.value !== '' ? `?user=${encodeURIComponent(userSelect.value)}` : '';
}

function showError(message) {
  statusBox.textContent = message;
}

function showNotice(message) {
  notice.textContent = message;
  notice.hidden = message === '';
}

async function load() {
  try {
    const { items, totalSize } = await api('GET', `/api/trash${scope()}`);
    showError('');
    summary.textContent = items.length === 0 ? '' : `${items.length === 1 ? '1 item' : `${items.length} items`} in the trash, ${formatSize(totalSize)} in total. Items are removed for good after 30 days.`;
    rows.replaceChildren(...items.map(trashRow));
    byId('empty').hidden = items.length > 0;
    emptyButton.hidden = items.length === 0;
    emptyConfirm.replaceChildren();
    window.refreshStorage?.();
  } catch (error) {
    if (error.message === 'signed out') return;
    showError(error.message);
    rows.replaceChildren();
    summary.textContent = '';
    byId('empty').hidden = true;
    emptyButton.hidden = true;
  }
}

function trashRow(item) {
  const actions = el('td', undefined, 'row-actions');
  const nameCell = el('td', item.isDir ? `${item.name}/` : item.name, 'name');
  const folder = item.folder === '' ? 'Files' : `Files/${item.folder}`;
  const where = el('td', `${folder} · deleted ${new Date(item.deleted).toLocaleDateString()} · ${expiresIn(item.expires)}`, 'meta');
  const row = el('tr');
  row.append(nameCell, where, el('td', formatSize(item.size), 'meta'), actions);
  showActions(actions, item);
  return row;
}

function showActions(cell, item) {
  cell.replaceChildren(
    button('Restore', () => restore(item), 'link'),
    button('Delete forever', () => confirmPurge(cell, item), 'link danger'),
  );
}

async function restore(item) {
  try {
    const { path } = await api('POST', `/api/trash/${item.id}/restore${scope()}`);
    showNotice(path === item.path ? `Restored ${path}.` : `Restored as ${path}.`);
    await load();
  } catch (error) {
    if (error.message !== 'signed out') showNotice(error.message);
  }
}

function confirmPurge(cell, item) {
  const message = el('span', undefined, 'error');
  cell.replaceChildren(
    el('span', `Delete ${item.name} for good?`, 'error'),
    button('Delete forever', async () => {
      try {
        await api('DELETE', `/api/trash/${item.id}${scope()}`);
        showNotice('');
        await load();
      } catch (error) {
        if (error.message !== 'signed out') message.textContent = error.message;
      }
    }, 'danger-solid'),
    button('Cancel', () => showActions(cell, item), 'link'),
    message,
  );
}

emptyButton.addEventListener('click', () => {
  const message = el('span', undefined, 'error');
  emptyButton.hidden = true;
  emptyConfirm.replaceChildren(
    el('span', 'This deletes everything in the trash for good.', 'error'),
    button('Empty trash', async () => {
      try {
        await api('DELETE', `/api/trash${scope()}`);
        showNotice('Emptied the trash.');
        await load();
      } catch (error) {
        if (error.message !== 'signed out') message.textContent = error.message;
      }
    }, 'danger-solid'),
    button('Cancel', () => {
      emptyConfirm.replaceChildren();
      emptyButton.hidden = false;
    }, 'link'),
    message,
  );
});

async function loadUsers() {
  const { users } = await api('GET', '/api/users');
  users.forEach((user) => {
    const option = el('option', `${user.name}'s trash`);
    option.value = user.name;
    userSelect.append(option);
  });
}

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

if (userSelect) {
  userSelect.addEventListener('change', () => {
    showNotice('');
    load();
  });
  loadUsers().catch((error) => {
    if (error.message !== 'signed out') showError(error.message);
  });
}
load();
