const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const rows = byId('user-rows');
let me = '';
let users = [];

async function api(method, path, payload) {
  const options = { method };
  if (payload !== undefined) {
    options.headers = { 'Content-Type': 'application/json' };
    options.body = JSON.stringify(payload);
  }
  const response = await fetch(path, options);
  const body = response.status === 204 ? {} : await response.json().catch(() => ({}));
  if (response.status === 401 && body.code === 'not_signed_in') {
    window.location.href = '/login';
    throw new Error('signed out');
  }
  if (response.status === 403 && body.code === 'admins_only') {
    window.location.href = '/';
    throw new Error('not an admin');
  }
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

function passwordInput(label, autocomplete, limits) {
  const input = el('input');
  input.type = 'password';
  input.required = true;
  input.autocomplete = autocomplete;
  input.placeholder = label;
  input.setAttribute('aria-label', label);
  if (limits) {
    input.minLength = 12;
    input.maxLength = 128;
  }
  return input;
}

function renderUsers() {
  rows.replaceChildren(...users.map(userRow));
  byId('empty').hidden = users.length > 0;
}

async function load() {
  try {
    ({ users } = await api('GET', '/api/admin/users'));
    i18n.msg(statusBox);
    renderUsers();
  } catch (error) {
    if (error.message !== 'signed out' && error.message !== 'not an admin') i18n.msg(statusBox, () => t('admin.loadFailed'));
  }
}

function usageText(user) {
  if (user.bytes === null) return '';
  if (user.quota !== null) return t('admin.usage', { used: i18n.size(user.bytes, true), quota: i18n.size(user.quota, true) });
  return t('admin.usageNoLimit', { used: i18n.size(user.bytes, true) });
}

function userRow(user) {
  const row = el('tr');
  const name = el('td', user.name, 'name');
  if (user.admin) name.append(el('span', t('admin.badgeAdmin'), 'badge'));
  if (user.name === me) name.append(el('span', t('admin.badgeYou'), 'badge'));
  const over = user.quota !== null && user.bytes !== null && user.bytes >= user.quota;
  const usage = el('td', usageText(user), over ? 'error' : 'detail');
  const actions = el('td', undefined, 'row-actions');
  row.append(name, usage, actions);
  showActions(actions, user);
  return row;
}

function showActions(cell, user) {
  const buttons = [];
  if (!user.admin) buttons.push(button(t('admin.changePassword'), () => showPasswordForm(cell, user), 'link'));
  if (!user.admin && user.quotaReady) {
    buttons.push(button(t('admin.quota'), () => showQuotaForm(cell, user), 'link'));
    if (user.quota !== null) buttons.push(button(t('admin.noLimit'), () => showClearQuotaForm(cell, user), 'link'));
  }
  if (user.name !== me) {
    buttons.push(
      button(user.admin ? t('admin.removeAdmin') : t('admin.makeAdmin'), () => showRoleForm(cell, user), 'link'),
      button(t('common.delete'), () => showDeleteForm(cell, user), 'link danger'),
    );
  }
  cell.replaceChildren(...buttons);
}

function showForm(cell, user, { notice, inputs, submit, danger, run }) {
  const form = el('form', undefined, 'inline');
  const message = el('span', undefined, 'error');
  const save = el('button', submit, danger ? 'danger-solid' : undefined);
  save.type = 'submit';
  form.append(...(notice ? [el('span', notice, danger ? 'error' : 'detail')] : []), ...inputs, save, button(t('common.cancel'), () => showActions(cell, user), 'link'), message);

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    save.disabled = true;
    try {
      await run();
    } catch (error) {
      i18n.report(message, error);
      save.disabled = false;
    }
  });

  cell.replaceChildren(form);
  inputs[0].focus();
}

function target(user, suffix = '') {
  return `/api/admin/users/${encodeURIComponent(user.name)}${suffix}`;
}

function showPasswordForm(cell, user) {
  const password = passwordInput(t('admin.newPasswordFor', { name: user.name }), 'new-password', true);
  const confirm = passwordInput(t('admin.yourPassword'), 'current-password');
  showForm(cell, user, {
    inputs: [password, confirm],
    submit: t('common.save'),
    run: async () => {
      await api('PUT', target(user, '/password'), { password: password.value, confirm: confirm.value });
      i18n.msg(statusBox);
      await load();
    },
  });
}

const QUOTA_UNITS = { MB: 1024 ** 2, GB: 1024 ** 3, TB: 1024 ** 4 };
const QUOTA_MIN = 1024 ** 2;
const QUOTA_MAX = 1024 ** 5;

function quotaBytes(amount, unit) {
  const bytes = Math.round(Number(amount) * QUOTA_UNITS[unit]);
  return Number.isSafeInteger(bytes) && bytes >= QUOTA_MIN && bytes <= QUOTA_MAX ? bytes : null;
}

function showQuotaForm(cell, user) {
  const amount = el('input');
  amount.type = 'number';
  amount.min = '0';
  amount.step = 'any';
  amount.required = true;
  amount.inputMode = 'decimal';
  amount.setAttribute('aria-label', t('admin.limitFor', { name: user.name }));
  const unit = el('select');
  unit.setAttribute('aria-label', t('admin.unit'));
  for (const name of Object.keys(QUOTA_UNITS)) unit.append(new Option(`${name[0]}iB`, name));
  unit.value = 'GB';
  if (user.quota !== null) {
    const best = Object.keys(QUOTA_UNITS).reverse().find((name) => user.quota >= QUOTA_UNITS[name]) || 'MB';
    unit.value = best;
    amount.value = String(Math.round((user.quota / QUOTA_UNITS[best]) * 100) / 100);
  }
  const confirm = passwordInput(t('admin.yourPassword'), 'current-password');
  showForm(cell, user, {
    notice: t('admin.quotaNotice', { name: user.name }),
    inputs: [amount, unit, confirm],
    submit: t('common.save'),
    run: async () => {
      const bytes = quotaBytes(amount.value, unit.value);
      if (bytes === null) throw i18n.problem('admin.quotaRange');
      await api('PUT', target(user, '/quota'), { bytes, confirm: confirm.value });
      i18n.msg(statusBox);
      await load();
    },
  });
}

function showClearQuotaForm(cell, user) {
  const confirm = passwordInput(t('admin.yourPassword'), 'current-password');
  showForm(cell, user, {
    notice: t('admin.clearNotice', { name: user.name }),
    inputs: [confirm],
    submit: t('admin.removeLimit'),
    run: async () => {
      await api('DELETE', target(user, '/quota'), { confirm: confirm.value });
      i18n.msg(statusBox);
      await load();
    },
  });
}

function showRoleForm(cell, user) {
  const confirm = passwordInput(t('admin.yourPassword'), 'current-password');
  const role = user.admin ? 'user' : 'admin';
  showForm(cell, user, {
    notice: user.admin ? t('admin.demoteNotice', { name: user.name }) : t('admin.promoteNotice', { name: user.name }),
    inputs: [confirm],
    submit: user.admin ? t('admin.removeAdmin') : t('admin.makeAdmin'),
    run: async () => {
      await api('PUT', target(user, '/role'), { role, confirm: confirm.value });
      i18n.msg(statusBox);
      await load();
    },
  });
}

function showDeleteForm(cell, user) {
  const confirm = passwordInput(t('admin.yourPassword'), 'current-password');
  showForm(cell, user, {
    notice: t('admin.deleteNotice', { name: user.name }),
    inputs: [confirm],
    submit: t('admin.deleteAccount'),
    danger: true,
    run: async () => {
      await api('DELETE', target(user), { confirm: confirm.value });
      i18n.msg(statusBox);
      await load();
    },
  });
}

byId('add-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const errorBox = byId('add-error');
  const submit = event.target.querySelector('button[type="submit"]');
  i18n.msg(errorBox);
  submit.disabled = true;
  try {
    await api('POST', '/api/admin/users', { name: byId('new-name').value, password: byId('new-password').value });
    event.target.reset();
    await load();
  } catch (error) {
    i18n.report(errorBox, error);
  } finally {
    submit.disabled = false;
  }
});

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

i18n.onChange(() => {
  if (!rows.querySelector('form')) renderUsers();
});

api('GET', '/api/me').then((body) => {
  me = body.user;
  return load();
}).catch(() => {});
