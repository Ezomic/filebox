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

const formatSize = (bytes) => i18n.size(bytes);

function expiresIn(expires) {
  const days = Math.ceil((new Date(expires).getTime() - Date.now()) / DAY);
  return days <= 0 ? t('trash.expiresToday') : t('trash.expiresIn', { count: days });
}

function scope() {
  return userSelect && userSelect.value !== '' ? `?user=${encodeURIComponent(userSelect.value)}` : '';
}

function showError(error) {
  if (error) i18n.report(statusBox, error);
  else i18n.msg(statusBox);
}

function showNotice(producer) {
  if (producer) i18n.msg(notice, producer);
  else i18n.msg(notice);
  notice.hidden = !producer;
}

let listing = null;

function render(items, totalSize) {
  listing = { items, totalSize };
  if (items.length === 0) i18n.msg(summary);
  else i18n.msg(summary, () => t('trash.summary', { count: items.length, size: formatSize(totalSize) }));
  rows.replaceChildren(...items.map(trashRow));
  byId('empty').hidden = items.length > 0;
  emptyButton.hidden = items.length === 0;
  emptyConfirm.replaceChildren();
}

async function load() {
  try {
    const { items, totalSize } = await api('GET', `/api/trash${scope()}`);
    showError(null);
    render(items, totalSize);
    window.refreshStorage?.();
  } catch (error) {
    if (error.message === 'signed out') return;
    showError(error);
    listing = null;
    rows.replaceChildren();
    i18n.msg(summary);
    byId('empty').hidden = true;
    emptyButton.hidden = true;
  }
}

function trashRow(item) {
  const actions = el('td', undefined, 'row-actions');
  const nameCell = el('td', item.isDir ? `${item.name}/` : item.name, 'name');
  const folder = item.folder === '' ? t('nav.files') : `${t('nav.files')}/${item.folder}`;
  const where = el('td', t('trash.where', { folder, date: i18n.date(item.deleted), expires: expiresIn(item.expires) }), 'meta');
  const row = el('tr');
  row.append(nameCell, where, el('td', formatSize(item.size), 'meta'), actions);
  showActions(actions, item);
  return row;
}

function showActions(cell, item) {
  cell.replaceChildren(
    button(t('trash.restore'), () => restore(item), 'link'),
    button(t('trash.deleteForever'), () => confirmPurge(cell, item), 'link danger'),
  );
}

async function restore(item) {
  try {
    const { path } = await api('POST', `/api/trash/${item.id}/restore${scope()}`);
    showNotice(() => (path === item.path ? t('files.restored', { path }) : t('trash.restoredAs', { path })));
    await load();
  } catch (error) {
    if (error.message !== 'signed out') {
      i18n.msg(notice, () => i18n.describe(error));
      notice.hidden = false;
    }
  }
}

function confirmPurge(cell, item) {
  const message = el('span', undefined, 'error');
  cell.replaceChildren(
    el('span', t('trash.confirmDelete', { name: item.name }), 'error'),
    button(t('trash.deleteForever'), async () => {
      try {
        await api('DELETE', `/api/trash/${item.id}${scope()}`);
        showNotice(null);
        await load();
      } catch (error) {
        if (error.message !== 'signed out') i18n.report(message, error);
      }
    }, 'danger-solid'),
    button(t('common.cancel'), () => showActions(cell, item), 'link'),
    message,
  );
}

emptyButton.addEventListener('click', () => {
  const message = el('span', undefined, 'error');
  emptyButton.hidden = true;
  emptyConfirm.replaceChildren(
    el('span', t('trash.confirmEmpty'), 'error'),
    button(t('trash.emptyAll'), async () => {
      try {
        await api('DELETE', `/api/trash${scope()}`);
        showNotice(() => t('trash.emptied'));
        await load();
      } catch (error) {
        if (error.message !== 'signed out') i18n.report(message, error);
      }
    }, 'danger-solid'),
    button(t('common.cancel'), () => {
      emptyConfirm.replaceChildren();
      emptyButton.hidden = false;
    }, 'link'),
    message,
  );
});

async function loadUsers() {
  const { users } = await api('GET', '/api/users');
  users.forEach((user) => {
    const option = el('option', t('trash.userTrash', { name: user.name }));
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
    showNotice(null);
    load();
  });
  loadUsers().catch((error) => {
    if (error.message !== 'signed out') showError(error);
  });
}

i18n.onChange(() => {
  if (listing) render(listing.items, listing.totalSize);
});

load();
