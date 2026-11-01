(() => {
  const header = document.querySelector('header.bar');
  if (!header) return;
  const REFRESH_MS = 30000;
  const LABELS = { warning: 'Warning', critical: 'Almost full', full: 'Full' };

  const banner = document.createElement('div');
  banner.className = 'storage-banner';
  banner.hidden = true;
  header.after(banner);

  const format = (bytes) => {
    if (bytes < 1024) return `${bytes} B`;
    const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
    let value = bytes;
    let unit = -1;
    do {
      value /= 1024;
      unit += 1;
    } while (value >= 1024 && unit < units.length - 1);
    return `${value.toFixed(value < 10 ? 1 : 0)} ${units[unit]}`;
  };

  function render(data) {
    if (data.level === 'ok') {
      banner.hidden = true;
      banner.replaceChildren();
      return;
    }
    const label = document.createElement('strong');
    label.textContent = `${LABELS[data.level]}: `;
    const detail = document.createElement('span');
    const state = `The data disk is ${Math.floor(data.percent)}% full, ${format(data.free)} free.`;
    detail.textContent = data.level === 'full' ? `${state} Uploads in the web portal are refused until space is freed.` : state;
    banner.replaceChildren(label, detail);
    banner.className = `storage-banner ${data.level}`;
    banner.setAttribute('role', data.level === 'full' ? 'alert' : 'status');
    banner.hidden = false;
  }

  async function refresh() {
    try {
      const response = await fetch('/api/storage');
      if (response.ok) render(await response.json());
    } catch {
      return;
    }
  }

  refresh();
  setInterval(refresh, REFRESH_MS);
})();
