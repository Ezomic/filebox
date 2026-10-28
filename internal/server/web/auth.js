const form = document.getElementById('auth-form');
const errorBox = document.getElementById('error');
const confirmField = document.getElementById('confirm');

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  errorBox.textContent = '';

  const password = document.getElementById('password').value;
  if (confirmField && confirmField.value !== password) {
    errorBox.textContent = 'The passwords do not match.';
    return;
  }

  const button = form.querySelector('button');
  button.disabled = true;
  try {
    const response = await fetch(form.dataset.endpoint, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password }),
    });
    if (response.ok) {
      window.location.href = '/';
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
