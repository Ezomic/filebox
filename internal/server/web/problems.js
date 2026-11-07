(() => {
  const REFRESH_MS = 60000;
  const card = document.getElementById('problems');
  const summary = document.getElementById('problems-summary');
  const rows = document.getElementById('problem-rows');
  const mailNote = document.getElementById('problems-mail');
  const when = (value) => new Date(value).toLocaleString();

  function row(problem) {
    const node = document.createElement('div');
    node.className = 'problem';
    const subject = document.createElement('div');
    subject.className = 'name';
    subject.textContent = problem.subject;
    const detail = document.createElement('p');
    detail.className = 'detail';
    detail.textContent = problem.detail;
    const since = document.createElement('div');
    since.className = 'detail';
    since.textContent = `Since ${when(problem.started)}`;
    node.append(subject, detail, since);
    return node;
  }

  function summaryText(view) {
    if (!view.checked) return 'Not checked yet. The first check runs right after the panel starts.';
    const count = view.problems.length;
    if (count === 0) return `No problems found. Last checked ${when(view.checked)}.`;
    return `${count === 1 ? '1 open problem' : `${count} open problems`}. Last checked ${when(view.checked)}.`;
  }

  function mailText(view) {
    if (!view.mail) return 'Alert emails are off: email is not set up on this box.';
    if (!view.address) return 'Alert emails are off until you set an address under Settings.';
    return `Alert emails go to ${view.address}.`;
  }

  function render(view) {
    rows.replaceChildren(...view.problems.map(row));
    card.classList.toggle('open', view.problems.length > 0);
    summary.textContent = summaryText(view);
    mailNote.textContent = mailText(view);
  }

  async function refresh() {
    try {
      const response = await fetch('/api/alerts');
      if (!response.ok) throw new Error('bad status');
      render(await response.json());
    } catch {
      summary.textContent = 'Could not load the open problems.';
    }
  }

  refresh();
  setInterval(refresh, REFRESH_MS);
})();
