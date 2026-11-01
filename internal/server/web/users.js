const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const rows = byId('user-rows');

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

async function load() {
  try {
    const { users, usageReady, quotas } = await api('GET', '/api/users');
    showError('');
    render(users, usageReady, quotas);
    if (!usageReady) setTimeout(load, 5000);
  } catch (error) {
    showError(error.message === 'signed out' ? '' : 'Could not load the users.');
  }
}

function formatBytes(bytes) {
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`;
}

function usageText(user, usageReady) {
  if (user.bytes === null) return usageReady ? '-' : 'calculating...';
  if (user.quota !== null) return `${formatBytes(user.bytes)} of ${formatBytes(user.quota)}`;
  return user.quotaReady ? `${formatBytes(user.bytes)}, no limit` : formatBytes(user.bytes);
}

function render(users, usageReady, quotas) {
  rows.replaceChildren(...users.map((user) => userRow(user, usageReady, quotas)));
  byId('empty').hidden = users.length > 0;
}

function userRow(user, usageReady, quotas) {
  const row = el('tr');
  const actions = el('td', undefined, 'row-actions');
  const name = el('td', user.name, 'name');
  if (user.admin) name.append(el('span', 'Admin', 'badge'));
  const over = user.quota !== null && user.bytes !== null && user.bytes >= user.quota;
  row.append(name, el('td', usageText(user, usageReady), over ? 'error' : 'detail'), actions);
  showActions(actions, user, quotas);
  return row;
}

async function setRole(name, role) {
  try {
    await api('PUT', `/api/users/${encodeURIComponent(name)}/role`, { role });
    showError('');
    await load();
  } catch (error) {
    showError(error.message);
  }
}

function showActions(cell, user, quotas) {
  const { name, admin } = user;
  const buttons = [button('Change password', () => showPasswordForm(cell, user, quotas), 'link')];
  if (quotas && user.quotaReady) buttons.push(button('Quota', () => showQuotaForm(cell, user, quotas), 'link'));
  buttons.push(
    button(admin ? 'Remove admin' : 'Make admin', () => setRole(name, admin ? 'user' : 'admin'), 'link'),
    button('Delete', () => showDeleteConfirm(cell, user, quotas), 'link danger'),
  );
  cell.replaceChildren(...buttons);
}

const QUOTA_UNITS = { MB: 1024 ** 2, GB: 1024 ** 3, TB: 1024 ** 4 };
const QUOTA_MIN = 1024 ** 2;
const QUOTA_MAX = 1024 ** 5;

function quotaBytes(amount, unit) {
  const bytes = Math.round(Number(amount) * QUOTA_UNITS[unit]);
  return Number.isSafeInteger(bytes) && bytes >= QUOTA_MIN && bytes <= QUOTA_MAX ? bytes : null;
}

function showQuotaForm(cell, user, quotas) {
  const amount = el('input');
  amount.type = 'number';
  amount.min = '0';
  amount.step = 'any';
  amount.required = true;
  amount.inputMode = 'decimal';
  amount.setAttribute('aria-label', `Storage limit for ${user.name}`);
  const unit = el('select');
  unit.setAttribute('aria-label', 'Unit');
  for (const name of Object.keys(QUOTA_UNITS)) unit.append(new Option(name, name));
  unit.value = 'GB';
  if (user.quota !== null) {
    const best = Object.keys(QUOTA_UNITS).reverse().find((name) => user.quota >= QUOTA_UNITS[name]) || 'MB';
    unit.value = best;
    amount.value = String(Math.round((user.quota / QUOTA_UNITS[best]) * 100) / 100);
  }

  const form = el('form', undefined, 'inline');
  const message = el('span', undefined, 'error');
  const save = el('button', 'Save');
  save.type = 'submit';
  const extra = user.quota !== null ? [button('No limit', () => changeQuota('DELETE'), 'link')] : [];
  form.append(
    el('span', 'Takes effect at once. Lowering it below current use deletes nothing; it only blocks new files.', 'detail'),
    amount, unit, save, ...extra, button('Cancel', () => showActions(cell, user, quotas), 'link'), message,
  );

  async function changeQuota(method, payload) {
    save.disabled = true;
    try {
      await api(method, `/api/users/${encodeURIComponent(user.name)}/quota`, payload);
      showError('');
      await load();
    } catch (error) {
      message.textContent = error.message;
      save.disabled = false;
    }
  }

  form.addEventListener('submit', (event) => {
    event.preventDefault();
    const bytes = quotaBytes(amount.value, unit.value);
    if (bytes === null) {
      message.textContent = 'Enter a limit between 1 MB and 1 PB.';
      return;
    }
    changeQuota('PUT', { bytes });
  });

  cell.replaceChildren(form);
  amount.focus();
}

function showPasswordForm(cell, user, quotas) {
  const { name } = user;
  const input = el('input');
  input.type = 'password';
  input.minLength = 12;
  input.maxLength = 128;
  input.required = true;
  input.autocomplete = 'new-password';
  input.setAttribute('aria-label', `New password for ${name}`);

  const form = el('form', undefined, 'inline');
  const message = el('span', undefined, 'error');
  const save = el('button', 'Save');
  save.type = 'submit';
  form.append(input, save, button('Cancel', () => showActions(cell, user, quotas), 'link'), message);

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    save.disabled = true;
    try {
      await api('PUT', `/api/users/${encodeURIComponent(name)}/password`, { password: input.value });
      input.value = '';
      showActions(cell, user, quotas);
      showError('');
    } catch (error) {
      message.textContent = error.message;
      save.disabled = false;
    }
  });

  cell.replaceChildren(form);
  input.focus();
}

function showDeleteConfirm(cell, user, quotas) {
  const { name } = user;
  const message = el('span', undefined, 'error');
  cell.replaceChildren(
    el('span', `This deletes the account ${name}. It cannot be undone.`, 'error'),
    button('Delete account', async () => {
      try {
        await api('DELETE', `/api/users/${encodeURIComponent(name)}`);
        await load();
      } catch (error) {
        message.textContent = error.message;
      }
    }, 'danger-solid'),
    button('Cancel', () => showActions(cell, user, quotas), 'link'),
    message,
  );
}

byId('add-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const errorBox = byId('add-error');
  const submit = event.target.querySelector('button[type="submit"]');
  errorBox.textContent = '';
  submit.disabled = true;
  try {
    await api('POST', '/api/users', { name: byId('new-name').value, password: byId('new-password').value });
    event.target.reset();
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

load();
