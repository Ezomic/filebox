const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const rows = byId('channel-rows');
const shareRows = byId('share-rows');
const slugMax = 24;
let channelsById = new Map();
let shares = [];
let suggestedSlug = '';
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
  const links = shares.filter((share) => share.channel === channel.id).length;
  const warning = links === 0 ? '' : ` Its ${links === 1 ? 'share link stops' : `${links} share links stop`} working.`;
  cell.replaceChildren(
    el('span', `Delete ${channel.name}?${warning}`, 'error'),
    button('Delete channel', async () => {
      try {
        await api('DELETE', `/api/tv/channels/${encodeURIComponent(channel.id)}`);
        await load();
        await loadShares();
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
    channelsById = new Map(channels.map((channel) => [channel.id, channel]));
    if (!channels.some((channel) => channel.id === confirmingDelete)) confirmingDelete = null;
    rows.replaceChildren(...channels.map(channelRow));
    byId('empty').hidden = channels.length > 0;
    showChannelChoices(channels);
  } catch (error) {
    showError(error.message === 'signed out' ? '' : 'Could not load the channels. Is streambox running?');
  }
}

// The suggestion is the channel name in the characters a link name may use, cut to fit.
function slugFrom(text) {
  return text.normalize('NFKD').replace(/\p{M}/gu, '').toLowerCase()
    .replace(/[^a-z0-9]+/g, '-').replace(/^-+/, '').slice(0, slugMax).replace(/-+$/, '');
}

// A name the admin typed is kept; only an empty field or the previous suggestion is replaced.
function suggestSlug() {
  const channel = channelsById.get(byId('share-channel').value);
  const slug = byId('share-slug');
  if (!channel) return;
  const suggestion = slugFrom(channel.name) || slugFrom(channel.id) || 'tv';
  if (slug.value === '' || slug.value === suggestedSlug) slug.value = suggestion;
  suggestedSlug = suggestion;
}

function showChannelChoices(channels) {
  const select = byId('share-channel');
  const chosen = select.value;
  const choices = channels.map((channel) => `${channel.id}\n${channel.name}`).join('\n');
  byId('share-form').hidden = channels.length === 0;
  if (select.dataset.choices === choices) return;
  select.dataset.choices = choices;
  select.replaceChildren(...channels.map((channel) => {
    const option = el('option', channel.name);
    option.value = channel.id;
    return option;
  }));
  if (channelsById.has(chosen)) select.value = chosen;
  suggestSlug();
}

function shareRow(share) {
  const row = el('tr');
  const info = el('td', undefined, 'name');
  const channel = channelsById.get(share.channel);
  info.append(
    el('div', share.label),
    el('div', `${channel ? channel.name : share.channel} · created ${share.created.slice(0, 10)}`, 'detail'),
    el('div', share.url || share.code, 'detail share-url'),
  );
  const actions = el('td', undefined, 'row-actions');
  showShareActions(actions, share);
  row.append(info, actions);
  return row;
}

function showShareActions(cell, share) {
  const copy = button('Copy link', async () => {
    try {
      await navigator.clipboard.writeText(share.url);
      copy.textContent = 'Copied';
    } catch {
      // The 5 second refresh clears the status line, so the selected link is what the admin is left with.
      getSelection().selectAllChildren(cell.closest('tr').querySelector('.share-url'));
      copy.textContent = 'Link selected';
      showError('Copying did not work. The link is selected: copy it by hand.');
    }
    setTimeout(() => { copy.textContent = 'Copy link'; }, 3000);
  }, 'link');
  copy.hidden = !share.url;
  cell.replaceChildren(copy, button('Revoke', () => showRevokeConfirm(cell, share), 'link danger'));
}

function showRevokeConfirm(cell, share) {
  const message = el('span', undefined, 'error');
  cell.replaceChildren(
    el('span', `Revoke the link for ${share.label}? It stops working at once.`, 'error'),
    button('Revoke link', async () => {
      try {
        await api('DELETE', `/api/tv/shares/${encodeURIComponent(share.code)}`);
        await loadShares();
      } catch (error) {
        message.textContent = error.message;
      }
    }, 'danger-solid'),
    button('Cancel', () => showShareActions(cell, share), 'link'),
    message,
  );
}

async function loadShares() {
  try {
    shares = (await api('GET', '/api/tv/shares')).shares;
    shareRows.replaceChildren(...shares.map(shareRow));
    byId('no-shares').hidden = shares.length > 0;
    byId('no-portal').hidden = !shares.some((share) => !share.url);
  } catch (error) {
    showError(error.message === 'signed out' ? '' : 'Could not load the share links.');
  }
}

byId('share-channel').addEventListener('change', suggestSlug);

byId('share-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const errorBox = byId('share-error');
  const submit = event.target.querySelector('button[type="submit"]');
  errorBox.textContent = '';
  submit.disabled = true;
  try {
    await api('POST', '/api/tv/shares', {
      label: byId('share-label').value,
      channel: byId('share-channel').value,
      slug: byId('share-slug').value,
    });
    byId('share-label').value = '';
    byId('share-slug').value = '';
    suggestSlug();
    await loadShares();
  } catch (error) {
    errorBox.textContent = error.message;
  } finally {
    submit.disabled = false;
  }
});

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
load().then(loadShares);
setInterval(load, 5000);
