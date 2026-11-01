const CHUNK_SIZE = 8 * 1024 * 1024;
const SINGLE_REQUEST_LIMIT = 8 * 1024 * 1024;
const EXISTS_BATCH = 500;
const MAX_RETRIES = 10;
const RESUME_PREFIX = 'filebox-upload:';

class NetworkError extends Error {
  constructor() {
    super(t('upload.connectionLost'));
    this.i18n = { key: 'upload.connectionLost' };
  }
}

class DiskFullError extends Error {}

function uploadFailure(body, fallbackKey) {
  const failure = i18n.failure(body, fallbackKey);
  if (!body.diskFull) return failure;
  return Object.assign(new DiskFullError(failure.message), { i18n: failure.i18n, code: failure.code });
}

const uploadHooks = { note: () => {} };

// A retry wait ends early when the phone comes back to the page or the network returns, so a resume does not idle out its backoff.
function pause(ms) {
  return new Promise((resolve) => {
    const finish = () => {
      clearTimeout(timer);
      window.removeEventListener('online', finish);
      document.removeEventListener('visibilitychange', onVisible);
      resolve();
    };
    const onVisible = () => {
      if (document.visibilityState === 'visible') finish();
    };
    const timer = setTimeout(finish, ms);
    window.addEventListener('online', finish);
    document.addEventListener('visibilitychange', onVisible);
  });
}

async function call(method, path, payload) {
  const options = { method };
  if (payload !== undefined) {
    options.headers = { 'Content-Type': 'application/json' };
    options.body = JSON.stringify(payload);
  }
  let response;
  try {
    response = await fetch(path, options);
  } catch {
    throw new NetworkError();
  }
  if (response.status === 401) {
    window.location.href = '/login';
    throw new Error('signed out');
  }
  const body = response.status === 204 ? {} : await response.json().catch(() => ({}));
  return { status: response.status, body };
}

function sendBlob(url, blob, headers, onProgress) {
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    request.open('PUT', url);
    request.setRequestHeader('Content-Type', 'application/octet-stream');
    Object.entries(headers).forEach(([name, value]) => request.setRequestHeader(name, value));
    request.upload.addEventListener('progress', (event) => {
      if (event.lengthComputable) onProgress(event.loaded);
    });
    request.addEventListener('load', () => {
      if (request.status === 401) {
        window.location.href = '/login';
        reject(new Error('signed out'));
        return;
      }
      let body = {};
      try {
        body = JSON.parse(request.responseText);
      } catch {
        body = {};
      }
      resolve({ status: request.status, body });
    });
    request.addEventListener('error', () => reject(new NetworkError()));
    request.addEventListener('abort', () => reject(new NetworkError()));
    request.send(blob);
  });
}

async function withRetry(task) {
  for (let attempt = 0; ; attempt += 1) {
    try {
      return await task();
    } catch (error) {
      if (!(error instanceof NetworkError) || attempt >= MAX_RETRIES) throw error;
      uploadHooks.note(() => t('upload.retrying', { attempt: attempt + 1, max: MAX_RETRIES }));
      await pause(Math.min(1000 * 2 ** attempt, 15000));
    }
  }
}

function resumeKey(item) {
  return `${RESUME_PREFIX}${item.target}|${item.file.size}|${item.file.lastModified}`;
}

function readSaved(key) {
  try {
    return JSON.parse(localStorage.getItem(key));
  } catch {
    return null;
  }
}

function saveSession(key, value) {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch {
    return;
  }
}

function forgetSession(key) {
  try {
    localStorage.removeItem(key);
  } catch {
    return;
  }
}

async function makeFolders(paths) {
  for (const path of paths) {
    const { status, body } = await withRetry(() => call('POST', '/api/files/folder', { path }));
    if (status !== 201 && status !== 409) throw i18n.failure(body, 'upload.folderFailed', { path });
  }
}

async function sendSmallFile(item, report) {
  const overwrite = item.overwrite ? '&overwrite=1' : '';
  const url = `/api/files/content?path=${encodeURIComponent(item.target)}${overwrite}`;
  const { status, body } = await withRetry(() => sendBlob(url, item.file, {}, report));
  if (status !== 204) throw uploadFailure(body, 'upload.failed');
}

async function resumeSession(item, key) {
  const saved = readSaved(key);
  if (!saved || typeof saved.id !== 'string' || saved.overwrite !== item.overwrite) return null;
  const { status, body } = await withRetry(() => call('GET', `/api/uploads/${encodeURIComponent(saved.id)}`));
  if (status === 200 && body.path === item.target && body.size === item.file.size) {
    return { id: saved.id, offset: body.offset, resumed: body.offset > 0 };
  }
  forgetSession(key);
  return null;
}

async function createSession(item, key) {
  const payload = { path: item.target, size: item.file.size, overwrite: item.overwrite };
  const { status, body } = await withRetry(() => call('POST', '/api/uploads', payload));
  if (status !== 201) throw uploadFailure(body, 'upload.startFailed');
  saveSession(key, { id: body.id, overwrite: item.overwrite });
  return { id: body.id, offset: 0, resumed: false };
}

function sendChunk(item, session, from, report) {
  const size = item.file.size;
  const end = Math.min(from + CHUNK_SIZE, size);
  const headers = { 'Content-Range': `bytes ${from}-${end - 1}/${size}` };
  return withRetry(() => sendBlob(`/api/uploads/${session.id}`, item.file.slice(from, end), headers, (loaded) => report(from + loaded)));
}

async function sendLargeFile(item, report) {
  const key = resumeKey(item);
  const size = item.file.size;
  let session = (await resumeSession(item, key)) || (await createSession(item, key));
  let restarted = false;
  let stalls = 0;
  let offset = session.offset;
  if (session.resumed) uploadHooks.note(() => t('upload.resuming', { size: i18n.size(offset) }));

  for (let finishTries = 0; finishTries < 3; finishTries += 1) {
    while (offset < size) {
      const from = offset;
      const { status, body } = await sendChunk(item, session, from, report);
      if (status === 200) {
        offset = body.offset;
        stalls = 0;
      } else if ((status === 409 || status === 400) && typeof body.offset === 'number') {
        stalls = body.offset === from ? stalls + 1 : 0;
        if (stalls >= 3) throw i18n.failure(body, 'upload.failed');
        offset = body.offset;
      } else if (status === 404 && !restarted) {
        restarted = true;
        forgetSession(key);
        session = await createSession(item, key);
        offset = 0;
      } else {
        throw i18n.failure(body, 'upload.failed');
      }
      report(offset);
    }

    const { status, body } = await withRetry(() => call('POST', `/api/uploads/${session.id}/finish`));
    if (status === 204 || (status === 404 && await alreadyThere(item))) {
      forgetSession(key);
      return;
    }
    if (status !== 409 || typeof body.offset !== 'number') throw i18n.failure(body, 'upload.incomplete');
    offset = body.offset;
  }
  throw i18n.problem('upload.incomplete');
}

async function alreadyThere(item) {
  const { status, body } = await withRetry(() => call('POST', '/api/files/exists', { paths: [item.target] }));
  return status === 200 && body.existing.some((entry) => entry.path === item.target && !entry.isDir);
}

async function lookupExisting(paths) {
  const existing = new Map();
  for (let start = 0; start < paths.length; start += EXISTS_BATCH) {
    const batch = paths.slice(start, start + EXISTS_BATCH);
    const { status, body } = await withRetry(() => call('POST', '/api/files/exists', { paths: batch }));
    if (status !== 200) throw i18n.failure(body, 'upload.checkFailed');
    body.existing.forEach((entry) => existing.set(entry.path, entry));
  }
  return existing;
}

function sendItem(item, report) {
  return item.file.size > SINGLE_REQUEST_LIMIT ? sendLargeFile(item, report) : sendSmallFile(item, report);
}
