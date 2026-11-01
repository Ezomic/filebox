(() => {
  const LANGS = ['nl', 'en'];
  const FALLBACK = 'en';
  const STORAGE_KEY = 'filebox-lang';
  const LOCALES = { nl: 'nl-NL', en: 'en-GB' };
  const NAMES = { nl: 'Nederlands', en: 'English' };
  const UNITS = ['KB', 'MB', 'GB', 'TB', 'PB'];

  const tables = {};
  const listeners = [];
  const bound = new Map();

  const owns = (object, key) => Object.prototype.hasOwnProperty.call(object, key);

  function storedLang() {
    try {
      const value = localStorage.getItem(STORAGE_KEY);
      return LANGS.includes(value) ? value : null;
    } catch {
      return null;
    }
  }

  function browserLang() {
    return String(navigator.language || '').toLowerCase().startsWith('nl') ? 'nl' : 'en';
  }

  let lang = storedLang() || browserLang();
  let plurals = new Intl.PluralRules(LOCALES[lang]);
  document.documentElement.lang = lang;

  // Read before the page is parsed on purpose: every script after this one can call t() at once, and nothing paints untranslated.
  function table(code) {
    if (owns(tables, code)) return tables[code];
    let loaded = {};
    try {
      const request = new XMLHttpRequest();
      request.open('GET', `/static/${code}.json`, false);
      request.send();
      if (request.status === 200) loaded = JSON.parse(request.responseText);
    } catch {
      loaded = {};
    }
    tables[code] = loaded && typeof loaded === 'object' ? loaded : {};
    return tables[code];
  }

  function find(code, key, count) {
    const source = table(code);
    const candidates = typeof count === 'number' ? [`${key}.${plurals.select(count)}`, `${key}.other`, key] : [key];
    for (const candidate of candidates) {
      if (owns(source, candidate) && typeof source[candidate] === 'string') return source[candidate];
    }
    return null;
  }

  function has(key) {
    return find(FALLBACK, key) !== null || find(FALLBACK, key, 0) !== null;
  }

  // One pass over the template, so a placeholder inside a value (a file called "{count}") is never expanded again.
  function fill(template, params) {
    return template.replace(/\{(\w+)\}/g, (match, name) => (owns(params, name) ? String(params[name]) : match));
  }

  function t(key, params = {}) {
    const template = find(lang, key, params.count) ?? find(FALLBACK, key, params.count);
    return template === null ? key : fill(template, params);
  }

  const locale = () => LOCALES[lang];

  function number(value, digits) {
    return new Intl.NumberFormat(locale(), { minimumFractionDigits: digits, maximumFractionDigits: digits }).format(value);
  }

  function size(bytes, alwaysDecimal) {
    if (bytes < 1024) return `${number(bytes, 0)} B`;
    let value = bytes;
    let unit = -1;
    do {
      value /= 1024;
      unit += 1;
    } while (value >= 1024 && unit < UNITS.length - 1);
    return `${number(value, alwaysDecimal || value < 10 ? 1 : 0)} ${UNITS[unit]}`;
  }

  const dateTime = (value) => new Date(value).toLocaleString(locale());
  const date = (value) => new Date(value).toLocaleDateString(locale());
  const monthYear = (value) => new Date(value).toLocaleDateString(locale(), { month: 'long', year: 'numeric', timeZone: 'UTC' });
  const longDay = (value) => new Date(value).toLocaleDateString(locale(), { day: 'numeric', month: 'long', year: 'numeric', timeZone: 'UTC' });

  function monthName(index) {
    const name = new Date(Date.UTC(2000, index, 1)).toLocaleDateString(locale(), { month: 'long', timeZone: 'UTC' });
    return name.charAt(0).toLocaleUpperCase(locale()) + name.slice(1);
  }

  function failure(body, fallbackKey, params) {
    const message = body && typeof body.error === 'string' ? body.error : '';
    const code = body && typeof body.code === 'string' ? body.code : '';
    const error = new Error(message || t(fallbackKey, params));
    if (code) error.code = code;
    if (code && has(`error.${code}`)) error.i18n = { key: `error.${code}`, params };
    else if (message === '') error.i18n = { key: fallbackKey, params };
    return error;
  }

  function problem(key, params) {
    const error = new Error(t(key, params));
    error.i18n = { key, params };
    return error;
  }

  function describe(error) {
    if (error && error.i18n) return t(error.i18n.key, error.i18n.params);
    return error && error.message ? error.message : t('error.generic');
  }

  // Text that can change after the page loaded goes through here, so it follows a language switch.
  function msg(node, producer) {
    node.removeAttribute('data-i18n');
    if (!producer) {
      bound.delete(node);
      node.textContent = '';
      return;
    }
    bound.set(node, producer);
    node.textContent = producer();
  }

  function label(node, key) {
    node.setAttribute('data-i18n', key);
    node.textContent = t(key);
    return node;
  }

  function report(node, error) {
    msg(node, () => describe(error));
  }

  function answer(node, body, fallbackKey) {
    const code = body && typeof body.code === 'string' ? body.code : '';
    const key = code && has(`answer.${code}`) ? `answer.${code}` : null;
    const text = body && typeof body.message === 'string' && body.message !== '' ? body.message : null;
    msg(node, () => (key ? t(key) : text ?? t(fallbackKey)));
  }

  function apply(root = document) {
    root.querySelectorAll('[data-i18n]').forEach((node) => {
      node.textContent = t(node.getAttribute('data-i18n'));
    });
    root.querySelectorAll('[data-i18n-attr]').forEach((node) => {
      node.getAttribute('data-i18n-attr').split(',').forEach((pair) => {
        const [attribute, key] = pair.split(':');
        if (attribute && key) node.setAttribute(attribute.trim(), t(key.trim()));
      });
    });
    bound.forEach((producer, node) => {
      if (node.isConnected) node.textContent = producer();
      else bound.delete(node);
    });
  }

  const switches = [];

  function buildSwitch() {
    const group = document.createElement('div');
    group.className = 'lang-switch';
    group.setAttribute('role', 'group');
    LANGS.forEach((code) => {
      const choice = document.createElement('button');
      choice.type = 'button';
      choice.lang = code;
      choice.textContent = NAMES[code];
      choice.dataset.lang = code;
      choice.addEventListener('click', () => setLang(code));
      group.append(choice);
    });
    switches.push(group);
    return group;
  }

  function markSwitches() {
    switches.forEach((group) => {
      group.setAttribute('aria-label', t('lang.label'));
      group.querySelectorAll('button').forEach((choice) => {
        choice.setAttribute('aria-pressed', String(choice.dataset.lang === lang));
      });
    });
  }

  function setLang(code) {
    if (!LANGS.includes(code) || code === lang) return;
    lang = code;
    plurals = new Intl.PluralRules(locale());
    try {
      localStorage.setItem(STORAGE_KEY, code);
    } catch {
      // The choice then lasts until the page is closed.
    }
    document.documentElement.lang = code;
    apply();
    markSwitches();
    listeners.forEach((listener) => listener(code));
  }

  const hiding = document.createElement('style');
  hiding.textContent = 'body { visibility: hidden; }';
  document.head.append(hiding);

  document.addEventListener('DOMContentLoaded', () => {
    try {
      const main = document.querySelector('main');
      if (main) main.prepend(buildSwitch());
      apply();
      markSwitches();
    } finally {
      hiding.remove();
    }
  });

  window.i18n = {
    t,
    has,
    lang: () => lang,
    locale,
    setLang,
    onChange: (listener) => listeners.push(listener),
    apply,
    msg,
    label,
    report,
    answer,
    failure,
    problem,
    describe,
    size,
    dateTime,
    date,
    monthYear,
    longDay,
    monthName,
  };
  window.t = t;
})();
