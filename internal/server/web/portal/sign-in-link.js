const form = document.getElementById('link-form');
const errorBox = document.getElementById('error');
const done = document.getElementById('done');
const button = form.querySelector('button');

fetch('/api/forgot')
  .then((response) => (response.ok ? response.json() : null))
  .then((status) => {
    if (status && !status.enabled) {
      i18n.msg(document.getElementById('intro'), () => t('link.disabled'));
      button.disabled = true;
    }
  })
  .catch(() => {});

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  i18n.msg(errorBox);
  done.hidden = true;

  button.disabled = true;
  try {
    const response = await fetch('/api/sign-in-link', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: document.getElementById('email').value.trim(), lang: i18n.lang() }),
    });
    const body = await response.json().catch(() => ({}));
    if (response.ok) {
      i18n.answer(done, body, 'answer.sign_in_link_sent');
      done.hidden = false;
      return;
    }
    i18n.report(errorBox, i18n.failure(body, 'error.generic'));
  } catch {
    i18n.msg(errorBox, () => t('error.network'));
  } finally {
    button.disabled = false;
  }
});
