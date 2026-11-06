const nav = document.querySelector('nav.actions');
const key = 'filebox-admin';

function setLink(admin) {
  if (!nav) return;
  const existing = nav.querySelector('a[href="/admin/users"]');
  if (!admin) {
    if (existing && !existing.hasAttribute('aria-current')) existing.remove();
    return;
  }
  if (existing) return;
  const link = document.createElement('a');
  link.className = 'link';
  link.href = '/admin/users';
  link.textContent = t('nav.users');
  link.setAttribute('data-i18n', 'nav.users');
  nav.querySelector('a[href="/"]').after(link);
}

function addSharesLink() {
  const trash = nav?.querySelector('a[href="/trash"]');
  if (!trash || nav.querySelector('a[href="/shares"]')) return;
  const link = document.createElement('a');
  link.className = 'link';
  link.href = '/shares';
  link.textContent = t('nav.shares');
  link.setAttribute('data-i18n', 'nav.shares');
  trash.after(link);
}

addSharesLink();

// The last answer is remembered for this tab so the link is already there on the next page, not added after a request.
try {
  setLink(sessionStorage.getItem(key) === '1');
} catch {}

fetch('/api/me')
  .then((response) => (response.ok ? response.json() : null))
  .then((me) => {
    if (!me) return;
    setLink(me.admin);
    try {
      sessionStorage.setItem(key, me.admin ? '1' : '0');
    } catch {}
  })
  .catch(() => {});
