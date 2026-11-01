fetch('/api/me')
  .then((response) => (response.ok ? response.json() : null))
  .then((me) => {
    if (!me || !me.admin) return;
    const link = document.createElement('a');
    link.className = 'link';
    link.href = '/admin/users';
    link.textContent = 'Users';
    document.querySelector('nav.actions').prepend(link);
  })
  .catch(() => {});
