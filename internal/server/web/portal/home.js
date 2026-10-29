async function loadUser() {
  const response = await fetch('/api/me');
  if (!response.ok) {
    window.location.href = '/login';
    return;
  }
  const { user } = await response.json();
  document.getElementById('user').textContent = user;
}

document.getElementById('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

loadUser();
