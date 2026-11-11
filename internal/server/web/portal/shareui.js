const EXPIRY_CHOICES = [
  ['never', 'shares.expiryNever'],
  ['1d', 'shares.expiry1d'],
  ['7d', 'shares.expiry7d'],
  ['30d', 'shares.expiry30d'],
];

const shareUrl = (name) => `${window.location.origin}/${name}`;

function expirySelect(label) {
  const select = document.createElement('select');
  select.setAttribute('aria-label', label);
  EXPIRY_CHOICES.forEach(([value, key]) => {
    const option = new Option(t(key), value);
    option.setAttribute('data-i18n', key);
    select.append(option);
  });
  return select;
}

function shareExpiryText(share) {
  if (share.expires === null) return t('shares.neverExpires');
  const when = i18n.dateTime(share.expires);
  return share.expired ? t('shares.expiredOn', { when }) : t('shares.expiresOn', { when });
}

function uploadUsageText(link) {
  const received = `${t('uploadLinks.received', { count: link.received })}, ${t('uploadLinks.size', { size: i18n.size(link.receivedBytes), cap: i18n.size(link.cap) })}`;
  return link.lastUpload === null ? received : `${received}, ${t('uploadLinks.lastUpload', { when: i18n.dateTime(link.lastUpload) })}`;
}

async function copyShareUrl(url, report) {
  try {
    await navigator.clipboard.writeText(url);
    report(() => t('shares.copied'));
  } catch {
    report(() => t('shares.copyFailed', { url }));
  }
}
