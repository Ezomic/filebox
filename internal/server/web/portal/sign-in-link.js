const form = document.getElementById('link-form');
const errorBox = document.getElementById('error');
const done = document.getElementById('done');
const button = form.querySelector('button');

fetch('/api/forgot')
  .then((response) => (response.ok ? response.json() : null))
  .then((status) => {
    if (status && !status.enabled) {
      document.getElementById('intro').textContent = 'Sign-in links are not set up on this server. Sign in with your password.';
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
    const response = await fetch('/api/sign-in-link', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: document.getElementById('email').value.trim() }),
    });
    const body = await response.json().catch(() => ({}));
    if (response.ok) {
      done.textContent = body.message || 'If that address belongs to an account, we sent a link.';
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
