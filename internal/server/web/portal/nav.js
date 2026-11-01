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
  link.textContent = 'Users';
  nav.querySelector('a[href="/"]').after(link);
}

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
