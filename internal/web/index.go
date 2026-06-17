package web

// indexHTML is the single-page web chat client (S1 shell). It opens an SSE
// stream (/events), renders an append-only message timeline + a channel/topic
// switcher + a user list, and posts to /send and /topic. Kept dependency-free
// (vanilla JS, inline CSS) so the tile needs no build step or CDN.
//
// Manifest mode: slot. When pdn mounts the tile in its content slot it appends
// ?pdn_embed=1; the SPA detects that and renders chrome-less (its own outer
// header is hidden) so there is no double chrome around the pdn shell. Opened
// directly (no pdn_embed) it shows a full standalone page and so degrades
// gracefully.
//
// SSE discipline: the hub drops a subscriber that falls more than subBuffer=256
// events behind (chat.Hub.Subscribe), so the live event handler does the
// minimum possible work — append one DOM node, no blocking fetch — to drain the
// stream quickly. History backfill happens on (re)connect via /history, not in
// the live path. EventSource auto-reconnects; on each reconnect we reload
// /history for the current topic so nothing is lost across a drop.
var indexHTML = []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>BPQ Chat</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  :root {
    color-scheme: light dark;
    --bg: #ffffff; --panel: #f5f6f8; --line: #d7dae0; --ink: #1b1d22;
    --muted: #6b7280; --accent: #2563eb; --sys: #8a8f99; --priv: #c0392b;
    --me: #0e7c5a; --node: #7c3aed;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #14161a; --panel: #1c1f25; --line: #2b2f37; --ink: #e6e8ec;
      --muted: #9aa0aa; --accent: #4f8cff; --sys: #6c7280; --priv: #ef6f63;
      --me: #34d399; --node: #b388ff;
    }
  }
  * { box-sizing: border-box; }
  html, body { height: 100%; }
  body {
    font: 15px/1.5 system-ui, -apple-system, Segoe UI, Roboto, sans-serif;
    margin: 0; color: var(--ink); background: var(--bg);
    display: flex; flex-direction: column; height: 100vh;
  }

  /* Outer app header — hidden when embedded in the pdn shell slot. */
  header.appbar {
    display: flex; align-items: center; gap: .6rem;
    padding: .55rem .9rem; border-bottom: 1px solid var(--line); background: var(--panel);
  }
  body.embed header.appbar { display: none; }
  header.appbar .brand { font-weight: 700; font-size: 1rem; }
  header.appbar .me { margin-left: auto; color: var(--muted); }
  header.appbar .me b { color: var(--ink); }

  main { flex: 1; display: flex; min-height: 0; }

  /* Channels rail */
  nav.channels {
    width: 12rem; flex: none; border-right: 1px solid var(--line);
    background: var(--panel); display: flex; flex-direction: column; min-height: 0;
  }
  nav.channels h2 {
    font-size: .7rem; text-transform: uppercase; letter-spacing: .04em;
    color: var(--muted); margin: 0; padding: .7rem .8rem .35rem;
  }
  nav.channels .list { overflow-y: auto; flex: 1; }
  nav.channels button.chan {
    display: block; width: 100%; text-align: left; border: 0; background: none;
    color: var(--ink); font: inherit; padding: .4rem .8rem; cursor: pointer;
    border-left: 3px solid transparent;
  }
  nav.channels button.chan:hover { background: var(--line); }
  nav.channels button.chan.active {
    border-left-color: var(--accent); background: var(--bg); font-weight: 600;
  }
  nav.channels button.chan .hash { color: var(--muted); margin-right: .15rem; }
  nav.channels form.newchan { display: flex; gap: .3rem; padding: .5rem .6rem; border-top: 1px solid var(--line); }
  nav.channels form.newchan input { flex: 1; min-width: 0; padding: .35rem .45rem; font: inherit;
    border: 1px solid var(--line); border-radius: 6px; background: var(--bg); color: var(--ink); }
  nav.channels form.newchan button { padding: .35rem .55rem; }

  /* Conversation column */
  section.convo { flex: 1; display: flex; flex-direction: column; min-width: 0; min-height: 0; }
  .convohead {
    display: flex; align-items: baseline; gap: .5rem;
    padding: .55rem .9rem; border-bottom: 1px solid var(--line);
  }
  .convohead .topic { font-weight: 700; }
  .convohead .topic .hash { color: var(--muted); }
  .convohead .count { color: var(--muted); font-size: .85rem; }
  .convohead .status { margin-left: auto; font-size: .8rem; color: var(--muted); display: flex; align-items: center; gap: .35rem; }
  .convohead .status .dot { width: .55rem; height: .55rem; border-radius: 50%; background: var(--sys); }
  .convohead .status.live .dot { background: var(--me); }

  #log { flex: 1; overflow-y: auto; padding: .6rem .9rem; }
  #log .line { margin: .18rem 0; word-wrap: break-word; overflow-wrap: anywhere; }
  #log .line .ts { color: var(--muted); font-size: .78rem; margin-right: .45rem; font-variant-numeric: tabular-nums; }
  #log .line .from { font-weight: 600; }
  #log .line .node { color: var(--node); font-size: .82em; }
  #log .sys { color: var(--sys); font-style: italic; }
  #log .priv .from { color: var(--priv); }
  #log .me .from { color: var(--me); }

  /* User list */
  aside.users {
    width: 12rem; flex: none; border-left: 1px solid var(--line);
    background: var(--panel); overflow-y: auto; padding: .2rem 0;
  }
  aside.users h2 {
    font-size: .7rem; text-transform: uppercase; letter-spacing: .04em;
    color: var(--muted); margin: 0; padding: .7rem .8rem .35rem;
  }
  aside.users .u { padding: .22rem .8rem; }
  aside.users .u .call { font-weight: 500; }
  aside.users .u .node { color: var(--node); font-size: .82em; }

  /* Composer */
  form.composer { display: flex; gap: .5rem; padding: .55rem .9rem; border-top: 1px solid var(--line); background: var(--panel); }
  form.composer input {
    flex: 1; padding: .55rem .7rem; font: inherit;
    border: 1px solid var(--line); border-radius: 8px; background: var(--bg); color: var(--ink);
  }
  form.composer input:focus { outline: 2px solid var(--accent); outline-offset: -1px; }
  form.composer button {
    padding: .55rem 1rem; font: inherit; cursor: pointer; border: 0; border-radius: 8px;
    background: var(--accent); color: #fff; font-weight: 600;
  }

  @media (max-width: 640px) {
    nav.channels, aside.users { width: 9rem; }
  }
</style>
</head>
<body>
<header class="appbar">
  <span class="brand">BPQ Chat</span>
  <span class="me">you: <b id="me">…</b></span>
</header>
<main>
  <nav class="channels">
    <h2>Channels</h2>
    <div class="list" id="chanlist"></div>
    <form class="newchan" id="newchan">
      <input id="newchaninput" autocomplete="off" placeholder="new channel">
      <button title="Join channel">+</button>
    </form>
  </nav>
  <section class="convo">
    <div class="convohead">
      <span class="topic"><span class="hash">#</span><span id="topic">General</span></span>
      <span class="count" id="usercount"></span>
      <span class="status" id="status"><span class="dot"></span><span id="statustext">connecting…</span></span>
    </div>
    <div id="log"></div>
    <form class="composer" id="f">
      <input id="msg" autocomplete="off" autofocus
        placeholder="Message #General  ·  /T topic to switch  ·  /S CALL text for private">
      <button>Send</button>
    </form>
  </section>
  <aside class="users">
    <h2>Users</h2>
    <div id="users"></div>
  </aside>
</main>
<script>
// Chrome-less when the pdn shell mounts us in its content slot (?pdn_embed=1).
if (new URLSearchParams(location.search).get('pdn_embed') === '1') {
  document.body.classList.add('embed');
}

const log = document.getElementById('log');
const usersEl = document.getElementById('users');
const chanlistEl = document.getElementById('chanlist');
const topicEl = document.getElementById('topic');
const usercountEl = document.getElementById('usercount');
const meEl = document.getElementById('me');
const statusEl = document.getElementById('status');
const statustextEl = document.getElementById('statustext');
const msgEl = document.getElementById('msg');

let me = '';
let topic = 'General';
const channels = new Set(['General']);

function esc(s){ const d = document.createElement('div'); d.textContent = s; return d.innerHTML; }
function pad(n){ return n < 10 ? '0'+n : ''+n; }
function hhmm(t){ if(!t) return ''; const d = new Date(t); return pad(d.getHours())+':'+pad(d.getMinutes()); }

// add appends one line. Append-only + minimal work so the live SSE path drains
// fast (the hub drops subscribers >256 events behind).
function add(html, cls){
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 60;
  const el = document.createElement('div');
  el.className = 'line ' + (cls||'');
  el.innerHTML = html;
  log.appendChild(el);
  if (atBottom) log.scrollTop = log.scrollHeight;
}

function ts(t){ const s = hhmm(t); return s ? '<span class="ts">'+s+'</span>' : ''; }

function renderEvent(e){
  const mine = e.from && me && e.from.toUpperCase() === me.toUpperCase();
  if (e.type === 'msg')
    add(ts(e.time)+'<span class="from">'+esc(e.from)+(e.node?'<span class="node"> @'+esc(e.node)+'</span>':'')+':</span> '+esc(e.text), mine?'me':'');
  else if (e.type === 'private')
    add(ts(e.time)+'<span class="from">*'+esc(e.from||e.to)+'*:</span> '+esc(e.text), 'priv');
  else if (e.type === 'join')
    add('*** '+esc(e.from)+(e.node?' @'+esc(e.node):'')+' joined '+esc(e.topic||''), 'sys');
  else if (e.type === 'leave')
    add('*** '+esc(e.from)+' left', 'sys');
  else if (e.type === 'node')
    add('*** '+esc(e.text), 'sys');
}

function renderUsers(list){
  usersEl.innerHTML = '';
  list.forEach(u => {
    const d = document.createElement('div'); d.className = 'u';
    d.innerHTML = '<span class="call">'+esc(u.call)+'</span>' + (u.node?' <span class="node">@'+esc(u.node)+'</span>':'');
    usersEl.appendChild(d);
  });
  usercountEl.textContent = list.length ? list.length + (list.length===1?' user':' users') : '';
}

function renderChannels(){
  chanlistEl.innerHTML = '';
  Array.from(channels).sort((a,b)=>a.localeCompare(b)).forEach(name => {
    const b = document.createElement('button');
    b.className = 'chan' + (name.toLowerCase()===topic.toLowerCase()?' active':'');
    b.innerHTML = '<span class="hash">#</span>'+esc(name);
    b.onclick = () => switchTopic(name);
    chanlistEl.appendChild(b);
  });
}

function setTopic(name){
  topic = name;
  channels.add(name);
  topicEl.textContent = name;
  msgEl.placeholder = 'Message #'+name+'  ·  /T topic to switch  ·  /S CALL text for private';
  renderChannels();
}

function setStatus(live, text){
  statusEl.classList.toggle('live', !!live);
  statustextEl.textContent = text;
}

// switchTopic moves us server-side, clears the log, and backfills history. We do
// NOT touch the SSE stream — the server re-snapshots our topic for new events.
async function switchTopic(name){
  if (name.toLowerCase() === topic.toLowerCase()) return;
  await post('topic', {topic: name});
  setTopic(name);
  log.innerHTML = '';
  await loadHistory();
  await refreshUsers();
}

let es;
function connect(){
  es = new EventSource('events');
  es.addEventListener('you', ev => {
    const d = JSON.parse(ev.data);
    me = d.call; meEl.textContent = d.call;
    setTopic(d.topic || topic);
    setStatus(true, 'live');
    // Fresh snapshot replaces the timeline so a reconnect backfills cleanly.
    log.innerHTML = '';
  });
  es.addEventListener('users', ev => renderUsers(JSON.parse(ev.data)));
  // Live path: render immediately, nothing blocking, to drain the stream fast.
  es.addEventListener('event', ev => {
    const e = JSON.parse(ev.data);
    if (e.topic) channels.add(e.topic);
    renderEvent(e);
  });
  es.onopen = () => setStatus(true, 'live');
  es.onerror = () => setStatus(false, 'reconnecting…'); // EventSource auto-reconnects
}
connect();

async function post(url, body){
  await fetch(url, {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body)});
}
async function refreshUsers(){
  const r = await fetch('users?topic='+encodeURIComponent(topic));
  if (r.ok) renderUsers(await r.json());
}
async function loadHistory(){
  const r = await fetch('history?topic='+encodeURIComponent(topic));
  if (r.ok) (await r.json()).forEach(renderEvent);
}

document.getElementById('f').addEventListener('submit', async ev => {
  ev.preventDefault();
  const text = msgEl.value.trim(); if (!text) return; msgEl.value = '';
  const m = text.match(/^\/[tT]\s+(.+)$/);
  if (m) { await switchTopic(m[1].trim()); return; }
  await post('send', {text});
});

document.getElementById('newchan').addEventListener('submit', async ev => {
  ev.preventDefault();
  const inp = document.getElementById('newchaninput');
  const name = inp.value.trim(); if (!name) return; inp.value = '';
  await switchTopic(name);
  msgEl.focus();
});
</script>
</body>
</html>
`)
