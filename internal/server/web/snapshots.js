(() => {
  const GUIDE = 'https://github.com/Ezomic/filebox/blob/main/docs/index.html#versions';
  const REFRESH_MS = 60000;
  const summary = document.getElementById('snapshot-summary');
  const table = document.getElementById('snapshot-rows');
  const heldNote = document.getElementById('snapshot-held');

  const REASONS = {
    not_btrfs: 'Earlier versions are off: the data disk is not btrfs.',
    no_btrfs_progs: 'Earlier versions are off: btrfs-progs is not installed.',
  };
  const ERRORS = {
    snapshot_failed: 'The last snapshot failed. See: journalctl -u filebox-snapshot',
    prune_failed: 'Old snapshots could not be deleted. See: journalctl -u filebox-snapshot',
  };

  function cell(text, className) {
    const node = document.createElement('td');
    node.textContent = text;
    if (className) node.className = className;
    return node;
  }

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

  const day = (value) => (value ? new Date(value).toLocaleDateString() : '-');
  const when = (value) => new Date(value).toLocaleString();

  function folderName(folder) {
    if (folder.kind === 'family') return 'Family';
    if (folder.kind === 'deleted') return `${folder.name.split('.deleted-')[0]} (deleted)`;
    return folder.name;
  }

  function protection(folder) {
    switch (folder.protection) {
      case 'yes':
        return ['protected', 'detail'];
      case 'gone':
        return ['kept until they expire', 'detail'];
      case 'unchecked':
        return ['not checked yet', 'detail'];
      default:
        return [folder.kind === 'family'
          ? 'not protected yet: the next update converts it, once the disk is below 95%'
          : `not protected: run sudo filebox-user convert ${folder.name}`, 'error'];
    }
  }

  function stateCell(folder) {
    const [state, stateClass] = protection(folder);
    const node = cell(state, stateClass);
    if (folder.error) {
      const problem = document.createElement('div');
      problem.className = 'error';
      problem.textContent = ERRORS[folder.error] || folder.error;
      node.append(problem);
    }
    return node;
  }

  function folderRow(folder) {
    const row = document.createElement('tr');
    const span = folder.count > 0 ? `${folder.count}, ${day(folder.oldest)} to ${day(folder.newest)}` : 'none yet';
    const held = folder.count > 0 && folder.held !== null ? `holds ${bytes(folder.held)}` : '-';
    row.append(cell(folderName(folder), 'name'), stateCell(folder), cell(span, 'detail'), cell(held, 'detail'));
    return row;
  }

  function summaryText(view) {
    if (!view.reported) return 'No report from filebox-snapshot yet. It checks every hour, so the first one appears soon after an update.';
    const parts = [view.daily
      ? `Last nightly run: ${when(view.daily)}${view.dailyOk ? '' : ', with errors'}.`
      : 'No nightly run yet: the first earlier versions are taken at 00:30.'];
    if (view.lowSpace) {
      parts.push(`On ${when(view.lowSpace.at)} the disk was 95% full and the ${view.lowSpace.deleted} oldest earlier versions were deleted${view.lowSpace.stillFull ? '; it is still that full, and every folder is down to its last 7' : ''}.`);
    }
    return parts.join(' ');
  }

  function render(view) {
    if (view.reported && !view.enabled) {
      const link = document.createElement('a');
      link.className = 'link';
      link.href = GUIDE;
      link.textContent = 'How to turn them on';
      summary.replaceChildren(`${REASONS[view.reason] || 'Earlier versions are off.'} `, link);
    } else {
      summary.textContent = summaryText(view);
    }
    table.replaceChildren(...view.folders.map(folderRow));
    heldNote.hidden = !view.folders.some((folder) => folder.held !== null);
  }

  async function refresh() {
    try {
      const response = await fetch('/api/snapshots');
      if (response.status === 401) return;
      if (!response.ok) throw new Error('bad status');
      render(await response.json());
    } catch {
      summary.textContent = 'Could not load the status of the earlier versions.';
    }
  }

  refresh();
  setInterval(refresh, REFRESH_MS);
})();
