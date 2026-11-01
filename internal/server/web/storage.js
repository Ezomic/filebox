(() => {
  const header = document.querySelector('header.bar');
  if (!header) return;
  const onTrash = window.location.pathname === '/trash';

  const el = (tag, text, className) => {
    const node = document.createElement(tag);
    if (text !== undefined) node.textContent = text;
    if (className) node.className = className;
    return node;
  };

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

  const box = el('section', undefined, 'storage');
  box.setAttribute('aria-label', 'Storage');
  box.hidden = true;
  const text = el('div', undefined, 'storage-text');
  text.id = 'storage-text';
  const meter = el('div', undefined, 'meter');
  meter.setAttribute('role', 'progressbar');
  meter.setAttribute('aria-labelledby', 'storage-text');
  meter.setAttribute('aria-valuemin', '0');
  meter.setAttribute('aria-valuemax', '100');
  const fill = el('span');
  meter.append(fill);
  const banner = el('div', undefined, 'storage-banner');
  banner.hidden = true;
  box.append(text, meter, banner);
  header.after(box);

  const LABELS = { warning: 'Warning', critical: 'Almost full', full: 'Full' };

  function summary(data) {
    if (data.measure === 'quota') return `${format(data.quotaUsed)} of ${format(data.quota)} used`;
    return `${format(data.used)} used, ${format(data.free)} free`;
  }

  function headline(data, percent) {
    if (data.level === 'full') return 'The disk is full. New uploads are paused until space is freed. You can still download and delete files.';
    if (data.measure === 'quota') return `You have used ${percent}% of your storage.`;
    return `The disk is ${percent}% full.`;
  }

  function trashLine(data) {
    if (data.trashBytes <= 0) return null;
    const line = el('span', `Your trash holds ${format(data.trashBytes)}, which still counts as used. `);
    if (onTrash) {
      line.append('Empty it below to free space.');
      return line;
    }
    const link = el('a', 'Empty trash to free space', 'link');
    link.href = '/trash';
    line.append(link);
    return line;
  }

  function showBanner(data) {
    if (data.level === 'ok') {
      banner.hidden = true;
      banner.replaceChildren();
      return;
    }
    const parts = [el('strong', `${LABELS[data.level]}: `), el('span', `${headline(data, Math.floor(data.percent))} `)];
    const trash = trashLine(data);
    if (trash) parts.push(trash);
    banner.replaceChildren(...parts);
    banner.className = `storage-banner ${data.level}`;
    banner.setAttribute('role', data.level === 'full' ? 'alert' : 'status');
    banner.hidden = false;
  }

  function render(data) {
    const percent = Math.floor(data.percent);
    const description = `${summary(data)} (${percent}%)`;
    text.textContent = description;
    meter.setAttribute('aria-valuenow', String(percent));
    meter.setAttribute('aria-valuetext', description);
    fill.style.width = `${Math.min(data.percent, 100)}%`;
    box.dataset.level = data.level;
    box.hidden = false;
    showBanner(data);
  }

  async function refresh() {
    try {
      const response = await fetch('/api/storage');
      if (!response.ok) return null;
      const data = await response.json();
      render(data);
      return data;
    } catch {
      return null;
    }
  }

  window.refreshStorage = refresh;
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) refresh();
  });
  refresh();
})();
