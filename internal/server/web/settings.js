const byId = (id) => document.getElementById(id);
const form = byId('alert-form');
const field = byId('alert-address');
const errorBox = byId('alert-error');
const statusBox = byId('alert-status');
const testButton = byId('alert-test');
let saved = '';
let mail = false;

async function call(method, url, body) {
  const options = { method };
  if (body !== undefined) {
    options.headers = { 'Content-Type': 'application/json' };
    options.body = JSON.stringify(body);
  }
  const response = await fetch(url, options);
  if (response.status === 401) {
    window.location.href = '/login';
    throw new Error('You are signed out.');
  }
  if (!response.ok) {
    const data = await response.json().catch(() => ({}));
    throw new Error(data.error || 'Something went wrong.');
  }
  return response;
}

function showState() {
  byId('mail-off').hidden = mail;
  testButton.disabled = !mail || saved === '';
}

async function load() {
  try {
    const status = await (await call('GET', '/api/alerts')).json();
    saved = status.address;
    mail = status.mail;
    field.value = saved;
    showState();
  } catch (error) {
    errorBox.textContent = error.message;
  }
}

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  errorBox.textContent = '';
  statusBox.textContent = '';
  const address = field.value.trim();
  try {
    await call('PUT', '/api/alerts/address', { address });
    saved = address;
    field.value = address;
    showState();
    statusBox.textContent = address ? `Saved. Alert emails go to ${address}.` : 'Saved. No alert emails go out now.';
  } catch (error) {
    errorBox.textContent = error.message;
  }
});

testButton.addEventListener('click', async () => {
  errorBox.textContent = '';
  statusBox.textContent = '';
  if (field.value.trim() !== saved) {
    errorBox.textContent = 'Save the address first.';
    return;
  }
  testButton.disabled = true;
  try {
    await call('POST', '/api/alerts/test');
    statusBox.textContent = `Test email sent to ${saved}. If it does not arrive, look in the spam folder.`;
  } catch (error) {
    errorBox.textContent = error.message;
  }
  showState();
});

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

load();
