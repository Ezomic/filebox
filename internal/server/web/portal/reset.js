const form = document.getElementById('reset-form');
const errorBox = document.getElementById('error');

// The link carries its token after the #, which the browser never sends to the server or writes in logs.
const token = window.location.hash.slice(1);
if (token) {
  history.replaceState(null, '', window.location.pathname);
}

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  i18n.msg(errorBox);

  const password = document.getElementById('password').value;
  if (document.getElementById('confirm').value !== password) {
    i18n.msg(errorBox, () => t('password.mismatch'));
    return;
  }

  const button = form.querySelector('button');
  button.disabled = true;
  try {
    const response = await fetch('/api/reset', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token, password }),
    });
    if (response.ok) {
      window.location.href = '/login';
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
