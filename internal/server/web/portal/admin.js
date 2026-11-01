const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const rows = byId('user-rows');
let me = '';

async function api(method, path, payload) {
  const options = { method };
  if (payload !== undefined) {
    options.headers = { 'Content-Type': 'application/json' };
    options.body = JSON.stringify(payload);
  }
  const response = await fetch(path, options);
  const body = response.status === 204 ? {} : await response.json().catch(() => ({}));
  if (response.status === 401 && body.error === 'not signed in') {
    window.location.href = '/login';
    throw new Error('signed out');
  }
  if (response.status === 403 && body.error === 'admins only') {
    window.location.href = '/';
    throw new Error('not an admin');
  }
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

async function load() {
  try {
    const { users } = await api('GET', '/api/admin/users');
    statusBox.textContent = '';
    rows.replaceChildren(...users.map(userRow));
    byId('empty').hidden = users.length > 0;
  } catch (error) {
    if (error.message !== 'signed out' && error.message !== 'not an admin') statusBox.textContent = 'Could not load the users.';
  }
}

function userRow(user) {
  const row = el('tr');
  const name = el('td', user.name, 'name');
  if (user.admin) name.append(el('span', 'Admin', 'badge'));
  if (user.name === me) name.append(el('span', 'You', 'badge'));
  const actions = el('td', undefined, 'row-actions');
  row.append(name, actions);
  showActions(actions, user);
  return row;
}

function showActions(cell, user) {
  const buttons = [];
  if (!user.admin) buttons.push(button('Change password', () => showPasswordForm(cell, user), 'link'));
  if (user.name !== me) {
    buttons.push(
      button(user.admin ? 'Remove admin' : 'Make admin', () => showRoleForm(cell, user), 'link'),
      button('Delete', () => showDeleteForm(cell, user), 'link danger'),
    );
  }
  cell.replaceChildren(...buttons);
}

function showForm(cell, user, { notice, inputs, submit, danger, run }) {
  const form = el('form', undefined, 'inline');
  const message = el('span', undefined, 'error');
  const save = el('button', submit, danger ? 'danger-solid' : undefined);
  save.type = 'submit';
  form.append(...(notice ? [el('span', notice, danger ? 'error' : 'detail')] : []), ...inputs, save, button('Cancel', () => showActions(cell, user), 'link'), message);

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    save.disabled = true;
    try {
      await run();
    } catch (error) {
      message.textContent = error.message;
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
  const password = passwordInput(`New password for ${user.name}`, 'new-password', true);
  const confirm = passwordInput('Your password', 'current-password');
  showForm(cell, user, {
    inputs: [password, confirm],
    submit: 'Save',
    run: async () => {
      await api('PUT', target(user, '/password'), { password: password.value, confirm: confirm.value });
      statusBox.textContent = '';
      await load();
    },
  });
}

function showRoleForm(cell, user) {
  const confirm = passwordInput('Your password', 'current-password');
  const role = user.admin ? 'user' : 'admin';
  showForm(cell, user, {
    notice: user.admin ? `${user.name} will no longer be able to manage accounts.` : `${user.name} will be able to create and delete accounts and reset passwords.`,
    inputs: [confirm],
    submit: user.admin ? 'Remove admin' : 'Make admin',
    run: async () => {
      await api('PUT', target(user, '/role'), { role, confirm: confirm.value });
      statusBox.textContent = '';
      await load();
    },
  });
}

function showDeleteForm(cell, user) {
  const confirm = passwordInput('Your password', 'current-password');
  showForm(cell, user, {
    notice: `This deletes the account ${user.name}. It cannot be undone.`,
    inputs: [confirm],
    submit: 'Delete account',
    danger: true,
    run: async () => {
      await api('DELETE', target(user), { confirm: confirm.value });
      statusBox.textContent = '';
      await load();
    },
  });
}

byId('add-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const errorBox = byId('add-error');
  const submit = event.target.querySelector('button[type="submit"]');
  errorBox.textContent = '';
  submit.disabled = true;
  try {
    await api('POST', '/api/admin/users', { name: byId('new-name').value, password: byId('new-password').value });
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

api('GET', '/api/me').then((body) => {
  me = body.user;
  return load();
}).catch(() => {});
