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
