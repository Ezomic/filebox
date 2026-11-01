const form = document.getElementById('forgot-form');
const errorBox = document.getElementById('error');
const done = document.getElementById('done');
const button = form.querySelector('button');

fetch('/api/forgot')
  .then((response) => (response.ok ? response.json() : null))
  .then((status) => {
    if (status && !status.enabled) {
      document.getElementById('intro').textContent = 'Reset by email is not set up on this server. Ask the owner to reset your password.';
      button.disabled = true;
    }
  })
  .catch(() => {});

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  errorBox.textContent = '';
  done.hidden = true;

  button.disabled = true;
  try {
    const response = await fetch('/api/forgot', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: document.getElementById('username').value.trim() }),
    });
    const body = await response.json().catch(() => ({}));
    if (response.ok) {
      done.textContent = body.message || 'If that account exists and has a confirmed address, we sent an email.';
      done.hidden = false;
      return;
    }
    errorBox.textContent = body.error || 'Something went wrong.';
  } catch {
    errorBox.textContent = 'Could not reach the server.';
  } finally {
    button.disabled = false;
  }
});
