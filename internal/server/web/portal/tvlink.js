// Shows the TV link in the navigation only when the server has the TV add-on.
(async () => {
  try {
    const response = await fetch('/api/tv');
    if (response.ok && (await response.json()).enabled) document.getElementById('tv-link').hidden = false;
  } catch {
    // the link simply stays hidden
  }
})();
