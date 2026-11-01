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

async function refreshPortal() {
  try {
    const response = await fetch('/api/portal');
    if (!response.ok) return;
    const portal = await response.json();
    byId('portal-card').hidden = !portal.enabled;
    if (!portal.enabled) return;
    byId('portal-value').textContent = portal.certDaysLeft >= 0 ? `${portal.certDaysLeft} days` : 'expired';
    byId('portal-detail').textContent = `Expires ${portal.certExpires.slice(0, 10)}`;
    byId('portal-card').classList.toggle('warn', portal.certDaysLeft < 14);
  } catch {
    byId('portal-card').hidden = true;
  }
}

async function showVersion() {
  try {
    const response = await fetch('/api/version');
    if (response.ok) byId('version').textContent = `filebox ${(await response.json()).version}`;
  } catch {
    byId('version').textContent = '';
  }
}

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});

refresh();
setInterval(refresh, REFRESH_MS);
showVersion();
refreshPortal();
setInterval(refreshPortal, 60 * 60 * 1000);

const POWER_MESSAGES = {
  'restart-sshd': 'sshd restarted.',
  reboot: 'Rebooting. The panel will be back in a minute or two.',
  shutdown: 'Shutting down. Power the machine on by hand to bring it back.',
};
const CONFIRM_RESET_MS = 5000;
const powerStatus = byId('power-status');
let armed = null;
let armTimer = null;

function disarm() {
  clearTimeout(armTimer);
  if (armed) armed.textContent = armed.dataset.label;
  armed = null;
}

async function runPower(button) {
  const action = button.dataset.action;
  disarm();
  powerStatus.textContent = '';
  for (const other of document.querySelectorAll('#power-actions button')) other.disabled = true;
  try {
    const response = await fetch(`/api/system/${action}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ confirm: true }),
    });
    if (response.status === 401) {
      window.location.href = '/login';
      return;
    }
    if (!response.ok) throw new Error('bad status');
    powerStatus.textContent = POWER_MESSAGES[action];
    if (action === 'restart-sshd') {
      for (const other of document.querySelectorAll('#power-actions button')) other.disabled = false;
    }
  } catch {
    powerStatus.textContent = 'That did not work. Check the panel log.';
    for (const other of document.querySelectorAll('#power-actions button')) other.disabled = false;
  }
}

byId('power-actions').addEventListener('click', (event) => {
  const button = event.target.closest('button[data-action]');
  if (!button) return;
  if (armed !== button) {
    disarm();
    armed = button;
    button.textContent = `Click again to ${button.dataset.label.toLowerCase()}`;
    armTimer = setTimeout(disarm, CONFIRM_RESET_MS);
    return;
  }
  runPower(button);
});

const DISK_REFRESH_MS = 60000;

function diskRow(disk) {
  const row = document.createElement('div');
  row.className = 'disk';
  const health = disk.healthy === null ? 'unknown' : disk.healthy ? 'healthy' : 'FAILING';
  const cells = [
    [disk.device, 'name'],
    [disk.model || '-', 'detail'],
    [disk.tempCelsius === null ? 'n/a' : `${disk.tempCelsius.toFixed(0)} °C`, 'detail'],
    [health, disk.healthy === false ? 'stale' : 'detail'],
  ];
  for (const [text, className] of cells) {
    const cell = document.createElement('span');
    cell.textContent = text;
    cell.className = className;
    row.append(cell);
  }
  return row;
}

async function refreshDisks() {
  const empty = byId('disks-empty');
  try {
    const response = await fetch('/api/disks');
    if (!response.ok) throw new Error('bad status');
    const { available, disks } = await response.json();
    byId('disk-rows').replaceChildren(...disks.map(diskRow));
    empty.hidden = disks.length > 0;
    empty.textContent = available ? 'No disks reported.' : 'Disk health is not available on this machine.';
  } catch {
    empty.hidden = false;
    empty.textContent = 'Could not load disk health.';
  }
}

refreshDisks();
setInterval(refreshDisks, DISK_REFRESH_MS);
