const REFRESH_MS = 3000;
const statusBox = document.getElementById('status');

const byId = (id) => document.getElementById(id);

function formatBytes(bytes) {
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`;
}

function formatUptime(seconds) {
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m`;
}

function percent(used, total) {
  return total > 0 ? (used / total) * 100 : 0;
}

function setMeter(name, pct) {
  byId(`${name}-value`).textContent = `${pct.toFixed(0)}%`;
  byId(`${name}-bar`).style.width = `${Math.min(pct, 100)}%`;
}

function render(metrics) {
  setMeter('cpu', metrics.cpuPercent);
  setMeter('mem', percent(metrics.memUsed, metrics.memTotal));
  byId('mem-detail').textContent = `${formatBytes(metrics.memUsed)} of ${formatBytes(metrics.memTotal)}`;
  setMeter('disk', percent(metrics.diskUsed, metrics.diskTotal));
  byId('disk-detail').textContent = `${formatBytes(metrics.diskUsed)} of ${formatBytes(metrics.diskTotal)}`;
  byId('uptime-value').textContent = formatUptime(metrics.uptimeSeconds);
  byId('temp-value').textContent = metrics.tempCelsius === null ? 'n/a' : `${metrics.tempCelsius.toFixed(0)} °C`;
}

async function refresh() {
  try {
    const response = await fetch('/api/metrics');
    if (response.status === 401) {
      window.location.href = '/login';
      return;
    }
    if (!response.ok) throw new Error('bad status');
    render(await response.json());
    statusBox.textContent = '';
  } catch {
    statusBox.textContent = 'Could not load the latest numbers.';
  }
}

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

refresh();
setInterval(refresh, REFRESH_MS);
