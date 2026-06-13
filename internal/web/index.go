package web

// indexHTML is the single-page web chat client. It opens an SSE stream
// (/events), renders messages/presence, and posts to /send and /topic. Kept
// dependency-free (vanilla JS) so the tile needs no build step or CDN.
var indexHTML = []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>pdn-bpqchat</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body { font: 15px/1.45 system-ui, sans-serif; margin: 0; height: 100vh; display: flex; flex-direction: column; }
  header { padding: .5rem .8rem; border-bottom: 1px solid #8884; display: flex; gap: .6rem; align-items: baseline; }
  header h1 { font-size: 1rem; margin: 0; }
  header .topic { font-weight: bold; }
  header .me { margin-left: auto; opacity: .7; }
  main { flex: 1; display: flex; min-height: 0; }
  #log { flex: 1; overflow-y: auto; padding: .6rem .8rem; }
  #log .line { margin: .15rem 0; word-wrap: break-word; }
  #log .sys { opacity: .6; font-style: italic; }
  #log .priv { color: #c0392b; }
  #log .from { font-weight: bold; }
  aside { width: 12rem; border-left: 1px solid #8884; overflow-y: auto; padding: .6rem; }
  aside h2 { font-size: .8rem; text-transform: uppercase; opacity: .6; margin: .2rem 0 .4rem; }
  aside .u { padding: .1rem 0; }
  aside .u .node { opacity: .6; font-size: .85em; }
  form { display: flex; gap: .4rem; padding: .5rem .8rem; border-top: 1px solid #8884; }
  input { flex: 1; padding: .45rem .6rem; font: inherit; }
  button { padding: .45rem .8rem; font: inherit; cursor: pointer; }
</style>
</head>
<body>
<header>
  <h1>pdn-bpqchat</h1>
  <span>topic <span class="topic" id="topic">General</span></span>
  <span class="me">you: <span id="me">…</span></span>
</header>
<main>
  <div id="log"></div>
  <aside><h2>Users</h2><div id="users"></div></aside>
</main>
<form id="f">
  <input id="msg" autocomplete="off" placeholder="Message, or /T topic to switch, /S CALL text for private" autofocus>
  <button>Send</button>
</form>
<script>
const log = document.getElementById('log');
const usersEl = document.getElementById('users');
const topicEl = document.getElementById('topic');
const meEl = document.getElementById('me');

function esc(s){ const d = document.createElement('div'); d.textContent = s; return d.innerHTML; }
function add(html, cls){
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
  const el = document.createElement('div'); el.className = 'line ' + (cls||''); el.innerHTML = html; log.appendChild(el);
  if (atBottom) log.scrollTop = log.scrollHeight;
}
function renderEvent(e){
  if (e.type === 'msg')      add('<span class="from">'+esc(e.from)+(e.node?'@'+esc(e.node):'')+':</span> '+esc(e.text));
  else if (e.type === 'private') add('<span class="from">*'+esc(e.from)+'*:</span> '+esc(e.text), 'priv');
  else if (e.type === 'join') add('*** '+esc(e.from)+' joined '+esc(e.topic||''), 'sys');
  else if (e.type === 'leave') add('*** '+esc(e.from)+' left', 'sys');
  else if (e.type === 'node') add('*** '+esc(e.text), 'sys');
}
function renderUsers(list){
  usersEl.innerHTML = '';
  list.forEach(u => { const d = document.createElement('div'); d.className='u';
    d.innerHTML = esc(u.call) + (u.node?' <span class="node">@'+esc(u.node)+'</span>':''); usersEl.appendChild(d); });
}

let es;
function connect(){
  es = new EventSource('events');
  es.addEventListener('you', ev => { const d = JSON.parse(ev.data); meEl.textContent = d.call; topicEl.textContent = d.topic; });
  es.addEventListener('users', ev => renderUsers(JSON.parse(ev.data)));
  es.addEventListener('event', ev => renderEvent(JSON.parse(ev.data)));
  es.onerror = () => { /* EventSource auto-reconnects */ };
}
connect();

async function post(url, body){ await fetch(url, {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body)}); }
async function refreshUsers(){ const r = await fetch('users?topic='+encodeURIComponent(topicEl.textContent)); if (r.ok) renderUsers(await r.json()); }

document.getElementById('f').addEventListener('submit', async ev => {
  ev.preventDefault();
  const inp = document.getElementById('msg'); const text = inp.value.trim(); if (!text) return; inp.value='';
  const m = text.match(/^\/[tT]\s+(.+)$/);
  if (m) { await post('topic', {topic: m[1]}); topicEl.textContent = m[1]; log.innerHTML=''; await loadHistory(); await refreshUsers(); return; }
  await post('send', {text});
});

async function loadHistory(){
  const r = await fetch('history?topic='+encodeURIComponent(topicEl.textContent));
  if (r.ok) (await r.json()).forEach(renderEvent);
}
</script>
</body>
</html>
`)
