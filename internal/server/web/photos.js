const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const groups = byId('photo-groups');
const sentinel = byId('sentinel');
const summary = byId('summary');
const emptyNote = byId('empty');
const userSelect = byId('photos-user');

const PAGE_SIZE = 200;
const LOAD_PARALLEL = 4;
const LONG_PRESS_MS = 500;
const UNDO_VISIBLE_MS = 30000;

let photos = [];
let seen = new Set();
let nextCursor = '';
let loadingPage = false;
let generation = 0;
let lastGroup = null;
let lastMonth = '';

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

function showError(message) {
  statusBox.textContent = message;
}

function userQuery() {
  return userSelect && userSelect.value !== '' ? `&user=${encodeURIComponent(userSelect.value)}` : '';
}

function monthKey(photo) {
  return photo.date.slice(0, 7);
}

function monthLabel(photo) {
  return new Date(photo.date).toLocaleDateString(undefined, { month: 'long', year: 'numeric', timeZone: 'UTC' });
}

function thumbUrl(photo) {
  return `/api/files/thumb?path=${encodeURIComponent(photo.path)}${userQuery()}`;
}

function nameOf(path) {
  return path.slice(path.lastIndexOf('/') + 1);
}

function toItem(photo) {
  return viewItem(photo.path, nameOf(photo.path), photo.size, userQuery());
}

const undoBar = el('p', undefined, 'notice');
const undoText = el('span');
let undoTimer = null;
undoBar.hidden = true;
statusBox.after(undoBar);

function hideUndo() {
  clearTimeout(undoTimer);
  undoBar.hidden = true;
}

function showUndo(message, id) {
  clearTimeout(undoTimer);
  undoText.textContent = message;
  undoBar.replaceChildren(undoText);
  if (id !== undefined) undoBar.append(button('Undo', () => undoDelete(id), 'link'));
  undoBar.hidden = false;
  undoTimer = setTimeout(hideUndo, UNDO_VISIBLE_MS);
}

const pending = [];
let running = 0;

function pump() {
  while (running < LOAD_PARALLEL && pending.length > 0) {
    const tile = pending.shift();
    tile.queued = false;
    if (!tile.visible || tile.loaded || !tile.node.isConnected) continue;
    startLoad(tile);
  }
}

function startLoad(tile) {
  running += 1;
  tile.loaded = true;
  const done = () => {
    running -= 1;
    pump();
  };
  tile.image.addEventListener('load', () => {
    tile.node.classList.add('ready');
    done();
  }, { once: true });
  tile.image.addEventListener('error', () => {
    tile.node.classList.add('failed');
    done();
  }, { once: true });
  tile.image.src = thumbUrl(tile.photo);
}

const tileObserver = 'IntersectionObserver' in window
  ? new IntersectionObserver((entries) => {
    entries.forEach((entry) => {
      const tile = entry.target.tile;
      tile.visible = entry.isIntersecting;
      if (tile.visible && !tile.loaded && !tile.queued) {
        tile.queued = true;
        pending.push(tile);
      }
    });
    pump();
  }, { rootMargin: '300px' })
  : null;

function makeTile(photo) {
  const node = el('div', undefined, `tile ${photo.kind}`);
  const name = nameOf(photo.path);
  const open = el('button', undefined, 'tile-open');
  open.type = 'button';
  open.title = name;
  open.setAttribute('aria-label', photo.kind === 'video' ? `Play ${name}` : `Open ${name}`);
  const image = document.createElement('img');
  image.alt = '';
  image.decoding = 'async';
  open.append(image, el('span', name, 'tile-name'));
  open.addEventListener('click', () => {
    if (node.suppressClick) {
      node.suppressClick = false;
      return;
    }
    openGallery(photo, open);
  });
  const remove = button('Delete', () => deletePhoto(photo), 'tile-delete');
  remove.setAttribute('aria-label', `Delete ${name}`);
  node.append(open, remove);
  armOnLongPress(node, open);

  const tile = { node, image, photo, visible: false, loaded: false, queued: false };
  node.tile = tile;
  photo.tile = tile;
  if (tileObserver) {
    tileObserver.observe(node);
  } else {
    tile.visible = true;
    pending.push(tile);
  }
  return node;
}

function armOnLongPress(node, open) {
  let timer = null;
  const cancel = () => clearTimeout(timer);
  open.addEventListener('pointerdown', (event) => {
    if (event.pointerType !== 'touch') return;
    timer = setTimeout(() => {
      node.classList.add('armed');
      node.suppressClick = true;
    }, LONG_PRESS_MS);
  });
  ['pointerup', 'pointercancel', 'pointerleave', 'pointermove'].forEach((type) => open.addEventListener(type, cancel));
  open.addEventListener('contextmenu', (event) => {
    if (node.classList.contains('armed')) event.preventDefault();
  });
}

function addPhotos(batch) {
  const fresh = batch.filter((photo) => !seen.has(photo.path));
  fresh.forEach((photo) => {
    seen.add(photo.path);
    photos.push(photo);
    if (monthKey(photo) !== lastMonth || lastGroup === null) {
      lastMonth = monthKey(photo);
      const section = el('section', undefined, 'month');
      section.append(el('h2', monthLabel(photo)));
      lastGroup = el('div', undefined, 'photo-grid');
      section.append(lastGroup);
      groups.append(section);
    }
    lastGroup.append(makeTile(photo));
  });
  updateSummary();
}

function updateSummary() {
  const videos = photos.filter((photo) => photo.kind === 'video').length;
  const total = photos.length;
  if (total === 0) {
    summary.textContent = '';
  } else {
    const parts = [`${total - videos} ${total - videos === 1 ? 'photo' : 'photos'}`];
    if (videos > 0) parts.push(`${videos} ${videos === 1 ? 'video' : 'videos'}`);
    summary.textContent = `${parts.join(' and ')}${nextCursor === '' ? '' : ' so far'}, newest first.`;
  }
  emptyNote.hidden = total > 0 || nextCursor !== '' || loadingPage;
}

async function loadPage() {
  if (loadingPage) return;
  loadingPage = true;
  const mine = generation;
  try {
    const after = nextCursor === '' ? '' : `&after=${encodeURIComponent(nextCursor)}`;
    const body = await api('GET', `/api/files/photos?limit=${PAGE_SIZE}${after}${userQuery()}`);
    if (mine !== generation) return;
    showError('');
    nextCursor = body.next;
    loadingPage = false;
    addPhotos(body.photos);
    extendViewerItems(photos.map(toItem));
  } catch (error) {
    if (mine !== generation) return;
    if (error.message !== 'signed out') showError(error.message);
    nextCursor = '';
  } finally {
    if (mine === generation) {
      loadingPage = false;
      updateSummary();
      watchSentinel();
    }
  }
}

const sentinelObserver = 'IntersectionObserver' in window
  ? new IntersectionObserver((entries) => {
    if (entries.some((entry) => entry.isIntersecting) && nextCursor !== '') loadPage();
  }, { rootMargin: '800px' })
  : null;

function watchSentinel() {
  if (nextCursor === '') return;
  if (!sentinelObserver) {
    loadPage();
    return;
  }
  sentinelObserver.unobserve(sentinel);
  sentinelObserver.observe(sentinel);
}

function reset() {
  generation += 1;
  photos = [];
  seen = new Set();
  nextCursor = '';
  loadingPage = false;
  lastGroup = null;
  lastMonth = '';
  pending.length = 0;
  groups.replaceChildren();
  closeViewer();
}

async function load() {
  reset();
  await loadPage();
}

function openGallery(photo, opener) {
  openViewer(photos.map(toItem), photos.indexOf(photo), opener, {
    onRemove: (item, index) => deletePhoto(photos[index]),
    loadMore: () => {
      if (nextCursor !== '') loadPage();
    },
  });
}

function dropPhoto(photo) {
  const index = photos.indexOf(photo);
  photos.splice(index, 1);
  seen.delete(photo.path);
  const tile = photo.tile.node;
  const section = tile.parentNode.parentNode;
  if (tileObserver) tileObserver.unobserve(tile);
  tile.remove();
  if (section.querySelector('.tile') === null) {
    if (section.querySelector('.photo-grid') === lastGroup) {
      lastGroup = null;
      lastMonth = '';
    }
    section.remove();
  }
  return index;
}

async function deletePhoto(photo) {
  try {
    const { id } = await api('DELETE', `/api/files?path=${encodeURIComponent(photo.path)}${userQuery()}`);
    showError('');
    const index = dropPhoto(photo);
    updateViewerItems(photos.map(toItem), index);
    updateSummary();
    showUndo('Deleted.', id);
  } catch (error) {
    if (error.message !== 'signed out') showError(error.message);
  }
}

async function undoDelete(id) {
  try {
    const query = userSelect && userSelect.value !== '' ? `?user=${encodeURIComponent(userSelect.value)}` : '';
    const { path } = await api('POST', `/api/trash/${id}/restore${query}`);
    showError('');
    showUndo(`Restored ${path}.`);
    await load();
  } catch (error) {
    if (error.message !== 'signed out') showError(error.message);
  }
}

async function loadUsers() {
  const { users } = await api('GET', '/api/users');
  users.forEach((user) => {
    const option = el('option', `${user.name}'s files`);
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
    hideUndo();
    load();
  });
  loadUsers().catch((error) => {
    if (error.message !== 'signed out') showError(error.message);
  });
}
load();
