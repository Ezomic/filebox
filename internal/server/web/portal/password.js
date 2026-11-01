const form = document.getElementById('password-form');
const errorBox = document.getElementById('error');

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
    const response = await fetch('/api/me/password', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ current: document.getElementById('current').value, password }),
    });
    if (response.ok) {
      window.location.href = '/login';
      return;
    }
    const body = await response.json().catch(() => ({}));
    if (response.status === 401 && body.code === 'not_signed_in') {
      window.location.href = '/login';
      return;
    }
    i18n.report(errorBox, i18n.failure(body, 'error.generic'));
  } catch {
    i18n.msg(errorBox, () => t('error.network'));
  } finally {
    button.disabled = false;
  }
});

const emailForm = document.getElementById('email-form');
const emailError = document.getElementById('email-error');
const emailDone = document.getElementById('email-done');
const emailState = document.getElementById('email-state');

function showEmail(state) {
  if (!state.email) {
    i18n.msg(emailState, () => t('email.none'));
    return;
  }
  document.getElementById('email').value = state.email;
  i18n.msg(emailState, () => t(state.confirmed ? 'email.confirmed' : 'email.unconfirmed', { email: state.email }));
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
  i18n.msg(emailError);
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
        lang: i18n.lang(),
      }),
    });
    if (response.ok) {
      document.getElementById('email-current').value = '';
      i18n.msg(emailDone, () => t('email.sent'));
      emailDone.hidden = false;
      const state = await fetch('/api/me/email').then((r) => r.json());
      showEmail(state);
      return;
    }
    const body = await response.json().catch(() => ({}));
    i18n.report(emailError, i18n.failure(body, 'error.generic'));
  } catch {
    i18n.msg(emailError, () => t('error.network'));
  } finally {
    button.disabled = false;
  }
});
