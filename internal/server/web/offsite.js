(() => {
  const REFRESH_MS = 60000;
  const JOURNAL = 'See: journalctl -u filebox-backup';
  const card = document.getElementById('backup-card');
  const summary = document.getElementById('backup-summary');
  const rows = document.getElementById('backup-rows');

  const KINDS = {
    b2: 'Backblaze B2',
    azure: 'Azure Blob Storage',
    gs: 'Google Cloud Storage',
    swift: 'OpenStack Swift',
    rclone: 'rclone',
    local: 'a folder on this box',
  };
  const RESULTS = {
    ok: ['finished', 'detail'],
    stopped: ['stopped at the 6 hour limit; the next night carries on', 'detail'],
    incomplete: [`finished, but some files could not be read. ${JOURNAL}`, 'error'],
    backup_failed: [`failed. ${JOURNAL}`, 'error'],
    prune_failed: [`backed up, but old backups could not be removed. ${JOURNAL}`, 'error'],
    prepare_failed: [`could not start: the accounts or folders could not be read. ${JOURNAL}`, 'error'],
  };

  const when = (value) => new Date(value).toLocaleString();
  const day = (value) => new Date(value).toLocaleDateString();

  function bytes(value) {
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let size = value;
    let unit = 0;
    while (size >= 1024 && unit < units.length - 1) {
      size /= 1024;
      unit += 1;
    }
    return `${size.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`;
  }

  function duration(seconds) {
    const hours = Math.floor(seconds / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);
    if (hours > 0) return `${hours}h ${minutes}m`;
    if (minutes > 0) return `${minutes}m`;
    return `${seconds}s`;
  }

  function row(label, text, className) {
    const node = document.createElement('tr');
    const name = document.createElement('td');
    name.className = 'name';
    name.textContent = label;
    const value = document.createElement('td');
    value.className = className || 'detail';
    value.textContent = text;
    node.append(name, value);
    return node;
  }

  function destination(view) {
    if (view.host) return view.host;
    return KINDS[view.kind] || view.kind || '-';
  }

  function runRows(run) {
    if (!run) return [row('Last run', 'none yet: the backup runs every night at 01:00')];
    const [text, className] = RESULTS[run.result] || [run.result, 'error'];
    return [
      row('Last run', `${when(run.started)}, ${text}`, className),
      row('Duration', duration(run.seconds)),
      row('Data added', run.added === null ? 'not known for a run that did not finish' : bytes(run.added)),
    ];
  }

  function checkRow(check) {
    if (!check) return row('Monthly check', 'not yet: the first one runs after the first complete backup');
    if (check.ok) return row('Monthly check', `passed on ${day(check.at)}`);
    return row('Monthly check', `failed on ${day(check.at)}; the next night checks again. ${JOURNAL}`, 'error');
  }

  function render(view) {
    card.hidden = !view.installed;
    if (!view.installed) return;
    if (!view.reported || !view.configured) {
      summary.textContent = 'Not set up yet. On the box, run: sudo filebox-backup setup';
      rows.replaceChildren();
      return;
    }
    summary.textContent = view.latest
      ? `Last complete backup: ${when(view.latest)}.`
      : 'The first complete backup has not finished yet. A large first upload takes several nights.';
    rows.replaceChildren(
      row('Destination', destination(view)),
      ...runRows(view.run),
      row('Repository size', view.size ? `${bytes(view.size.bytes)} on ${day(view.size.measured)}` : 'measured after the first complete backup'),
      checkRow(view.check),
    );
  }

  async function refresh() {
    try {
      const response = await fetch('/api/offsite');
      if (response.status === 401) return;
      if (!response.ok) throw new Error('bad status');
      render(await response.json());
    } catch {
      summary.textContent = 'Could not load the status of the offsite backup.';
    }
  }

  refresh();
  setInterval(refresh, REFRESH_MS);
})();
