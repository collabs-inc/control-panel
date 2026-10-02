// Loaded before everything else: an uncaught error, even one that stops another script from loading, is
// sent to the server (client-errors.jsonl, with this browser's version) and shown at the bottom of the
// page, so a failure only one device has can be fixed from the VM.
(function () {
  var sent = 0;
  function report(kind, msg, stack) {
    if (++sent > 20) return;
    try {
      fetch('/api/client-error', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, keepalive: true,
        body: JSON.stringify({ kind: kind, msg: String(msg).slice(0, 500), stack: String(stack || '').slice(0, 3000), view: document.title }),
      }).catch(function () {});
    } catch (e) { /* nothing more to do */ }
    var el = document.getElementById('cerr');
    if (!el && document.body) {
      el = document.createElement('button');
      el.id = 'cerr';
      el.type = 'button';
      el.className = 'cerr';
      el.addEventListener('click', function () { el.remove(); });
      document.body.appendChild(el);
    }
    if (el) el.textContent = 'Something broke here, and it was reported: ' + String(msg).slice(0, 200) + ' (tap to hide)';
  }
  window.cpReport = report; // the app reports what fails without throwing: a stream that never delivers
  addEventListener('error', function (e) {
    if (e.target && e.target !== window) return; // a failed image or script fetch, not a script error
    report('error', (e.message || 'error') + ' @ ' + String(e.filename || '').replace(/^.*\//, '') + ':' + e.lineno + ':' + e.colno, e.error && e.error.stack);
  });
  addEventListener('unhandledrejection', function (e) {
    var r = e.reason || {};
    report('rejection', r.message || r, r.stack);
  });
})();
