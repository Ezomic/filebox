const form = document.getElementById('password-form');
const errorBox = document.getElementById('error');

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  errorBox.textContent = '';

  const password = document.getElementById('password').value;
  if (document.getElementById('confirm').value !== password) {
    errorBox.textContent = 'The new passwords do not match.';
    return;
  }

  const button = form.querySelector('button');
  button.disabled = true;
  try {
    const response = await fetch('/api/me/password', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ current: document.getElementById('current').value, password }),
    });
    if (response.ok) {
      window.location.href = '/login';
      return;
    }
    if (response.status === 401 && (await response.clone().json().catch(() => ({}))).error === 'not signed in') {
      window.location.href = '/login';
      return;
    }
    const body = await response.json().catch(() => ({}));
    errorBox.textContent = body.error || 'Something went wrong.';
  } catch {
    errorBox.textContent = 'Could not reach the server.';
  } finally {
    button.disabled = false;
  }
});

const emailForm = document.getElementById('email-form');
const emailError = document.getElementById('email-error');
const emailDone = document.getElementById('email-done');

function showEmail(state) {
  const input = document.getElementById('email');
  if (!state.email) {
    document.getElementById('email-state').textContent = 'No address saved yet.';
    return;
  }
  input.value = state.email;
  document.getElementById('email-state').textContent = state.email + (state.confirmed ? ' (confirmed)' : ' (not confirmed yet, check your inbox)');
}

fetch('/api/me/email')
  .then((response) => (response.ok ? response.json() : null))
  .then((state) => {
    if (!state || !state.enabled) return;
    emailForm.hidden = false;
    showEmail(state);
  })
  .catch(() => {});

emailForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  emailError.textContent = '';
  emailDone.hidden = true;

  const button = emailForm.querySelector('button');
  button.disabled = true;
  try {
    const response = await fetch('/api/me/email', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        email: document.getElementById('email').value.trim(),
        current: document.getElementById('email-current').value,
      }),
    });
    if (response.ok) {
      document.getElementById('email-current').value = '';
      emailDone.textContent = 'Check your inbox for the confirmation link.';
      emailDone.hidden = false;
      const state = await fetch('/api/me/email').then((r) => r.json());
      showEmail(state);
      return;
    }
    const body = await response.json().catch(() => ({}));
    emailError.textContent = body.error || 'Something went wrong.';
  } catch {
    emailError.textContent = 'Could not reach the server.';
  } finally {
    button.disabled = false;
  }
});
