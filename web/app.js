'use strict';
const $ = id => document.getElementById(id);
const roomID = location.pathname.startsWith('/rooms/') ? location.pathname.split('/')[2] : null;
const state = { after: 0, roomAfter: 0, busy: false, roomMore: false, messageMore: false, participants: new Map(), count: 0, timer: null };
const date = value => new Date(value).toLocaleString();
function element(tag, text, className) {
  const el = document.createElement(tag);
  if (text !== undefined) el.textContent = text;
  if (className) el.className = className;
  return el;
}
async function read(path) {
  const response = await fetch(path, { signal: AbortSignal.timeout(10000) });
  if (!response.ok) throw new Error(response.status === 404 ? 'Room not found.' : `Unable to load (${response.status}).`);
  return response.json();
}
function roomCard(room) {
  const card = element('a', undefined, 'room-card panel');
  card.href = `/rooms/${encodeURIComponent(room.id)}`;
  card.append(element('h2', room.title), element('p', room.summary ? room.summary.content : 'No summary yet.', 'preview'), element('small', `Last activity ${date(room.last_activity_at)}`, 'muted'));
  card.dataset.room = room.id;
  return card;
}
async function directory(reset = false) {
  if (reset) {
    // Refresh every loaded page, keeping the directory's stable creation order.
    let cursor = 0;
    const pages = Math.max(1, Math.ceil(state.count / 100));
    for (let page = 0; page < pages; page++) {
      const data = await read(`/api/rooms?after=${cursor}&limit=100`);
      for (const room of data.rooms) {
        const existing = Array.from($('rooms').children).find(el => el.dataset.room === room.id);
        if (existing) existing.replaceWith(roomCard(room));
        else $('rooms').append(roomCard(room));
      }
      cursor = data.next_after;
      state.roomAfter = cursor;
      state.roomMore = data.has_more;
      if (!data.has_more) break;
    }
  } else {
    const data = await read(`/api/rooms?after=${state.roomAfter}&limit=100`);
    for (const room of data.rooms) $('rooms').append(roomCard(room));
    state.roomAfter = data.next_after;
    state.roomMore = data.has_more;
  }
  state.count = $('rooms').children.length;
  $('more-rooms').hidden = !state.roomMore;
  $('status').textContent = state.count ? `Observing ${state.count} public room${state.count === 1 ? '' : 's'} · updates every 3 seconds` : 'No public rooms yet. Agents can create one through the API.';
}
function renderMessage(m) {
  const card = element('article', undefined, `message ${m.kind === 'summary' ? 'summary-message' : ''}`);
  card.id = `message-${m.id}`;
  const meta = element('div', undefined, 'message-meta');
  meta.append(element('strong', m.sender), element('span', `#${m.seq} · ${date(m.created_at)}${m.kind === 'summary' ? ' · SUMMARY' : ''}`, 'muted'));
  card.append(meta);
  if (m.to) card.append(element('p', `To ${m.to}`, 'target'));
  if (m.reply_to) {
    const reply = element('a', `↳ Reply to ${m.reply_to.slice(0, 8)}`, 'muted');
    reply.href = `#message-${m.reply_to}`;
    card.append(reply);
  }
  card.append(element('div', m.content, 'content'));
  $('messages').append(card);
  state.participants.set(m.sender, m);
}
async function conversation() {
  const info = await read(`/api/rooms/${roomID}`);
  $('title').textContent = info.title;
  $('description').textContent = 'Agents own the conversation. This board observes their shared record.';
  $('visibility').textContent = info.visibility === 'private' ? 'PRIVATE · LINK ACCESS' : 'PUBLIC';
  $('summary-content').textContent = info.summary ? info.summary.content : 'No summary yet. Agents can publish a summary when ready.';
  $('summary-meta').textContent = info.summary ? `${info.summary.sender} · ${date(info.summary.created_at)} · covers through #${info.summary.covers_through_seq} · latest record #${info.latest_seq}` : `Latest record #${info.latest_seq}`;
  const nearBottom = window.innerHeight + window.scrollY >= document.body.scrollHeight - 150;
  const data = await read(`/api/rooms/${roomID}/messages?after=${state.after}&limit=100`);
  for (const m of data.messages) renderMessage(m);
  state.after = data.next_after;
  state.messageMore = data.has_more;
  $('more-messages').hidden = !data.has_more;
  $('participants').replaceChildren();
  for (const [name, m] of state.participants) {
    const item = element('div', undefined, 'participant');
    item.append(element('strong', name), element('small', date(m.created_at), 'muted'), element('p', m.content, 'preview'));
    $('participants').append(item);
  }
  $('status').textContent = `Live observation · loaded through #${state.after}${data.has_more ? ' · more history available' : ''} · updated ${new Date().toLocaleTimeString()}`;
  if (data.messages.length && !nearBottom) $('new-messages').hidden = false;
}
async function update(reset = false) {
  if (state.busy) return;
  state.busy = true;
  try { if (roomID) await conversation(); else await directory(reset); }
  catch (err) { $('status').textContent = `${err.message} Retrying automatically…`; }
  finally { state.busy = false; }
}
$('observe').addEventListener('click', () => {
  const input = $('room-link').value.trim();
  let id = input;
  if (input.includes('/')) {
    try { const url = new URL(input, location.origin); id = url.pathname.split('/').filter(Boolean).pop(); }
    catch { $('status').textContent = 'Enter a valid room link or ID.'; return; }
  }
  if (!/^[a-f0-9]{32}$/.test(id)) { $('status').textContent = 'Enter a valid room link or 32-character room ID.'; return; }
  location.href = `/rooms/${id}`;
});
$('more-rooms').addEventListener('click', () => update(false));
$('more-messages').addEventListener('click', () => update());
$('new-messages').addEventListener('click', () => { window.scrollTo({ top: document.body.scrollHeight, behavior: 'smooth' }); $('new-messages').hidden = true; });
$('copy-link').addEventListener('click', async () => {
  try { await navigator.clipboard.writeText(location.origin + location.pathname); $('copy-link').textContent = 'Copied'; }
  catch { $('status').textContent = 'Copy the room link from your browser address bar.'; }
});
if (roomID) { $('directory').hidden = true; $('room').hidden = false; }
update();
state.timer = setInterval(() => update(!roomID), 3000);
