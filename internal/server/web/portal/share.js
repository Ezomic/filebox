const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const rows = byId('entry-rows');
const base = `/${encodeURIComponent(window.location.pathname.split('/')[1] || '')}`;

let currentPath = '';
let listing = null;

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

const query = (path) => `?path=${encodeURIComponent(path)}`;
const join = (parent, name) => (parent === '' ? name : `${parent}/${name}`);

function pathFromHash() {
  try {
    return decodeURIComponent(window.location.hash.slice(1));
  } catch {
    return '';
  }
}

function go(path) {
  const hash = path === '' ? '' : `#${encodeURIComponent(path)}`;
  if (window.location.hash === hash || (hash === '' && window.location.hash === '')) load();
  else window.location.hash = hash;
}

function unavailable() {
  byId('file-card').hidden = true;
  byId('folder-card').hidden = true;
  i18n.msg(statusBox, () => t('share.unavailable'));
}

async function load() {
  currentPath = pathFromHash();
  try {
    const response = await fetch(`${base}/list${query(currentPath)}`);
    if (response.status === 429) {
      i18n.msg(statusBox, () => t('error.rate_limited'));
      return;
    }
    if (!response.ok) {
      unavailable();
      return;
    }
    listing = await response.json();
    i18n.msg(statusBox);
    render();
  } catch {
    i18n.msg(statusBox, () => t('error.network'));
  }
}

function render() {
  if (listing.kind === 'file') renderFile();
  else renderFolder();
}

function renderFile() {
  byId('folder-card').hidden = true;
  byId('file-card').hidden = false;
  byId('title').textContent = listing.name;
  document.title = listing.name;
  byId('title').removeAttribute('data-i18n');
  i18n.msg(byId('file-meta'), () => t('share.fileMeta', { size: i18n.size(listing.size), when: i18n.dateTime(listing.modified) }));
  const download = byId('file-download');
  download.href = `${base}/download`;
  download.download = listing.name;
  const preview = byId('file-preview');
  preview.hidden = !listing.preview;
  if (listing.preview) {
    preview.src = `${base}/preview`;
    preview.alt = listing.name;
  }
}

function renderFolder() {
  byId('file-card').hidden = true;
  byId('folder-card').hidden = false;
  byId('title').textContent = listing.name;
  document.title = listing.name;
  byId('title').removeAttribute('data-i18n');
  renderCrumbs();
  rows.replaceChildren(...listing.entries.map(entryRow));
  byId('empty').hidden = listing.entries.length > 0;
  byId('zip-download').href = `${base}/zip${query(currentPath)}`;
}

function renderCrumbs() {
  const parts = currentPath === '' ? [] : currentPath.split('/');
  const nodes = [button(listing.name, () => go(''), 'link')];
  parts.forEach((part, index) => {
    nodes.push(el('span', '/', 'sep'));
    nodes.push(button(part, () => go(parts.slice(0, index + 1).join('/')), 'link'));
  });
  byId('crumbs').replaceChildren(...nodes);
}

function entryRow(entry) {
  const path = join(currentPath, entry.name);
  const row = el('tr');
  const name = el('td', undefined, 'name');
  if (entry.isDir) name.append(button(`${entry.name}/`, () => go(path), 'folder'));
  else name.textContent = entry.name;
  const actions = el('td', undefined, 'row-actions');
  if (!entry.isDir) {
    const link = el('a', t('common.download'), 'link');
    link.href = `${base}/download${query(path)}`;
    link.download = entry.name;
    actions.append(link);
  }
  row.append(name, el('td', entry.isDir ? '' : i18n.size(entry.size), 'meta'), el('td', i18n.dateTime(entry.modified), 'meta'), actions);
  return row;
}

i18n.onChange(() => {
  if (!listing) return;
  if (listing.kind === 'file') i18n.apply();
  else render();
});

window.addEventListener('hashchange', load);
load();
