const byId = (id) => document.getElementById(id);
const statusBox = byId('status');
const output = byId('log-output');

async function load() {
  const query = new URLSearchParams({
    source: byId('log-source').value,
    lines: byId('log-lines').value,
    filter: byId('log-filter').value,
  });
  try {
    const response = await fetch(`/api/logs?${query}`);
    if (response.status === 401) {
      window.location.href = '/login';
      return;
    }
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(body.error || 'Something went wrong.');
    statusBox.textContent = '';
    output.textContent = body.lines.length > 0 ? body.lines.join('\n') : 'No matching log lines.';
    output.scrollTop = output.scrollHeight;
  } catch (error) {
    statusBox.textContent = error.message;
  }
}

byId('log-form').addEventListener('submit', (event) => {
  event.preventDefault();
  load();
});

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

load();
