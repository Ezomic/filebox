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

  const format = (bytes) => i18n.size(bytes);

  const box = el('section', undefined, 'storage');
  box.setAttribute('aria-label', t('storage.label'));
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

  const LABELS = { warning: 'storage.levelWarning', critical: 'storage.levelCritical', full: 'storage.levelFull' };

  function summary(data) {
    if (data.measure === 'quota') return t('storage.quotaSummary', { used: format(data.quotaUsed), quota: format(data.quota) });
    return t('storage.diskSummary', { used: format(data.used), free: format(data.free) });
  }

  function headline(data, percent) {
    if (data.level === 'full') return t('storage.full');
    if (data.measure === 'quota') return t('storage.quotaPercent', { percent });
    return t('storage.diskPercent', { percent });
  }

  function trashLine(data) {
    if (data.trashBytes <= 0) return null;
    const line = el('span', `${t('storage.trashHolds', { size: format(data.trashBytes) })} `);
    if (onTrash) {
      line.append(t('storage.emptyBelow'));
      return line;
    }
    const link = el('a', t('storage.emptyLink'), 'link');
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
    const parts = [el('strong', `${t(LABELS[data.level])}: `), el('span', `${headline(data, Math.floor(data.percent))} `)];
    const trash = trashLine(data);
    if (trash) parts.push(trash);
    banner.replaceChildren(...parts);
    banner.className = `storage-banner ${data.level}`;
    banner.setAttribute('role', data.level === 'full' ? 'alert' : 'status');
    banner.hidden = false;
  }

  let latest = null;

  function render(data) {
    latest = data;
    box.setAttribute('aria-label', t('storage.label'));
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

  i18n.onChange(() => {
    if (latest) render(latest);
  });

  window.refreshStorage = refresh;
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) refresh();
  });
  refresh();
})();
