const form = document.getElementById('login-form');
const errorBox = document.getElementById('error');

fetch('/api/forgot')
  .then((response) => (response.ok ? response.json() : null))
  .then((status) => {
    if (status && status.enabled) {
      document.getElementById('link-option').hidden = false;
    }
  })
  .catch(() => {});

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  i18n.msg(errorBox);

  const button = form.querySelector('button');
  button.disabled = true;
  try {
    const response = await fetch('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        username: document.getElementById('username').value.trim(),
        password: document.getElementById('password').value,
      }),
    });
    if (response.ok) {
      window.location.href = '/';
      return;
    }
    const body = await response.json().catch(() => ({}));
    i18n.report(errorBox, i18n.failure(body, 'error.generic'));
  } catch {
    i18n.msg(errorBox, () => t('error.network'));
  } finally {
    button.disabled = false;
  }
});
