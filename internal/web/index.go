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

  /* Direct-messages section in the rail (S6). A correspondent button mirrors a
     channel button; an unread badge marks threads with new DMs while you're
     looking elsewhere. The hash glyph is swapped for a person glyph. */
  nav.channels button.dm .glyph { color: var(--muted); margin-right: .25rem; }
  nav.channels button.dm.active { border-left-color: var(--priv); }
  nav.channels button.dm .badge {
    float: right; background: var(--priv); color: #fff; border-radius: 999px;
    font-size: .7rem; line-height: 1; padding: .15rem .4rem; margin-left: .3rem;
  }
  nav.channels form.newdm { display: flex; gap: .3rem; padding: .35rem .6rem .5rem; }
  nav.channels form.newdm input { flex: 1; min-width: 0; padding: .35rem .45rem; font: inherit;
    border: 1px solid var(--line); border-radius: 6px; background: var(--bg); color: var(--ink); }
  nav.channels form.newdm button { padding: .35rem .55rem; }
  #log .line.dm.mine .from { color: var(--me); }
  #log .line.dm .from { color: var(--priv); font-weight: 600; }

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

  /* Settings button + pane. The button is a floating control (not in the appbar)
     so it stays reachable when embedded in the pdn slot, where the SPA's own
     appbar is hidden. */
  button.settings {
    position: fixed; top: .5rem; right: .6rem; z-index: 40;
    padding: .3rem .6rem; font: inherit; cursor: pointer;
    border: 1px solid var(--line); border-radius: 8px; background: var(--panel); color: var(--ink);
  }
  body.embed button.settings { top: .35rem; }
  button.settings:hover { background: var(--line); }

  /* Federation (admin) button — sits left of Settings; hidden until we confirm
     the viewer holds admin scope (a successful GET /peers). */
  button.fed {
    position: fixed; top: .5rem; right: 6.2rem; z-index: 40;
    padding: .3rem .6rem; font: inherit; cursor: pointer;
    border: 1px solid var(--line); border-radius: 8px; background: var(--panel); color: var(--ink);
    display: none;
  }
  body.embed button.fed { top: .35rem; }
  button.fed.show { display: inline-block; }
  button.fed:hover { background: var(--line); }

  /* Federation panel internals */
  .fedsec { margin: 0 0 1rem; }
  .fedsec h3 { font-size: .8rem; text-transform: uppercase; letter-spacing: .04em;
    color: var(--muted); margin: 0 0 .35rem; }
  .fedsec table { width: 100%; border-collapse: collapse; font-size: .85rem; }
  .fedsec th, .fedsec td { text-align: left; padding: .25rem .4rem; border-bottom: 1px solid var(--line); }
  .fedsec th { color: var(--muted); font-weight: 600; }
  .fedsec .empty { color: var(--muted); font-style: italic; font-size: .85rem; }
  .fedsec .badge { color: var(--node); font-size: .82em; }
  .allowrow { display: flex; align-items: center; gap: .4rem; padding: .2rem 0; }
  .allowrow .call { font-weight: 600; flex: 1; }
  .allowrow .pin { color: var(--muted); font-size: .8rem; font-style: italic; }
  .allowrow button.rm { padding: .15rem .5rem; font: inherit; cursor: pointer; border: 1px solid var(--line);
    border-radius: 6px; background: var(--panel); color: var(--priv); }
  form.allowadd { display: flex; gap: .4rem; margin-top: .5rem; }
  form.allowadd input { flex: 1; padding: .4rem .55rem; font: inherit;
    border: 1px solid var(--line); border-radius: 8px; background: var(--panel); color: var(--ink); }
  form.allowadd button { padding: .4rem .8rem; }
  .modal { position: fixed; inset: 0; background: rgba(0,0,0,.45);
    display: none; align-items: center; justify-content: center; z-index: 50; }
  .modal.open { display: flex; }
  .modal .sheet { background: var(--bg); color: var(--ink); border: 1px solid var(--line);
    border-radius: 12px; padding: 1.3rem 1.4rem; width: min(28rem, 92vw); max-height: 90vh; overflow-y: auto; }
  .modal .sheet h2 { margin: 0 0 .2rem; font-size: 1.1rem; }
  .modal .sheet p.sub { margin: 0 0 1rem; color: var(--muted); font-size: .85rem; }
  .modal .field { display: flex; flex-direction: column; gap: .25rem; margin-bottom: .8rem; }
  .modal .field label { font-size: .8rem; color: var(--muted); }
  .modal .field input[type=text] { padding: .5rem .65rem; font: inherit;
    border: 1px solid var(--line); border-radius: 8px; background: var(--panel); color: var(--ink); }
  .modal .toggle { display: flex; align-items: center; gap: .55rem; padding: .3rem 0; cursor: pointer; }
  .modal .toggle input { width: 1.1rem; height: 1.1rem; }
  .modal .actions { display: flex; gap: .5rem; justify-content: flex-end; margin-top: 1rem; }
  .modal .actions button { padding: .5rem 1rem; font: inherit; font-weight: 600; cursor: pointer; border: 0; border-radius: 8px; }
  .modal .actions .save { background: var(--accent); color: #fff; }
  .modal .actions .cancel { background: var(--line); color: var(--ink); }
  .modal .saved { color: var(--me); font-size: .82rem; margin-right: auto; align-self: center; }

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

<!-- Floating settings control: outside the appbar so it survives the embed-mode
     appbar hide (the pdn slot supplies its own chrome). -->
<button class="settings" id="settingsbtn" title="Settings">Settings</button>

<!-- Federation (admin) control — revealed only after a successful GET /peers
     confirms the viewer holds admin scope (S5). Opens the federation status panel
     + inbound-peer allow-list editor. -->
<button class="fed" id="fedbtn" title="Federation (admin)">Federation</button>

<!-- Federation panel (S5, admin scope): the known mesh-node graph, live per-link
     state/last-seen/RTT, the operator-configured outbound peers, and the
     inbound-peer allow-list editor (add/remove against the live shared list). -->
<div class="modal" id="fedmodal" role="dialog" aria-modal="true" aria-labelledby="fedtitle">
  <div class="sheet">
    <h2 id="fedtitle">Federation</h2>
    <p class="sub">Linked nodes, per-link health, and the inbound-peer allow-list. Edits apply live to every ingress and survive a restart.</p>

    <div class="fedsec">
      <h3>Inbound allow-list</h3>
      <div id="allowlist"></div>
      <form class="allowadd" id="allowadd">
        <input id="allowcall" autocomplete="off" autocapitalize="characters" placeholder="add callsign e.g. GB7NDH-1">
        <button title="Allow this peer to link in">Add</button>
      </form>
      <div id="pinnedlist"></div>
    </div>

    <div class="fedsec">
      <h3>Known nodes</h3>
      <div id="fednodes"></div>
    </div>

    <div class="fedsec">
      <h3>Live links</h3>
      <div id="fedlinks"></div>
    </div>

    <div class="fedsec">
      <h3>Configured outbound peers</h3>
      <div id="fedconfigured"></div>
    </div>

    <div class="actions">
      <span class="saved" id="fed-msg" hidden></span>
      <button type="button" class="cancel" id="fed-close">Close</button>
    </div>
  </div>
</div>

<!-- Settings pane (S3): name/QTH + the BPQ display flags. A flip POSTs to
     /settings, which persists into the hub user so RF/mesh peers see the same
     identity — not a web-only preference. -->
<div class="modal" id="settingsmodal" role="dialog" aria-modal="true" aria-labelledby="settingstitle">
  <form class="sheet" id="settingsform">
    <h2 id="settingstitle">Settings</h2>
    <p class="sub">Your name, QTH, and display preferences. Changes apply to your identity everywhere — web, RF, and across linked nodes.</p>
    <div class="field">
      <label for="set-name">Name</label>
      <input type="text" id="set-name" name="name" autocomplete="off" placeholder="e.g. Paula">
    </div>
    <div class="field">
      <label for="set-qth">QTH</label>
      <input type="text" id="set-qth" name="qth" autocomplete="off" placeholder="e.g. Kidderminster">
    </div>
    <label class="toggle"><input type="checkbox" id="set-echo" name="echo"> Echo my own messages</label>
    <label class="toggle"><input type="checkbox" id="set-bells" name="bells"> Bells (alert on activity)</label>
    <label class="toggle"><input type="checkbox" id="set-colour" name="colour"> Colour</label>
    <label class="toggle"><input type="checkbox" id="set-shownames" name="shownames"> Show names</label>
    <label class="toggle"><input type="checkbox" id="set-showtime" name="showtime"> Show timestamps</label>
    <div class="actions">
      <span class="saved" id="set-saved" hidden>Saved</span>
      <button type="button" class="cancel" id="set-cancel">Cancel</button>
      <button type="submit" class="save">Save</button>
    </div>
  </form>
</div>
<main>
  <nav class="channels">
    <h2>Channels</h2>
    <div class="list" id="chanlist"></div>
    <form class="newchan" id="newchan">
      <input id="newchaninput" autocomplete="off" placeholder="new channel">
      <button title="Join channel">+</button>
    </form>
    <h2>Direct messages</h2>
    <div class="list" id="dmlist"></div>
    <form class="newdm" id="newdm">
      <input id="newdminput" autocomplete="off" autocapitalize="characters" placeholder="DM a callsign">
      <button title="Open DM thread">+</button>
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

// DM state (S6). dms maps an UPPER-CASED correspondent callsign to its thread:
// an ordered list of {from,to,text,time,mine} plus an unread counter shown as a
// rail badge while you're viewing something else. dmWith is the correspondent of
// the open DM thread, or null when a channel (topic) is in view — the single
// toggle that decides where the composer routes (/dm vs /send) and what #log shows.
const dms = new Map();
let dmWith = null;
const dmlistEl = document.getElementById('dmlist');

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
    handleDM(e); // route DMs into the DM pane (S6), not the channel timeline
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
    b.className = 'chan' + (dmWith===null && name.toLowerCase()===topic.toLowerCase()?' active':'');
    b.innerHTML = '<span class="hash">#</span>'+esc(name);
    b.onclick = () => switchTopic(name);
    chanlistEl.appendChild(b);
  });
}

// --- DMs (S6) ---
// A correspondent's thread is the OTHER party (e.with is computed server-side from
// the viewer's POV, so a thread is keyed the same whether sent or received). A DM
// that arrives while you're elsewhere bumps the thread's unread badge; opening the
// thread clears it and renders the thread into the log.
function dmThread(call){
  const k = call.toUpperCase();
  let t = dms.get(k);
  if (!t) { t = {call: k, msgs: [], unread: 0}; dms.set(k, t); }
  return t;
}

function dmLine(m){
  return ts(m.time)+'<span class="from">'+(m.mine?'me':esc(m.with||m.from))+':</span> '+esc(m.text);
}

// handleDM ingests one private wireEvent into its thread. If that thread is open
// it appends live; otherwise it counts an unread. Always re-renders the DM rail so
// a new correspondent appears and badges update.
function handleDM(e){
  const t = dmThread(e.with || (e.mine ? e.to : e.from));
  const m = {with: t.call, from: e.from, to: e.to, text: e.text, time: e.time, mine: !!e.mine};
  t.msgs.push(m);
  if (dmWith === t.call) {
    add(dmLine(m), 'dm' + (m.mine?' mine':''));
  } else {
    t.unread++;
  }
  renderDMs();
}

function renderDMs(){
  dmlistEl.innerHTML = '';
  Array.from(dms.values())
    .sort((a,b)=>a.call.localeCompare(b.call))
    .forEach(t => {
      const b = document.createElement('button');
      b.className = 'dm chan' + (dmWith===t.call?' active':'');
      let h = '<span class="glyph">@</span>'+esc(t.call);
      if (t.unread && dmWith!==t.call) h += '<span class="badge">'+t.unread+'</span>';
      b.innerHTML = h;
      b.onclick = () => openDM(t.call);
      dmlistEl.appendChild(b);
    });
}

// openDM switches the conversation column to a DM thread: clears the channel-active
// state, renders the stored thread, and points the composer at /dm. We do NOT touch
// the SSE stream — live DMs arrive on the same stream and handleDM appends them.
function openDM(call){
  const t = dmThread(call);
  dmWith = t.call;
  t.unread = 0;
  topicEl.textContent = t.call;
  document.querySelector('.convohead .hash').textContent = '@';
  usercountEl.textContent = 'direct message';
  msgEl.placeholder = 'Message '+t.call+'  (private)';
  log.innerHTML = '';
  t.msgs.forEach(m => add(dmLine(m), 'dm' + (m.mine?' mine':'')));
  log.scrollTop = log.scrollHeight;
  renderChannels();
  renderDMs();
  msgEl.focus();
}

// leaveDM returns the column to the current topic view (called when a channel is
// selected). It does not lose any DM state — threads stay in the dms map.
function leaveDM(){
  if (dmWith === null) return;
  dmWith = null;
  document.querySelector('.convohead .hash').textContent = '#';
  renderDMs();
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
  const wasDM = dmWith !== null;
  if (!wasDM && name.toLowerCase() === topic.toLowerCase()) return;
  leaveDM();
  usercountEl.textContent = '';
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
    // Fresh snapshot replaces the timeline so a reconnect backfills cleanly. DM
    // threads are rebuilt from the persisted /dms backfill (the live stream never
    // replays old DMs); re-render whichever view is open afterwards.
    log.innerHTML = '';
    loadDMs();
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

// loadDMs backfills persisted DM threads on (re)connect. It rebuilds the dms map
// from scratch (so a reconnect doesn't duplicate) and, if a DM thread is open,
// re-renders it; otherwise it just refreshes the rail (with unread badges intact
// at 0 — backfilled history is not "unread").
async function loadDMs(){
  const r = await fetch('dms');
  if (!r.ok) return;
  const list = await r.json();
  dms.clear();
  list.forEach(e => {
    const t = dmThread(e.with || (e.mine ? e.to : e.from));
    t.msgs.push({with: t.call, from: e.from, to: e.to, text: e.text, time: e.time, mine: !!e.mine});
  });
  if (dmWith !== null && dms.has(dmWith)) {
    const t = dms.get(dmWith);
    log.innerHTML = '';
    t.msgs.forEach(m => add(dmLine(m), 'dm' + (m.mine?' mine':'')));
    log.scrollTop = log.scrollHeight;
  }
  renderDMs();
}

document.getElementById('f').addEventListener('submit', async ev => {
  ev.preventDefault();
  const text = msgEl.value.trim(); if (!text) return; msgEl.value = '';
  const t = text.match(/^\/[tT]\s+(.+)$/);
  if (t) { await switchTopic(t[1].trim()); return; }
  // /S CALL text — the BPQ private-message command; works from any view. It opens
  // (or reuses) the correspondent's thread and sends through /dm.
  const s = text.match(/^\/[sS]\s+(\S+)\s+(.+)$/);
  if (s) { const call = s[1].toUpperCase(); openDM(call); await sendDM(call, s[2].trim()); return; }
  // In a DM thread the composer IS a DM compose (= /S correspondent text).
  if (dmWith !== null) { await sendDM(dmWith, text); return; }
  await post('send', {text});
});

// sendDM composes a direct message through /dm — the web compose path that drives
// the same hub.Private the RF /S command does. A 403 is the read-only lurker gate;
// a 400 is an offline/unknown recipient (the "/S that user is not logged in" case).
async function sendDM(call, text){
  const r = await fetch('dm', {method:'POST', headers:{'Content-Type':'application/json'},
    body: JSON.stringify({to: call, text})});
  if (r.status === 403) {
    alert('Your access is read-only — you cannot send messages.');
  } else if (!r.ok) {
    alert(call + ' is not logged in — direct messages reach connected users only.');
  }
}

document.getElementById('newchan').addEventListener('submit', async ev => {
  ev.preventDefault();
  const inp = document.getElementById('newchaninput');
  const name = inp.value.trim(); if (!name) return; inp.value = '';
  await switchTopic(name);
  msgEl.focus();
});

document.getElementById('newdm').addEventListener('submit', ev => {
  ev.preventDefault();
  const inp = document.getElementById('newdminput');
  const call = inp.value.trim().toUpperCase(); if (!call) return; inp.value = '';
  openDM(call); // opens (or creates) the thread; the composer now routes to /dm
});

// --- Settings pane (S3) ---
// A flip is the persisted identity RF/mesh peers see: it round-trips through
// /settings, which writes name/QTH/flags into the hub user. GET prefills the form
// from what the hub currently holds; POST persists and echoes the now-current
// state so the form reflects the truth (not just the local guess).
const settingsModal = document.getElementById('settingsmodal');
const setSaved = document.getElementById('set-saved');
const SET_BOOLS = ['echo','bells','colour','shownames','showtime'];

function fillSettings(s){
  document.getElementById('set-name').value = s.name || '';
  document.getElementById('set-qth').value = s.qth || '';
  SET_BOOLS.forEach(k => { document.getElementById('set-'+k).checked = !!s[k]; });
}

async function openSettings(){
  setSaved.hidden = true;
  try {
    const r = await fetch('settings');
    if (r.ok) fillSettings(await r.json());
  } catch (e) { /* keep whatever the form has */ }
  settingsModal.classList.add('open');
  document.getElementById('set-name').focus();
}
function closeSettings(){ settingsModal.classList.remove('open'); }

document.getElementById('settingsbtn').addEventListener('click', openSettings);
document.getElementById('set-cancel').addEventListener('click', closeSettings);
settingsModal.addEventListener('click', ev => { if (ev.target === settingsModal) closeSettings(); });

document.getElementById('settingsform').addEventListener('submit', async ev => {
  ev.preventDefault();
  const payload = {
    name: document.getElementById('set-name').value.trim(),
    qth: document.getElementById('set-qth').value.trim(),
  };
  SET_BOOLS.forEach(k => { payload[k] = document.getElementById('set-'+k).checked; });
  const r = await fetch('settings', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(payload)});
  if (r.ok) {
    fillSettings(await r.json()); // reflect the persisted truth
    setSaved.hidden = false;
    setTimeout(closeSettings, 600);
  } else if (r.status === 403) {
    alert('Your access is read-only — you cannot change settings.');
  } else {
    alert('Could not save settings.');
  }
});

// --- Federation panel (S5, admin scope) ---
// The split federation decision: origin-node badges (above) render for EVERYONE;
// the topology + per-link state and the allow-list editor are admin-only. The SPA
// discovers admin scope by probing GET /peers — a 200 reveals the button; a 403
// (not admin) or 503 (no federation wiring) leaves it hidden. The editor POSTs to
// /peers/allow, which mutates the live shared allow-list and persists it.
const fedBtn = document.getElementById('fedbtn');
const fedModal = document.getElementById('fedmodal');
const fedMsg = document.getElementById('fed-msg');

function relTime(ms){
  if (!ms) return '—';
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 60) return s + 's ago';
  if (s < 3600) return Math.round(s/60) + 'm ago';
  return Math.round(s/3600) + 'h ago';
}

function renderAllow(view){
  const al = document.getElementById('allowlist');
  al.innerHTML = '';
  const allow = view.allow || [];
  if (!allow.length) {
    al.innerHTML = '<div class="empty">No inbound peers allowed — default-deny.</div>';
  } else {
    allow.forEach(call => {
      const row = document.createElement('div'); row.className = 'allowrow';
      row.innerHTML = '<span class="call">'+esc(call)+'</span>';
      const rm = document.createElement('button'); rm.className = 'rm'; rm.textContent = 'Remove';
      rm.onclick = () => editAllow('remove', call);
      row.appendChild(rm);
      al.appendChild(row);
    });
  }
  const pl = document.getElementById('pinnedlist');
  pl.innerHTML = '';
  (view.pinned || []).forEach(call => {
    const row = document.createElement('div'); row.className = 'allowrow';
    row.innerHTML = '<span class="call">'+esc(call)+'</span><span class="pin">dialed — always allowed</span>';
    pl.appendChild(row);
  });
}

function renderFedNodes(view){
  const el = document.getElementById('fednodes');
  const nodes = view.nodes || [];
  if (!nodes.length) { el.innerHTML = '<div class="empty">No nodes linked.</div>'; return; }
  let h = '<table><tr><th>Node</th><th>Alias</th><th>Version</th><th>Linked</th></tr>';
  nodes.forEach(n => {
    h += '<tr><td>'+esc(n.call)+'</td><td>'+esc(n.alias||'')+'</td><td>'+esc(n.version||'')+'</td><td>'+relTime(n.linkedSince)+'</td></tr>';
  });
  el.innerHTML = h + '</table>';
}

function renderFedLinks(view){
  const el = document.getElementById('fedlinks');
  const links = view.links || [];
  if (!links.length) { el.innerHTML = '<div class="empty">No live links.</div>'; return; }
  let h = '<table><tr><th>Peer</th><th>Dir</th><th>Last seen</th><th>RTT</th></tr>';
  links.forEach(l => {
    h += '<tr><td>'+esc(l.peer)+'</td><td>'+(l.outbound?'out':'in')+'</td><td>'+relTime(l.lastSeen)+'</td><td>'+(l.rttMs?l.rttMs+'ms':'—')+'</td></tr>';
  });
  el.innerHTML = h + '</table>';
}

function renderFedConfigured(view){
  const el = document.getElementById('fedconfigured');
  const peers = view.configured || [];
  if (!peers.length) { el.innerHTML = '<div class="empty">None configured.</div>'; return; }
  let h = '<table><tr><th>Peer</th><th>Transport</th><th>Target</th></tr>';
  peers.forEach(p => {
    h += '<tr><td>'+esc(p.call)+'</td><td>'+esc(p.transport)+'</td><td>'+esc(p.target||'')+'</td></tr>';
  });
  el.innerHTML = h + '</table>';
}

function renderFed(view){
  renderAllow(view);
  renderFedNodes(view);
  renderFedLinks(view);
  renderFedConfigured(view);
}

async function loadFed(){
  const r = await fetch('peers');
  if (!r.ok) return null;
  return r.json();
}

// probeFed reveals the Federation button iff GET /peers is admitted (admin scope).
async function probeFed(){
  try {
    const r = await fetch('peers');
    if (r.ok) { fedBtn.classList.add('show'); }
  } catch (e) { /* leave hidden */ }
}
probeFed();

async function openFed(){
  fedMsg.hidden = true;
  const view = await loadFed();
  if (view) renderFed(view);
  fedModal.classList.add('open');
}
function closeFed(){ fedModal.classList.remove('open'); }

async function editAllow(action, call){
  const r = await fetch('peers/allow', {method:'POST', headers:{'Content-Type':'application/json'},
    body: JSON.stringify({action, callsign: call})});
  if (r.ok) {
    const view = await loadFed(); // refetch the full panel so links/nodes stay current
    if (view) renderFed(view);
    fedMsg.textContent = action === 'add' ? ('Allowed '+call) : ('Removed '+call);
    fedMsg.hidden = false;
  } else if (r.status === 403) {
    alert('Admin scope is required to edit the allow-list.');
  } else {
    alert('Could not apply the allow-list edit.');
  }
}

fedBtn.addEventListener('click', openFed);
document.getElementById('fed-close').addEventListener('click', closeFed);
fedModal.addEventListener('click', ev => { if (ev.target === fedModal) closeFed(); });
document.getElementById('allowadd').addEventListener('submit', async ev => {
  ev.preventDefault();
  const inp = document.getElementById('allowcall');
  const call = inp.value.trim().toUpperCase(); if (!call) return; inp.value = '';
  await editAllow('add', call);
});
</script>
</body>
</html>
`)
