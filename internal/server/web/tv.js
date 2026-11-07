const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const rows = byId('channel-rows');
// The list is rebuilt every few seconds, so an open delete confirmation is redrawn from this.
let confirmingDelete = null;

async function api(method, path, payload) {
  const options = { method };
  if (payload !== undefined) {
    options.headers = { 'Content-Type': 'application/json' };
    options.body = JSON.stringify(payload);
  }
  const response = await fetch(path, options);
  if (response.status === 401 && (await response.clone().json().catch(() => ({}))).error === 'not signed in') {
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

function stateText(channel) {
  if (!channel.enabled) return 'off';
  const status = channel.status;
  if (status.ready) return 'live';
  if (status.state === 'backoff') return `failing: ${status.last_error || 'restarting'}`;
  return 'starting...';
}

function describe(channel) {
  if (channel.kind === 'url') return channel.source.replace(/^([a-z]+:\/\/)[^/@]*@/, '$1');
  return `${channel.files.length} file${channel.files.length === 1 ? '' : 's'}, looped`;
}

function channelRow(channel) {
  const row = el('tr');
  const info = el('td');
  info.append(el('div', channel.name, 'name'), el('div', `${channel.id} · ${describe(channel)}`, 'detail'));
  const actions = el('td', undefined, 'row-actions');
  row.append(info, el('td', stateText(channel), channel.status.ready || !channel.enabled ? 'detail' : 'detail stale'), actions);
  if (channel.id === confirmingDelete) showDeleteConfirm(actions, channel);
  else showActions(actions, channel);
  return row;
}

function showActions(cell, channel) {
  if (confirmingDelete === channel.id) confirmingDelete = null;
  cell.replaceChildren(
    button('Restart', async () => {
      try {
        await api('POST', `/api/tv/channels/${encodeURIComponent(channel.id)}/restart`);
        await load();
      } catch (error) {
        showError(error.message);
      }
    }, 'link'),
    button('Delete', () => showDeleteConfirm(cell, channel), 'link danger'),
  );
}

function showDeleteConfirm(cell, channel) {
  confirmingDelete = channel.id;
  const message = el('span', undefined, 'error');
  cell.replaceChildren(
    el('span', `Delete ${channel.name}?`, 'error'),
    button('Delete channel', async () => {
      try {
        await api('DELETE', `/api/tv/channels/${encodeURIComponent(channel.id)}`);
        await load();
      } catch (error) {
        message.textContent = error.message;
      }
    }, 'danger-solid'),
    button('Cancel', () => showActions(cell, channel), 'link'),
    message,
  );
}

function showError(message) {
  statusBox.textContent = message;
}

async function load() {
  try {
    const channels = await api('GET', '/api/tv/channels');
    showError('');
    if (!channels.some((channel) => channel.id === confirmingDelete)) confirmingDelete = null;
    rows.replaceChildren(...channels.map(channelRow));
    byId('empty').hidden = channels.length > 0;
  } catch (error) {
    showError(error.message === 'signed out' ? '' : 'Could not load the channels. Is streambox running?');
  }
}

async function loadMedia() {
  const select = byId('new-files');
  try {
    const files = await api('GET', '/api/tv/media');
    files.sort();
    select.replaceChildren(...files.map((file) => {
      const option = el('option', file);
      option.value = file;
      return option;
    }));
    byId('no-media').hidden = files.length > 0;
  } catch {
    byId('no-media').hidden = false;
  }
}

function syncKind() {
  const files = byId('new-kind').value === 'files';
  byId('url-fields').hidden = files;
  byId('file-fields').hidden = !files;
  byId('new-source').required = !files;
  if (files) loadMedia();
}

byId('new-kind').addEventListener('change', syncKind);

byId('add-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const errorBox = byId('add-error');
  const submit = event.target.querySelector('button[type="submit"]');
  errorBox.textContent = '';
  submit.disabled = true;
  try {
    const kind = byId('new-kind').value;
    const channel = { id: byId('new-id').value, name: byId('new-name').value, kind };
    if (kind === 'url') {
      channel.source = byId('new-source').value.trim();
      channel.transcode = byId('new-transcode').checked;
    } else {
      channel.files = Array.from(byId('new-files').selectedOptions, (option) => option.value);
    }
    await api('POST', '/api/tv/channels', channel);
    event.target.reset();
    syncKind();
    await load();
  } catch (error) {
    errorBox.textContent = error.message;
  } finally {
    submit.disabled = false;
  }
});

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

syncKind();
load();
setInterval(load, 5000);
