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
    const { users } = await api('GET', '/api/users');
    showError('');
    render(users);
  } catch (error) {
    showError(error.message === 'signed out' ? '' : 'Could not load the users.');
  }
}

function render(users) {
  rows.replaceChildren(...users.map((user) => userRow(user.name)));
  byId('empty').hidden = users.length > 0;
}

function userRow(name) {
  const row = el('tr');
  const actions = el('td', undefined, 'row-actions');
  row.append(el('td', name, 'name'), actions);
  showActions(actions, name);
  return row;
}

function showActions(cell, name) {
  cell.replaceChildren(
    button('Change password', () => showPasswordForm(cell, name), 'link'),
    button('Delete', () => showDeleteConfirm(cell, name), 'link danger'),
  );
}

function showPasswordForm(cell, name) {
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
  form.append(input, save, button('Cancel', () => showActions(cell, name), 'link'), message);

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    save.disabled = true;
    try {
      await api('PUT', `/api/users/${encodeURIComponent(name)}/password`, { password: input.value });
      input.value = '';
      showActions(cell, name);
      showError('');
    } catch (error) {
      message.textContent = error.message;
      save.disabled = false;
    }
  });

  cell.replaceChildren(form);
  input.focus();
}

function showDeleteConfirm(cell, name) {
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
    button('Cancel', () => showActions(cell, name), 'link'),
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
