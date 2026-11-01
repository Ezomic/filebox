function extensionOf(name) {
  const dot = name.lastIndexOf('.');
  return dot <= 0 ? '' : name.slice(dot + 1).toLowerCase();
}

const VIEW_KINDS = {
  image: ['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'heic', 'heif'],
  video: ['mp4', 'webm', 'mov', 'm4v'],
  audio: ['mp3', 'm4a', 'wav', 'ogg', 'flac'],
  pdf: ['pdf'],
  text: ['txt', 'md', 'log', 'json', 'csv'],
};
const CONVERTED_EXTENSIONS = ['heic', 'heif'];
const TEXT_LIMIT = 1024 * 1024;
const LOAD_MORE_AHEAD = 3;

let viewItems = [];
let viewer = null;

function viewKind(name) {
  const ext = extensionOf(name);
  const kind = Object.keys(VIEW_KINDS).find((candidate) => VIEW_KINDS[candidate].includes(ext));
  return kind === undefined ? null : kind;
}

function viewItem(path, name, size, extra) {
  const query = `path=${encodeURIComponent(path)}${extra || ''}`;
  const kind = viewKind(name);
  const converted = CONVERTED_EXTENSIONS.includes(extensionOf(name));
  return {
    name,
    size,
    kind,
    url: `/api/files/${converted ? 'preview' : 'view'}?${query}`,
    downloadUrl: `/api/files/download?${query}`,
    posterUrl: kind === 'video' ? `/api/files/thumb?${query}` : null,
  };
}

function buildViewer(options) {
  const overlay = el('div', undefined, 'viewer');
  overlay.setAttribute('role', 'dialog');
  overlay.setAttribute('aria-modal', 'true');
  overlay.setAttribute('aria-label', 'File preview');
  const title = el('span', undefined, 'viewer-title');
  const download = el('a', 'Download', 'link');
  const previous = button('Previous', () => step(-1), 'link');
  const next = button('Next', () => step(1), 'link');
  const close = button('Close', closeViewer, 'link');
  const bar = el('div', undefined, 'viewer-bar');
  bar.append(title, previous, next, download);
  const datePanel = options.onSetDate ? buildDatePanel(options) : null;
  if (datePanel) bar.append(button('Change date', datePanel.toggle, 'link'));
  if (options.onRemove) {
    bar.append(button('Delete', () => options.onRemove(viewItems[viewer.index], viewer.index), 'link danger'));
  }
  bar.append(close);
  const body = el('div', undefined, 'viewer-body');
  overlay.append(bar);
  if (datePanel) overlay.append(datePanel.form);
  overlay.append(body);
  overlay.addEventListener('click', (event) => {
    if (event.target === overlay || event.target === body) closeViewer();
  });
  return { overlay, title, download, previous, next, close, body, datePanel, index: 0, opener: null, token: 0, options };
}

// The panel never closes itself on success: the page re-renders and closes the viewer, or shows the error here.
function buildDatePanel(options) {
  const form = el('form', undefined, 'viewer-date');
  form.hidden = true;
  const input = document.createElement('input');
  input.type = 'date';
  input.required = true;
  input.min = '1900-01-01';
  input.setAttribute('aria-label', 'Photo date');
  const save = el('button', 'Save date');
  save.type = 'submit';
  const reset = button('Reset to original', () => run(() => options.onResetDate(viewItems[viewer.index], viewer.index)), 'link');
  const cancel = button('Cancel', () => { form.hidden = true; }, 'link');
  const message = el('p', undefined, 'error');
  message.setAttribute('role', 'status');
  form.append(input, save, reset, cancel, message);

  async function run(action) {
    message.textContent = '';
    save.disabled = true;
    reset.disabled = true;
    try {
      await action();
    } catch (error) {
      if (error.message !== 'signed out') message.textContent = error.message;
    } finally {
      save.disabled = false;
      reset.disabled = false;
    }
  }

  form.addEventListener('submit', (event) => {
    event.preventDefault();
    if (input.value === '') {
      message.textContent = 'Pick a date.';
      return;
    }
    run(() => options.onSetDate(viewItems[viewer.index], viewer.index, input.value));
  });

  return {
    form,
    toggle: () => {
      form.hidden = !form.hidden;
      if (!form.hidden) input.focus();
    },
    show: (item) => {
      form.hidden = true;
      input.value = item.date;
      reset.hidden = !item.custom;
      message.textContent = '';
    },
  };
}

function stopMedia() {
  viewer.body.querySelectorAll('video, audio').forEach((media) => {
    media.dataset.stopped = '1';
    media.pause();
    media.removeAttribute('src');
    media.load();
  });
}

function mediaBody(item) {
  const kind = item.kind;
  const node = document.createElement(kind === 'image' ? 'img' : kind === 'pdf' ? 'iframe' : kind);
  if (kind === 'video' || kind === 'audio') node.controls = true;
  if (kind === 'video' && item.posterUrl) node.poster = item.posterUrl;
  if (kind === 'image') node.alt = item.name;
  if (kind === 'pdf') node.title = item.name;
  node.addEventListener('error', () => {
    if (viewer && !node.dataset.stopped && node.parentNode === viewer.body) viewer.body.replaceChildren(failedBody());
  });
  node.src = item.url;
  node.className = `viewer-${kind}`;
  return node;
}

function failedBody() {
  return el('p', 'This file could not be previewed. Download it instead.', 'viewer-note');
}

function textBody(item, token) {
  const pre = el('pre', undefined, 'viewer-text');
  if (item.size > TEXT_LIMIT) {
    pre.textContent = 'Too large to preview, download instead.';
    return pre;
  }
  pre.textContent = 'Loading...';
  const current = () => viewer !== null && viewer.token === token;
  fetch(item.url)
    .then(async (response) => {
      if (response.status === 401) {
        window.location.href = '/login';
        return;
      }
      if (!response.ok) throw new Error('failed');
      if (Number(response.headers.get('Content-Length')) > TEXT_LIMIT) {
        if (current()) pre.textContent = 'Too large to preview, download instead.';
        return;
      }
      const text = await response.text();
      if (current()) pre.textContent = text.length > TEXT_LIMIT ? 'Too large to preview, download instead.' : text;
    })
    .catch(() => {
      if (current()) viewer.body.replaceChildren(failedBody());
    });
  return pre;
}

function showViewerItem(index) {
  const item = viewItems[index];
  viewer.index = index;
  viewer.token += 1;
  stopMedia();
  viewer.title.textContent = item.name;
  viewer.download.href = item.downloadUrl;
  viewer.download.download = item.name;
  viewer.previous.disabled = index === 0;
  viewer.next.disabled = index === viewItems.length - 1;
  if (viewer.datePanel) viewer.datePanel.show(item);
  viewer.body.replaceChildren(item.kind === 'text' ? textBody(item, viewer.token) : mediaBody(item));
  if (viewer.options.loadMore && index >= viewItems.length - LOAD_MORE_AHEAD) viewer.options.loadMore();
}

function step(direction) {
  if (!viewer) return;
  const index = viewer.index + direction;
  if (index >= 0 && index < viewItems.length) showViewerItem(index);
}

function openViewer(items, index, opener, options) {
  if (index < 0 || index >= items.length) return;
  viewItems = items;
  if (!viewer) {
    viewer = buildViewer(options || {});
    document.body.append(viewer.overlay);
    document.body.classList.add('viewing');
  }
  viewer.opener = opener;
  showViewerItem(index);
  viewer.close.focus();
}

function updateViewerItems(items, index) {
  if (!viewer) return;
  viewItems = items;
  if (items.length === 0) {
    closeViewer();
    return;
  }
  showViewerItem(Math.min(Math.max(index, 0), items.length - 1));
}

function extendViewerItems(items) {
  if (!viewer) return;
  viewItems = items;
  viewer.next.disabled = viewer.index === viewItems.length - 1;
}

function closeViewer() {
  if (!viewer) return;
  stopMedia();
  const { overlay, opener } = viewer;
  viewer = null;
  overlay.remove();
  document.body.classList.remove('viewing');
  if (opener && opener.isConnected) opener.focus();
}

document.addEventListener('keydown', (event) => {
  if (!viewer) return;
  if (event.key === 'Escape') {
    closeViewer();
  } else if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
    if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    if (/^(VIDEO|AUDIO|INPUT)$/.test(event.target.tagName)) return;
    step(event.key === 'ArrowLeft' ? -1 : 1);
  }
});
