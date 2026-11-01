const form = document.getElementById('sign-in-form');
const errorBox = document.getElementById('error');

// The token stays in the fragment and is only used when the button is pressed, so a mail scanner that opens the link does not use it up.
const token = window.location.hash.slice(1);
if (token) {
  history.replaceState(null, '', window.location.pathname);
}

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  errorBox.textContent = '';

  const button = form.querySelector('button');
  button.disabled = true;
  try {
    const response = await fetch('/api/sign-in', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token }),
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
