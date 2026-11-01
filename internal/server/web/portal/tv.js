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
      i18n.msg(byId('status'), () => t('tv.disabled'));
      return;
    }
    byId('playlist').value = info.playlistUrl;
  } catch {
    i18n.msg(byId('status'), () => t('tv.loadFailed'));
  }
}

byId('copy').addEventListener('click', async () => {
  const input = byId('playlist');
  input.select();
  try {
    await navigator.clipboard.writeText(input.value);
    i18n.msg(byId('status'));
  } catch {
    i18n.msg(byId('status'), () => t('tv.copyByHand'));
  }
});

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

load();
