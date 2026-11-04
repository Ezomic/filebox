const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const notice = byId('notice');
const rows = byId('share-rows');

let shares = [];

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

function showNotice(producer) {
  if (producer) i18n.msg(notice, producer);
  else i18n.msg(notice);
  notice.hidden = !producer;
}

function usageText(share) {
  const used = t('shares.downloads', { count: share.downloads });
  return share.lastUsed === null ? used : `${used}, ${t('shares.lastUsed', { when: i18n.dateTime(share.lastUsed) })}`;
}

function shareRow(share) {
  const row = el('tr');
  const name = el('td', undefined, 'name');
  name.append(el('div', `/${share.name}`));
  name.append(el('div', `${share.dir ? t('shares.folder') : t('shares.file')}: ${share.path}`, 'detail'));
  const meta = el('td', undefined, 'meta');
  meta.append(el('div', shareExpiryText(share), share.expired ? 'error' : 'detail'));
  meta.append(el('div', usageText(share), 'detail'));
  const actions = el('td', undefined, 'row-actions');
  row.append(name, meta, actions);
  showActions(actions, share);
  return row;
}

function showActions(cell, share) {
  cell.replaceChildren(
    button(t('shares.copy'), () => copyShareUrl(shareUrl(share.name), showNotice), 'link'),
    button(t('shares.changeExpiry'), () => showExpiryForm(cell, share), 'link'),
    button(t('shares.revoke'), () => showRevokeForm(cell, share), 'link danger'),
  );
}

function showExpiryForm(cell, share) {
  const select = expirySelect(t('shares.expiryLabel'));
  const form = el('form', undefined, 'inline');
  const message = el('span', undefined, 'error');
  const save = el('button', t('common.save'));
  save.type = 'submit';
  form.append(select, save, button(t('common.cancel'), () => showActions(cell, share), 'link'), message);
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    save.disabled = true;
    try {
      await api('PUT', `/api/shares/${encodeURIComponent(share.name)}`, { expires: select.value });
      await load();
    } catch (error) {
      i18n.report(message, error);
      save.disabled = false;
    }
  });
  cell.replaceChildren(form);
  select.focus();
}

function showRevokeForm(cell, share) {
  const form = el('form', undefined, 'inline');
  const message = el('span', undefined, 'error');
  const confirm = el('button', t('shares.revoke'), 'danger-solid');
  confirm.type = 'submit';
  form.append(el('span', t('shares.revokeNotice', { name: share.name }), 'error'), confirm, button(t('common.cancel'), () => showActions(cell, share), 'link'), message);
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    confirm.disabled = true;
    try {
      await api('DELETE', `/api/shares/${encodeURIComponent(share.name)}`);
      showNotice(() => t('shares.revoked', { name: share.name }));
      await load();
    } catch (error) {
      i18n.report(message, error);
      confirm.disabled = false;
    }
  });
  cell.replaceChildren(form);
  confirm.focus();
}

function render() {
  rows.replaceChildren(...shares.map(shareRow));
  byId('empty').hidden = shares.length > 0;
}

async function load() {
  try {
    ({ shares } = await api('GET', '/api/shares'));
    i18n.msg(statusBox);
    render();
  } catch (error) {
    if (error.message !== 'signed out') i18n.report(statusBox, error);
  }
}

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

i18n.onChange(() => {
  if (!rows.querySelector('form')) render();
});

load();
