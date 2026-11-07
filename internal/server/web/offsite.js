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
    incomplete: ['finished, but some files could not be read', 'error'],
    backup_failed: [`failed. ${JOURNAL}`, 'error'],
    prune_failed: [`backed up, but old backups could not be removed. ${JOURNAL}`, 'error'],
    prepare_failed: [`could not start: no accounts, an account without its folder, or the backup disk not mounted. ${JOURNAL}`, 'error'],
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

  function runRows(view) {
    const { run } = view;
    if (!run) return [row('Last run', 'none yet: the backup runs every night at 01:00')];
    let [text, className] = RESULTS[run.result] || [run.result, 'error'];
    if (run.result === 'incomplete' && view.unreadableNights > 1) text += `, ${view.unreadableNights} nights in a row`;
    if (run.result === 'incomplete') text += `. ${JOURNAL}`;
    return [
      row('Last run', `${when(run.started)}, ${text}`, className),
      row('Duration', duration(run.seconds)),
      row('Data added', run.added === null ? 'not known for a run that did not finish' : bytes(run.added)),
    ];
  }

  function checkText(check) {
    if (!check) return ['not yet: the first one runs after the first complete backup', 'detail'];
    if (check.ok) return [`passed on ${day(check.at)}`, 'detail'];
    return [`found damage on ${day(check.at)}; the next night checks again. ${JOURNAL}`, 'error'];
  }

  function checkRow(view) {
    let [text, className] = checkText(view.check);
    if (view.checkErrorNights > 0) {
      const span = view.checkErrorNights === 1 ? 'last night' : `the last ${view.checkErrorNights} nights in a row`;
      text += `. It could not run to the end ${span}, which says nothing about the data; it is tried every night.`;
      if (view.checkErrorNights >= 3) className = 'error';
    }
    return row('Monthly check', text, className);
  }

  function summaryText(view) {
    if (view.latest) return `Last complete backup: ${when(view.latest)}.`;
    const since = view.setup ? ` It was set up on ${day(view.setup)}.` : '';
    return `The first complete backup has not finished yet. A large first upload takes several nights.${since}`;
  }

  function render(view) {
    card.hidden = !view.installed;
    if (!view.installed) return;
    if (!view.reported || !view.configured) {
      summary.textContent = 'Not set up yet. On the box, run: sudo filebox-backup setup';
      rows.replaceChildren();
      return;
    }
    summary.textContent = summaryText(view);
    rows.replaceChildren(
      row('Destination', destination(view)),
      ...runRows(view),
      row('Repository size', view.size
        ? `${bytes(view.size.bytes)}, measured on ${day(view.size.measured)} (once a week)`
        : 'measured after the first complete backup'),
      checkRow(view),
    );
  }

  async function refresh() {
    try {
      const response = await fetch('/api/offsite');
      if (response.status === 401) return;
      if (!response.ok) throw new Error('bad status');
      render(await response.json());
    } catch {
      card.hidden = false;
      rows.replaceChildren();
      summary.textContent = 'Could not load the status of the offsite backup.';
    }
  }

  refresh();
  setInterval(refresh, REFRESH_MS);
})();
