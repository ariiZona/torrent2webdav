package main

// statusPage is self-contained: it polls /_status.json once a second and
// renders the snapshot. All untrusted text (torrent name, peer addresses) goes
// through textContent, never innerHTML.
const statusPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>torrent2webdav</title>
<style>
:root { color-scheme: dark }
body { margin: 0; padding: 1.5rem; font: 14px/1.45 system-ui, sans-serif; background: #101010; color: #ddd }
main { max-width: 62rem; margin: 0 auto }
h1 { font-size: 1.3rem; margin: 0 0 .2rem; word-break: break-all }
h2 { font-size: .75rem; text-transform: uppercase; letter-spacing: .07em; color: #888; margin: 1.6rem 0 .5rem }
.muted { color: #888 }
.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(10rem, 1fr)); gap: .7rem; margin: 1.2rem 0 0 }
.card { background: #1a1a1a; border: 1px solid #2b2b2b; border-radius: 6px; padding: .6rem .75rem; min-width: 0 }
.card span { font-size: .75rem; color: #888; text-transform: uppercase; letter-spacing: .05em }
.card b { display: block; font-size: 1.1rem; font-weight: 600; font-variant-numeric: tabular-nums; overflow: hidden; text-overflow: ellipsis }
.bar { height: 6px; background: #2b2b2b; border-radius: 3px; overflow: hidden; margin: .8rem 0 0 }
.bar > i { display: block; height: 100%; width: 0; background: #4c9 }
table { width: 100%; border-collapse: collapse; font-variant-numeric: tabular-nums }
th, td { text-align: left; padding: .32rem .5rem; border-bottom: 1px solid #242424; white-space: nowrap }
td:first-child, th:first-child { white-space: normal; word-break: break-all }
th { color: #888; font-weight: 500; font-size: .75rem; text-transform: uppercase; letter-spacing: .05em }
th[data-key] { cursor: pointer; user-select: none }
th[data-key]:focus-visible { outline: 1px solid #4c9; outline-offset: 2px }
.pager { display: flex; gap: .6rem; align-items: center; margin-top: .5rem; color: #888 }
.pager button { background: #1a1a1a; color: #ddd; border: 1px solid #2b2b2b; border-radius: 4px; padding: .15rem .55rem; font: inherit; cursor: pointer }
.pager button:disabled { color: #555; cursor: default }
ul { margin: 0; padding-left: 1.1rem }
li { word-break: break-all }
</style>
</head>
<body><main>
<h1 id="name">torrent2webdav</h1>
<div class="muted" id="hash"></div>
<div class="bar"><i id="bar"></i></div>
<div class="grid">
  <div class="card"><span>Progress</span><b id="progress">-</b></div>
  <div class="card"><span>Download</span><b id="down">-</b></div>
  <div class="card"><span>Upload</span><b id="up">-</b></div>
  <div class="card"><span>Swarm</span><b id="swarm">-</b></div>
  <div class="card"><span>Public IP</span><b id="ip">-</b></div>
  <div class="card"><span>BitTorrent port</span><b id="port">-</b></div>
</div>
<div class="muted" id="listen" style="margin-top:.5rem"></div>
<div class="muted" id="totals" style="margin-top:.35rem"></div>
<h2>Open files</h2><ul id="files"><li class="muted">none</li></ul>
<h2>Cached files</h2>
<table id="cachedTable"><thead><tr>
<th data-key="name" tabindex="0">File</th>
<th data-key="cached" tabindex="0">Cached</th>
<th data-key="size" tabindex="0">Size</th>
<th data-key="lastOpened" tabindex="0">Last opened</th>
<th data-key="uploaded" tabindex="0">Uploaded</th>
</tr></thead>
<tbody id="cached"><tr><td colspan="5" class="muted">none</td></tr></tbody></table>
<div class="pager" id="cachedPager"><button id="cachedPrev" type="button">Prev</button><span id="cachedPage"></span><button id="cachedNext" type="button">Next</button></div>
<h2>Peers</h2>
<table><thead><tr><th>Address</th><th>Source</th><th>Down</th><th>Up</th><th>Pieces</th></tr></thead>
<tbody id="peers"><tr><td colspan="5" class="muted">none</td></tr></tbody></table>
<h2>Trackers</h2><ul id="trackers"><li class="muted">none</li></ul>
</main>
<script>
const $ = id => document.getElementById(id);
const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
function bytes(n) {
  let i = 0;
  while (n >= 1024 && i < UNITS.length - 1) { n /= 1024; i++ }
  return (i ? n.toFixed(1) : String(n)) + ' ' + UNITS[i];
}
const rate = n => bytes(n) + '/s';
function item(text, cls) {
  const li = document.createElement('li');
  li.textContent = text;
  if (cls) li.className = cls;
  return li;
}
function row(cells) {
  const tr = document.createElement('tr');
  for (const c of cells) {
    const td = document.createElement('td');
    td.textContent = c;
    tr.append(td);
  }
  return tr;
}
function emptyRow(cols, text = 'none') {
  const tr = document.createElement('tr');
  const td = document.createElement('td');
  td.colSpan = cols;
  td.className = 'muted';
  td.textContent = text;
  tr.append(td);
  return tr;
}
function fileText(f) {
  const pct = f.size ? (100 * f.cached / f.size).toFixed(1) : '0.0';
  return f.name + ' - ' + pct + '% cached (' + bytes(f.cached) + ' / ' + bytes(f.size) + ')';
}
const cachedPageSize = 25;
const cachedHeads = [...$('cachedTable').tHead.rows[0].cells];
let cached = {list: [], total: 0};
let cachedPage = 0, cachedSort = 'name', cachedDir = 1;
function renderCached() {
  const list = cached.list.slice().sort((a, b) => {
    const x = a[cachedSort], y = b[cachedSort];
    return (x < y ? -1 : x > y ? 1 : 0) * cachedDir;
  });
  const pages = Math.max(1, Math.ceil(list.length / cachedPageSize));
  cachedPage = Math.min(Math.max(cachedPage, 0), pages - 1);
  const start = cachedPage * cachedPageSize;
  const rows = list.slice(start, start + cachedPageSize).map(f => row([
    f.name, bytes(f.cached), bytes(f.size),
    f.lastOpened ? new Date(f.lastOpened).toLocaleString() : '-',
    bytes(f.uploaded || 0),
  ]));
  $('cached').replaceChildren(...(rows.length ? rows : [emptyRow(5)]));
  for (const th of cachedHeads) {
    const on = th.dataset.key === cachedSort;
    th.setAttribute('aria-sort', on ? (cachedDir > 0 ? 'ascending' : 'descending') : 'none');
  }
  $('cachedPager').style.display = cached.total ? '' : 'none';
  $('cachedPrev').disabled = cachedPage === 0;
  $('cachedNext').disabled = cachedPage >= pages - 1;
  $('cachedPage').textContent = 'Page ' + (cachedPage + 1) + ' of ' + pages;
}
function sortCached(key) {
  if (key === cachedSort) cachedDir = -cachedDir;
  else { cachedSort = key; cachedDir = 1; }
  cachedPage = 0;
  renderCached();
}
for (const th of cachedHeads) {
  th.addEventListener('click', () => sortCached(th.dataset.key));
  th.addEventListener('keydown', e => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); sortCached(th.dataset.key); }
  });
}
$('cachedPrev').onclick = () => { cachedPage--; renderCached(); };
$('cachedNext').onclick = () => { cachedPage++; renderCached(); };
let busy = false;
async function tick() {
  if (busy) return;
  busy = true;
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 5000);
  let s;
  try {
    const r = await fetch('/_status.json', {cache: 'no-store', signal: ctrl.signal});
    if (!r.ok) throw new Error('HTTP ' + r.status);
    s = await r.json();
  } catch (e) {
    $('name').textContent = 'status unavailable: ' + e.message;
    return;
  } finally {
    clearTimeout(timer);
    busy = false;
  }
  const t = s.torrent, q = s.selfTest;
  document.title = t.name + ' - torrent2webdav';
  $('name').textContent = t.name;
  $('hash').textContent = t.infohash;
  $('bar').style.width = t.percent.toFixed(1) + '%';
  $('progress').textContent = t.percent.toFixed(1) + '% - ' + bytes(t.cached) + ' / ' + bytes(t.size);
  $('down').textContent = rate(s.transfer.down);
  $('up').textContent = rate(s.transfer.up);
  $('swarm').textContent = s.swarm.connected + ' / ' + s.swarm.total + ' peers';
  $('swarm').title = s.swarm.seeders + ' seeders, ' + s.swarm.pending + ' pending, ' + s.swarm.halfOpen + ' half-open';
  $('ip').textContent = q.publicIp || 'unknown';
  $('port').textContent = q.port + ' ' + q.portStatus;
  $('port').title = q.error || ('checked ' + (q.checked || 'not yet'));
  $('listen').textContent = s.listeners.length ? 'listening on ' + s.listeners.join(', ') : '';
  $('totals').textContent = 'Totals: ' + bytes(s.transfer.downloaded) + ' downloaded, ' +
    bytes(s.transfer.uploaded) + ' uploaded, ' + bytes(s.transfer.webdavSent) + ' sent over WebDAV';
  $('files').replaceChildren(...(s.openFiles.length
    ? s.openFiles.map(f => item(fileText(f) + (f.readers > 1 ? ' (' + f.readers + ' readers)' : '')))
    : [item('none', 'muted')]));
  cached = {list: s.cachedFiles, total: s.cachedFilesTotal};
  renderCached();
  $('peers').replaceChildren(...(s.peers.length
    ? s.peers.map(p => row([p.addr, p.source, rate(p.down), rate(p.up), String(p.pieces)]))
    : [emptyRow(5)]));
  $('trackers').replaceChildren(...(s.trackers.length
    ? s.trackers.map(u => item(u))
    : [item('none', 'muted')]));
}
tick();
setInterval(tick, 1000);
</script>
</body></html>
`
