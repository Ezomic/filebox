const byId = (id) => document.getElementById(id);

async function load() {
  try {
    const response = await fetch('/api/tv');
    if (response.status === 401) {
      window.location.href = '/login';
      return;
    }
    const info = await response.json();
    if (!info.enabled) {
      byId('status').textContent = 'TV is not set up on this server.';
      return;
    }
    byId('playlist').value = info.playlistUrl;
  } catch {
    byId('status').textContent = 'Could not load the link.';
  }
}

byId('copy').addEventListener('click', async () => {
  const input = byId('playlist');
  input.select();
  try {
    await navigator.clipboard.writeText(input.value);
    byId('status').textContent = '';
  } catch {
    byId('status').textContent = 'Copy it by hand: the text is selected.';
  }
});

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

load();
