const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const groups = byId('photo-groups');
const sentinel = byId('sentinel');
const summary = byId('summary');
const emptyNote = byId('empty');
const userSelect = byId('photos-user');
const typeButtons = byId('type-filter');
const jumpForm = byId('jump');
const jumpYear = byId('jump-year');
const jumpMonth = byId('jump-month');
const jumpGo = byId('jump-go');
const jumpNote = byId('jump-note') || document.createElement('p');

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
let inflight = null;
let typeFilter = '';

try {
  typeFilter = sessionStorage.getItem('photos.type') || '';
} catch (error) {
  typeFilter = '';
}
if (!['', 'image', 'video'].includes(typeFilter)) typeFilter = '';

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

function userQuery() {
  return userSelect && userSelect.value !== '' ? `&user=${encodeURIComponent(userSelect.value)}` : '';
}

function monthKey(photo) {
  return photo.date.slice(0, 7);
}

function monthLabel(photo) {
  return i18n.monthYear(photo.date);
}

function thumbUrl(photo) {
  return `/api/files/thumb?path=${encodeURIComponent(photo.path)}${userQuery()}`;
}

function nameOf(path) {
  return path.slice(path.lastIndexOf('/') + 1);
}

function toItem(photo) {
  return { ...viewItem(photo.path, nameOf(photo.path), photo.size, userQuery()), date: photo.date.slice(0, 10), custom: photo.custom };
}

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
  open.setAttribute('aria-label', t(photo.kind === 'video' ? 'photos.play' : 'photos.open', { name }));
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
  const remove = button(t('common.delete'), () => deletePhoto(photo), 'tile-delete');
  remove.setAttribute('aria-label', t('photos.delete', { name }));
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
      section.dataset.month = lastMonth;
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
    i18n.msg(summary);
  } else {
    const more = nextCursor !== '';
    i18n.msg(summary, () => {
      const photoText = t('photos.countPhotos', { count: total - videos });
      const items = videos > 0 ? t('photos.both', { photos: photoText, videos: t('photos.countVideos', { count: videos }) }) : photoText;
      return t(more ? 'photos.summarySoFar' : 'photos.summary', { items });
    });
  }
  const emptyKey = { '': 'photos.empty', video: 'photos.emptyVideos', image: 'photos.emptyPhotos' }[typeFilter];
  i18n.msg(emptyNote, () => t(emptyKey));
  emptyNote.hidden = total > 0 || nextCursor !== '' || loadingPage;
}

function loadPage() {
  if (inflight) return inflight;
  const request = fetchPage().finally(() => {
    if (inflight === request) inflight = null;
  });
  inflight = request;
  return request;
}

async function fetchPage() {
  loadingPage = true;
  const mine = generation;
  try {
    const after = nextCursor === '' ? '' : `&after=${encodeURIComponent(nextCursor)}`;
    const type = typeFilter === '' ? '' : `&type=${typeFilter}`;
    const body = await api('GET', `/api/files/photos?limit=${PAGE_SIZE}${after}${type}${userQuery()}`);
    if (mine !== generation) return;
    showError(null);
    nextCursor = body.next;
    loadingPage = false;
    addPhotos(body.photos);
    extendViewerItems(photos.map(toItem));
  } catch (error) {
    if (mine !== generation) return;
    if (error.message !== 'signed out') showError(error);
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
  inflight = null;
  pending.length = 0;
  groups.replaceChildren();
  i18n.msg(jumpNote);
  closeViewer();
}

async function load() {
  reset();
  await loadPage();
}

function openGallery(photo, opener) {
  const options = {
    onRemove: (item, index) => deletePhoto(photos[index]),
    loadMore: () => {
      if (nextCursor !== '') loadPage();
    },
  };
  if (!userSelect) {
    options.onSetDate = (item, index, day) => changeDate(photos[index], 'PUT', day);
    options.onResetDate = (item, index) => changeDate(photos[index], 'DELETE');
  }
  openViewer(photos.map(toItem), photos.indexOf(photo), opener, options);
}

async function loadUntil(done) {
  const mine = generation;
  if (inflight) await inflight;
  if (mine !== generation) return false;
  while (!done() && nextCursor !== '') {
    await loadPage();
    if (mine !== generation) return false;
  }
  return mine === generation;
}

function showTile(photo) {
  const node = photo.tile.node;
  node.scrollIntoView({ block: 'center' });
  node.classList.add('found');
  setTimeout(() => node.classList.remove('found'), 2500);
}

async function changeDate(photo, method, day) {
  const path = '/api/files/photo-date';
  if (method === 'PUT') {
    await api('PUT', path, { path: photo.path, date: day });
  } else {
    await api('DELETE', `${path}?path=${encodeURIComponent(photo.path)}`);
  }
  const target = photo.path;
  await load();
  const found = await loadUntil(() => seen.has(target));
  if (!found) return;
  const moved = photos.find((candidate) => candidate.path === target);
  if (!moved) return;
  showTile(moved);
  i18n.msg(jumpNote, () => (method === 'PUT' ? t('photos.dateChanged', { date: i18n.longDay(moved.date) }) : t('photos.dateReset')));
}

function setType(type) {
  typeFilter = type;
  try {
    sessionStorage.setItem('photos.type', type);
  } catch (error) {
    // The choice then only lasts until the next filter change.
  }
  markType();
  hideUndo();
  load();
}

function markType() {
  typeButtons.querySelectorAll('button').forEach((node) => {
    node.setAttribute('aria-pressed', String(node.dataset.type === typeFilter));
  });
}

function sectionFor(target) {
  const sections = [...groups.querySelectorAll('.month')];
  return sections.find((section) => section.dataset.month <= target) || sections[sections.length - 1];
}

async function jump(year, month) {
  if (!/^\d{4}$/.test(year) || Number(year) < 1900) {
    i18n.msg(jumpNote, () => t('photos.yearRange'));
    return;
  }
  const target = `${year}-${month === '' ? '12' : month}`;
  jumpGo.disabled = true;
  i18n.msg(jumpNote, () => t('photos.looking'));
  const reached = await loadUntil(() => photos.length > 0 && monthKey(photos[photos.length - 1]) <= target);
  jumpGo.disabled = false;
  if (!reached) return;
  const section = sectionFor(target);
  if (!section) {
    i18n.msg(jumpNote, () => t('photos.nothingToJump'));
    return;
  }
  const exact = month === '' ? section.dataset.month.startsWith(year) : section.dataset.month === target;
  const label = () => (month === '' ? year : i18n.monthYear(`${year}-${month}-01`));
  const oldest = nextCursor === '' && section === groups.querySelector('.month:last-child') && section.dataset.month > target;
  if (exact) {
    i18n.msg(jumpNote);
  } else if (oldest) {
    i18n.msg(jumpNote, () => t('photos.nothingOldest', { label: label() }));
  } else {
    i18n.msg(jumpNote, () => t('photos.nothingClosest', { label: label() }));
  }
  section.scrollIntoView({ block: 'start' });
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
    showError(null);
    const index = dropPhoto(photo);
    updateViewerItems(photos.map(toItem), index);
    updateSummary();
    showUndo(() => t('files.deleted'), id);
    window.refreshStorage?.();
  } catch (error) {
    if (error.message !== 'signed out') showError(error);
  }
}

async function undoDelete(id) {
  try {
    const query = userSelect && userSelect.value !== '' ? `?user=${encodeURIComponent(userSelect.value)}` : '';
    const { path } = await api('POST', `/api/trash/${id}/restore${query}`);
    showError(null);
    showUndo(() => t('files.restored', { path }));
    await load();
    window.refreshStorage?.();
  } catch (error) {
    if (error.message !== 'signed out') showError(error);
  }
}

async function loadUsers() {
  const { users } = await api('GET', '/api/users');
  users.forEach((user) => {
    const option = el('option', t('photos.userFiles', { name: user.name }));
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
    if (error.message !== 'signed out') showError(error);
  });
}

function fillMonths() {
  [...(jumpMonth ? jumpMonth.options : [])].forEach((option) => {
    if (option.value !== '') option.textContent = i18n.monthName(Number(option.value) - 1);
  });
}

function relabel() {
  fillMonths();
  groups.querySelectorAll('.month').forEach((section) => {
    section.querySelector('h2').textContent = i18n.monthYear(`${section.dataset.month}-01`);
  });
  photos.forEach((photo) => {
    const name = nameOf(photo.path);
    photo.tile.node.querySelector('.tile-open').setAttribute('aria-label', t(photo.kind === 'video' ? 'photos.play' : 'photos.open', { name }));
    const remove = photo.tile.node.querySelector('.tile-delete');
    remove.textContent = t('common.delete');
    remove.setAttribute('aria-label', t('photos.delete', { name }));
  });
  updateSummary();
}

i18n.onChange(relabel);
fillMonths();
if (typeButtons && jumpForm) {
  markType();
  typeButtons.querySelectorAll('button').forEach((node) => {
    node.addEventListener('click', () => {
      if (node.dataset.type !== typeFilter) setType(node.dataset.type);
    });
  });
  jumpForm.addEventListener('submit', (event) => {
    event.preventDefault();
    jump(jumpYear.value.trim(), jumpMonth.value);
  });
}
load();
