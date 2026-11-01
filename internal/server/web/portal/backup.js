const byId = (id) => document.getElementById(id);

const BACKUP_ROOT = 'Photo backup';
const MAX_NAME_BYTES = 240;
const MAX_ATTEMPTS = 200;
const MAX_FAILURES_SHOWN = 100;
const FUTURE_SLACK_MS = 5 * 60 * 1000;
const RECHECK_MS = 30000;
const RESERVED_NAMES = new Set(['.trash', '.uploads', '.thumbs']);
const encoder = new TextEncoder();

const input = byId('backup-input');
const totalBar = byId('backup-total');
const fileBar = byId('backup-file');
const detail = byId('backup-detail');
const summary = byId('backup-summary');
const failureList = byId('backup-failures');
const retryButton = byId('backup-retry');
const waitingBox = byId('backup-waiting');
const awakeNote = byId('backup-awake');

let running = false;
let lastRetry = [];
let totals = { uploaded: 0, skipped: 0, renamed: 0 };

function join(parent, name) {
  return `${parent}/${name}`;
}

function backupFolder(file) {
  const modified = Number(file.lastModified);
  const usable = Number.isFinite(modified) && modified > 0 && modified <= Date.now() + FUTURE_SLACK_MS;
  const date = new Date(usable ? modified : Date.now());
  const year = String(date.getFullYear()).padStart(4, '0');
  const month = String(date.getMonth() + 1).padStart(2, '0');
  return `${BACKUP_ROOT}/${year}/${month}`;
}

function splitName(name) {
  const dot = name.lastIndexOf('.');
  if (dot <= 0 || !/^\.[A-Za-z0-9]{1,8}$/.test(name.slice(dot))) return { stem: name, ext: '' };
  return { stem: name.slice(0, dot), ext: name.slice(dot) };
}

function fitName(name) {
  if (encoder.encode(name).length <= MAX_NAME_BYTES) return name;
  const { stem, ext } = splitName(name);
  const kept = Array.from(stem).slice(0, MAX_NAME_BYTES);
  while (kept.length > 1 && encoder.encode(kept.join('') + ext).length > MAX_NAME_BYTES) kept.pop();
  return kept.join('') + ext;
}

function cleanName(raw) {
  const text = String(raw ?? '');
  const last = (text.toWellFormed ? text.toWellFormed() : text).split(/[\\/]/).pop();
  const name = fitName(last.replace(/[\u0000-\u001f\u007f]/g, '').trim()).replace(/[\s.]+$/, '');
  return name === '' || RESERVED_NAMES.has(name.toLowerCase()) ? null : name;
}

function candidateName(name, attempt) {
  if (attempt === 1) return name;
  const { stem, ext } = splitName(name);
  return `${stem} (${attempt})${ext}`;
}

function describeEntries(files) {
  const entries = [];
  const refused = [];
  files.forEach((file) => {
    const name = cleanName(file.name);
    if (name === null) refused.push({ file, error: i18n.problem('backup.badName'), retry: false });
    else entries.push({ file, name, folder: backupFolder(file), attempt: 1 });
  });
  return { entries, refused };
}

// Round by round, each file tries its own name and then "name (2)", "name (3)" and so on. The same name and size
// means the photo is already backed up. A different size means a different photo, which is never overwritten.
async function planTargets(entries) {
  const claimed = new Map();
  const ready = [];
  const skipped = [];
  const failed = [];
  let open = entries;
  while (open.length > 0) {
    const targets = open.map((entry) => join(entry.folder, candidateName(entry.name, entry.attempt)));
    const existing = await lookupExisting(Array.from(new Set(targets)));
    const next = [];
    open.forEach((entry, index) => {
      const target = targets[index];
      const taken = claimed.get(target);
      const there = existing.get(target);
      if (taken !== undefined && taken === entry.file.size) skipped.push(entry);
      else if (taken === undefined && !there) {
        claimed.set(target, entry.file.size);
        ready.push({ ...entry, target, overwrite: false });
      } else if (taken === undefined && !there.isDir && there.size === entry.file.size) skipped.push(entry);
      else if (entry.attempt >= MAX_ATTEMPTS) failed.push({ file: entry.file, error: i18n.problem('backup.tooManyNames') });
      else next.push({ ...entry, attempt: entry.attempt + 1 });
    });
    open = next;
  }
  return { ready, skipped, failed };
}

const wake = { sentinel: null, wanted: false, state: 'idle' };

function showAwake(state) {
  wake.state = state;
  const keys = { on: 'backup.awake', unsupported: 'backup.awakeUnsupported' };
  if (keys[state]) i18n.msg(awakeNote, () => t(keys[state]));
  else i18n.msg(awakeNote);
}

async function acquireWake() {
  if (!wake.wanted || wake.sentinel || document.visibilityState !== 'visible') return;
  if (typeof navigator.wakeLock?.request !== 'function') {
    showAwake('unsupported');
    return;
  }
  try {
    const sentinel = await navigator.wakeLock.request('screen');
    if (!wake.wanted) {
      sentinel.release().catch(() => {});
      return;
    }
    wake.sentinel = sentinel;
    sentinel.addEventListener('release', () => {
      if (wake.sentinel !== sentinel) return;
      wake.sentinel = null;
      showAwake('idle');
    });
    showAwake('on');
  } catch {
    showAwake('unsupported');
  }
}

function holdAwake() {
  wake.wanted = true;
  return acquireWake();
}

function releaseAwake() {
  wake.wanted = false;
  const sentinel = wake.sentinel;
  wake.sentinel = null;
  if (sentinel) sentinel.release().catch(() => {});
  showAwake('idle');
}

document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') acquireWake();
});

function waitForReturn() {
  i18n.msg(detail, () => t('backup.waiting'));
  waitingBox.hidden = false;
  return new Promise((resolve) => {
    const finish = () => {
      clearInterval(timer);
      window.removeEventListener('online', onOnline);
      document.removeEventListener('visibilitychange', onVisible);
      byId('backup-resume').removeEventListener('click', finish);
      waitingBox.hidden = true;
      resolve();
    };
    const onOnline = () => finish();
    const onVisible = () => {
      if (document.visibilityState === 'visible' && navigator.onLine !== false) finish();
    };
    const timer = setInterval(() => {
      if (document.visibilityState === 'visible' && navigator.onLine !== false) finish();
    }, RECHECK_MS);
    window.addEventListener('online', onOnline);
    document.addEventListener('visibilitychange', onVisible);
    byId('backup-resume').addEventListener('click', finish);
  });
}

function showProgress(visible) {
  fileBar.hidden = !visible;
  totalBar.hidden = !visible;
}

function reportProgress(run, item, position, fileBytes) {
  fileBar.max = Math.max(item.file.size, 1);
  fileBar.value = fileBytes;
  totalBar.max = Math.max(run.totalBytes, 1);
  totalBar.value = run.doneBytes + fileBytes;
  i18n.msg(detail, () => t('upload.progress', {
    name: item.target,
    position,
    total: run.count,
    sent: i18n.size(fileBytes),
    size: i18n.size(item.file.size),
    overallSent: i18n.size(run.doneBytes + fileBytes),
    overallSize: i18n.size(run.totalBytes),
  }));
}

async function transfer(queue, run, outcome) {
  for (const [index, item] of queue.entries()) {
    const position = run.count - queue.length + index + 1;
    const report = (bytes) => reportProgress(run, item, position, bytes);
    report(0);
    try {
      await sendItem(item, report);
      outcome.uploaded += 1;
      if (item.attempt > 1) outcome.renamed += 1;
      run.doneBytes += item.file.size;
    } catch (error) {
      if (error.message === 'signed out') throw error;
      if (error instanceof NetworkError) return { stop: 'network', remaining: queue.slice(index) };
      outcome.failed.push({ file: item.file, error });
      if (error instanceof DiskFullError) return { stop: 'disk', remaining: queue.slice(index + 1) };
    }
  }
  return { stop: null, remaining: [] };
}

async function uploadAll(ready, outcome) {
  const run = { count: ready.length, doneBytes: 0, totalBytes: ready.reduce((sum, item) => sum + item.file.size, 0) };
  let queue = ready;
  showProgress(true);
  i18n.msg(summary, () => t('upload.uploading'));
  for (;;) {
    const { stop, remaining } = await transfer(queue, run, outcome);
    if (stop === 'disk') outcome.untried.push(...remaining.map((item) => item.file));
    if (stop !== 'network') return stop;
    queue = remaining;
    await waitForReturn();
    i18n.msg(summary, () => t('upload.uploading'));
  }
}

function showFailures(failed) {
  const lines = failed.slice(0, MAX_FAILURES_SHOWN).map(({ file, error }) => {
    const line = document.createElement('li');
    line.className = 'error';
    i18n.msg(line, () => `${file.name}: ${i18n.describe(error)}`);
    return line;
  });
  if (failed.length > MAX_FAILURES_SHOWN) {
    const more = document.createElement('li');
    more.className = 'detail';
    i18n.msg(more, () => t('backup.moreFailed', { count: failed.length - MAX_FAILURES_SHOWN }));
    lines.push(more);
  }
  failureList.replaceChildren(...lines);
}

function showSummary(failed, stopped, retryFiles) {
  const { uploaded, skipped, renamed } = totals;
  i18n.msg(summary, () => {
    const parts = [t('upload.done', { count: uploaded })];
    if (skipped > 0) parts.push(t('backup.skipped', { count: skipped }));
    if (renamed > 0) parts.push(t('backup.renamed', { count: renamed }));
    if (failed.length > 0) parts.push(t('backup.failed', { count: failed.length }));
    if (stopped) parts.push(stopped());
    return parts.join(' ');
  });
  i18n.msg(detail);
  showFailures(failed);
  lastRetry = retryFiles;
  retryButton.hidden = retryFiles.length === 0;
}

function setBusy(busy) {
  running = busy;
  input.disabled = busy;
  retryButton.hidden = true;
  if (!busy) showProgress(false);
}

async function backUp(files) {
  if (running || files.length === 0) return;
  setBusy(true);
  failureList.replaceChildren();
  i18n.msg(summary, () => t('upload.checking'));
  const outcome = { uploaded: 0, renamed: 0, failed: [], untried: [] };
  let stopped = null;
  let retryFiles = null;
  try {
    const { entries, refused } = describeEntries(files);
    outcome.failed.push(...refused);
    const plan = await planTargets(entries);
    outcome.failed.push(...plan.failed);
    totals.skipped += plan.skipped.length;
    if (plan.ready.length > 0) {
      await makeFolders(folderChain(plan.ready));
      await holdAwake();
      if ((await uploadAll(plan.ready, outcome)) === 'disk') stopped = () => t('upload.stoppedDisk');
    }
  } catch (error) {
    if (error.message === 'signed out') return;
    stopped = () => i18n.describe(error);
    retryFiles = files;
  } finally {
    releaseAwake();
    setBusy(false);
  }
  totals.uploaded += outcome.uploaded;
  totals.renamed += outcome.renamed;
  showSummary(outcome.failed, stopped, retryFiles ?? [...outcome.failed.filter((entry) => entry.retry !== false).map((entry) => entry.file), ...outcome.untried]);
  window.refreshStorage?.();
}

function folderChain(items) {
  const dirs = new Set();
  items.forEach(({ folder }) => {
    const parts = folder.split('/');
    parts.forEach((_, index) => dirs.add(parts.slice(0, index + 1).join('/')));
  });
  return Array.from(dirs).sort((a, b) => a.split('/').length - b.split('/').length || a.localeCompare(b));
}

input.addEventListener('change', () => {
  const files = Array.from(input.files);
  input.value = '';
  totals = { uploaded: 0, skipped: 0, renamed: 0 };
  backUp(files);
});

retryButton.addEventListener('click', () => {
  backUp(lastRetry);
});

uploadHooks.note = (producer) => i18n.msg(detail, producer);

window.addEventListener('beforeunload', (event) => {
  if (running) event.preventDefault();
});

byId('logout').addEventListener('click', async () => {
  await fetch('/api/logout', { method: 'POST' });
  window.location.href = '/login';
});
