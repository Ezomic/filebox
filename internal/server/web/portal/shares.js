const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const notice = byId('notice');
const rows = byId('share-rows');
const uploadRows = byId('upload-rows');
const uploadForm = byId('upload-form');
const uploadStart = byId('upload-start');
const UPLOAD_EXPIRY_CHOICES = EXPIRY_CHOICES.filter(([value]) => value !== 'never');
const CAP_CHOICES = [
  ['1g', 'uploadLinks.cap1g'],
  ['5g', 'uploadLinks.cap5g'],
  ['20g', 'uploadLinks.cap20g'],
];

let shares = [];
let uploadLinks = [];
let pickerPath = '';
let pickerChoice = { expires: '7d', cap: '5g' };

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

function showRevokeForm(cell, share, back = showActions) {
  const form = el('form', undefined, 'inline');
  const message = el('span', undefined, 'error');
  const confirm = el('button', t('shares.revoke'), 'danger-solid');
  confirm.type = 'submit';
  form.append(el('span', t('shares.revokeNotice', { name: share.name }), 'error'), confirm, button(t('common.cancel'), () => back(cell, share), 'link'), message);
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

function uploadRow(link) {
  const row = el('tr');
  const name = el('td', undefined, 'name');
  name.append(el('div', `/${link.name}`));
  name.append(el('div', `${t('shares.folder')}: ${link.path}`, 'detail'));
  const meta = el('td', undefined, 'meta');
  meta.append(el('div', shareExpiryText(link), link.expired ? 'error' : 'detail'));
  meta.append(el('div', uploadUsageText(link), 'detail'));
  const actions = el('td', undefined, 'row-actions');
  row.append(name, meta, actions);
  showUploadActions(actions, link);
  return row;
}

function showUploadActions(cell, link) {
  cell.replaceChildren(
    button(t('shares.copy'), () => copyShareUrl(shareUrl(link.name), showNotice), 'link'),
    button(t('shares.revoke'), () => showRevokeForm(cell, link, showUploadActions), 'link danger'),
  );
}

const join = (parent, name) => (parent === '' ? name : `${parent}/${name}`);

function labelled(node, key) {
  node.setAttribute('data-i18n-attr', `aria-label:${key}`);
  node.setAttribute('aria-label', t(key));
  return node;
}

function choiceSelect(choices, labelKey, chosen, remember) {
  const select = labelled(document.createElement('select'), labelKey);
  choices.forEach(([value, key]) => {
    const option = new Option(t(key), value);
    option.setAttribute('data-i18n', key);
    select.append(option);
  });
  select.value = chosen;
  select.addEventListener('change', () => remember(select.value));
  return select;
}

function closePicker() {
  uploadForm.replaceChildren();
  uploadForm.hidden = true;
  uploadStart.hidden = false;
}

async function openPicker(path) {
  try {
    const { entries } = await api('GET', `/api/files?path=${encodeURIComponent(path)}`);
    pickerPath = path;
    renderPicker(entries.filter((entry) => entry.isDir));
  } catch (error) {
    if (error.message !== 'signed out') i18n.report(statusBox, error);
  }
}

function renderPicker(folders) {
  const parts = pickerPath === '' ? [] : pickerPath.split('/');
  const crumbs = el('nav', undefined, 'crumbs');
  crumbs.append(i18n.label(button('', () => openPicker(''), 'link'), 'nav.files'));
  parts.forEach((part, index) => {
    crumbs.append(el('span', '/', 'sep'), button(part, () => openPicker(parts.slice(0, index + 1).join('/')), 'link'));
  });
  const list = el('div', undefined, 'crumbs');
  folders.forEach((folder) => list.append(button(`${folder.name}/`, () => openPicker(join(pickerPath, folder.name)), 'folder')));
  if (folders.length === 0) list.append(i18n.label(el('span', undefined, 'detail'), 'uploadLinks.noFolders'));

  uploadForm.replaceChildren(
    i18n.label(el('div', undefined, 'label'), 'uploadLinks.into'),
    crumbs,
    i18n.label(el('p', undefined, 'detail'), 'uploadLinks.pickFolder'),
    list,
    makeFolderForm(),
    createLinkForm(),
  );
  uploadForm.hidden = false;
  uploadStart.hidden = true;
}

function makeFolderForm() {
  const name = el('input');
  name.type = 'text';
  name.maxLength = 255;
  name.required = true;
  name.spellcheck = false;
  name.setAttribute('data-i18n-attr', 'aria-label:uploadLinks.newFolder,placeholder:uploadLinks.newFolder');
  name.setAttribute('aria-label', t('uploadLinks.newFolder'));
  name.placeholder = t('uploadLinks.newFolder');
  const make = i18n.label(el('button'), 'uploadLinks.makeFolder');
  make.type = 'submit';
  const message = el('span', undefined, 'error');
  const form = el('form', undefined, 'inline');
  form.append(name, make, message);
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    make.disabled = true;
    const path = join(pickerPath, name.value.trim());
    try {
      await api('POST', '/api/files/folder', { path });
      await openPicker(path);
    } catch (error) {
      i18n.report(message, error);
      make.disabled = false;
    }
  });
  return form;
}

function createLinkForm() {
  const expiry = choiceSelect(UPLOAD_EXPIRY_CHOICES, 'shares.expiryLabel', pickerChoice.expires, (value) => { pickerChoice.expires = value; });
  const cap = choiceSelect(CAP_CHOICES, 'uploadLinks.capLabel', pickerChoice.cap, (value) => { pickerChoice.cap = value; });
  const create = i18n.label(el('button'), 'uploadLinks.create');
  create.type = 'submit';
  create.disabled = pickerPath === '';
  const message = el('span', undefined, pickerPath === '' ? 'detail' : 'error');
  if (pickerPath === '') i18n.msg(message, () => t('uploadLinks.chooseFolder'));
  const form = el('form', undefined, 'inline');
  form.append(expiry, cap, create, i18n.label(button('', closePicker, 'link'), 'common.cancel'), message);
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    create.disabled = true;
    try {
      const link = await api('POST', '/api/upload-links', { path: pickerPath, expires: expiry.value, cap: cap.value, lang: i18n.lang() });
      showCreatedLink(link);
      await load();
    } catch (error) {
      message.className = 'error';
      i18n.report(message, error);
      create.disabled = false;
    }
  });
  return form;
}

function showCreatedLink(link) {
  const url = shareUrl(link.name);
  const field = labelled(el('input'), 'files.shareLink');
  field.type = 'text';
  field.readOnly = true;
  field.value = url;
  field.addEventListener('focus', () => field.select());
  const message = el('span', undefined, 'detail');
  const form = el('form', undefined, 'inline');
  form.append(
    field,
    i18n.label(button('', () => copyShareUrl(url, (producer) => i18n.msg(message, producer)), 'link'), 'shares.copy'),
    i18n.label(button('', closePicker, 'link'), 'files.shareDone'),
    message,
  );
  form.addEventListener('submit', (event) => event.preventDefault());
  uploadForm.replaceChildren(i18n.label(el('p', undefined, 'detail'), 'uploadLinks.created'), form);
  field.select();
}

function render() {
  rows.replaceChildren(...shares.map(shareRow));
  byId('empty').hidden = shares.length > 0;
  uploadRows.replaceChildren(...uploadLinks.map(uploadRow));
  byId('upload-empty').hidden = uploadLinks.length > 0;
}

async function load() {
  try {
    ({ shares, uploadLinks } = await api('GET', '/api/shares'));
    i18n.msg(statusBox);
    render();
  } catch (error) {
    if (error.message !== 'signed out') i18n.report(statusBox, error);
  }
}

byId('new-upload-link').addEventListener('click', () => {
  pickerChoice = { expires: '7d', cap: '5g' };
  openPicker('');
});

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

i18n.onChange(() => {
  if (!rows.querySelector('form') && !uploadRows.querySelector('form')) render();
});

load();
