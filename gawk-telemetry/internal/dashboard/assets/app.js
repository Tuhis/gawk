// R28 TM8 (docs/33 §4.8). The dashboard's single design goal: someone who
// opens it during a live stream should see whether anything is wrong BEFORE
// they click anything. Everything below follows from that.
//
// It POLLS one endpoint every 2 s rather than using SSE or a WebSocket:
// polling has no connection state to lose, survives any proxy, is trivially
// debuggable with curl, and 2 s against an in-memory projection costs nothing
// at this scale.

const POLL_MS = 2000;

// Severity -> glyph. NEVER colour alone: the glyph and the word both carry the
// state, so the page survives a colour-blind reader, a greyscale screenshot,
// and the CSS failing to load entirely.
const GLYPH = { ok: '○', warn: '△', bad: '●', unknown: '?' };

// The page rebuilds every card from scratch on each poll, so an expanded card
// has to survive the rebuild or it is unreadable: a human reading a session
// table is slower than 2 s, and the card was snapping shut under them.
//
// What is carried is NOT "is this card open". A card opens by DEFAULT when
// something is wrong — that is the dashboard's whole premise, and remembering
// raw open-state would defeat it by pinning a card open long after it went
// quiet. What is carried is the operator's DISAGREEMENT with the default: each
// card records the default it was handed in `data-default`, and anything
// differing from that on the next rebuild is a deliberate act and wins.
//
// The override dissolves once the default catches up with it, so a card kept
// open through a `bad` spell goes back to following severity after the operator
// has also seen it recover. Keyed by broadcastKey so a card stays open when the
// broadcast ends underneath it and the row moves to the recessed group.
const openOverrides = new Map();

function captureOpenState(root) {
  for (const card of root.querySelectorAll('details.bcast[data-key]')) {
    if (card.open === (card.dataset.default === '1')) openOverrides.delete(card.dataset.key);
    else openOverrides.set(card.dataset.key, card.open);
  }
}

function sev(severity) {
  const s = severity || 'unknown';
  const el = document.createElement('span');
  el.className = 'sev sev-' + s;
  el.textContent = GLYPH[s] + ' ' + s;
  return el;
}

function text(tag, cls, content) {
  const el = document.createElement(tag);
  if (cls) el.className = cls;
  if (content !== undefined) el.textContent = content;
  return el;
}

function ago(ms) {
  if (ms === undefined || ms === null || ms < 0) return 'never';
  if (ms < 1000) return 'now';
  const s = Math.round(ms / 1000);
  if (s < 60) return s + 's ago';
  const m = Math.round(s / 60);
  if (m < 60) return m + 'm ago';
  return Math.round(m / 60) + 'h ago';
}

function dur(ms) {
  if (!ms || ms < 0) return '—';
  const s = Math.round(ms / 1000);
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return m + 'm ' + (s % 60) + 's';
  return Math.floor(m / 60) + 'h ' + (m % 60) + 'm';
}

function num(v, digits) {
  if (v === undefined || v === null) return '—';
  return digits ? v.toFixed(digits) : String(Math.round(v));
}

// A session row's freshness is reported PER SIDE, because the relay side and
// the client side are refreshed by different mechanisms at different rates.
// Painting them as one instant would be a lie a viewer's absence could hide
// behind.
function freshness(state, ageMs) {
  if (state === 'unknown') return 'never reported';
  if (state === 'stale') return 'stale ' + ago(ageMs);
  return ago(ageMs);
}

// The dip reading (docs/33 D16): how many times this session's rate collapsed
// inside the live window, and how far. This is the column that makes an
// intermittent stutter visible on a page whose other numbers are, by
// construction, whatever the stream happened to be doing at the last sample.
//
// A window with no baseline yet reads '—', never '0': "we cannot judge" and
// "we judged and it was clean" are different claims, and only the second is
// evidence.
function dips(m) {
  if (m.fpsDipEpisodes === undefined) return '—';
  if (!m.fpsDipEpisodes) return '0';
  const worst = m.fpsDipWorstFps === undefined ? '' : ' ↓' + num(m.fpsDipWorstFps);
  return m.fpsDipEpisodes + '×' + worst;
}

function sessionTable(sessions) {
  const table = document.createElement('table');
  const head = document.createElement('tr');
  for (const h of ['', 'role', 'session', 'client', 'fps in/dec', 'dips', 'stall', 'latency', 'delivery', 'relay', 'verdict']) {
    head.appendChild(text('th', null, h));
  }
  table.appendChild(head);

  for (const s of sessions) {
    const tr = document.createElement('tr');
    const c0 = document.createElement('td');
    c0.appendChild(sev(s.severity));
    tr.appendChild(c0);

    tr.appendChild(text('td', 'role role-' + s.role, s.role));
    tr.appendChild(text('td', null, s.sessionId.slice(0, 8)));
    tr.appendChild(text('td', null, [s.browser, s.os].filter(Boolean).join(' / ') || '—'));

    const m = s.metrics || {};
    if (s.role === 'broadcaster') {
      // Capture / encode, with the configured target beside them (D17) — the
      // number a rate has to be read against to mean anything.
      const target = m.targetFps === undefined ? '' : ' →' + num(m.targetFps);
      tr.appendChild(
        text(
          'td',
          'num',
          num(m.captureFps ?? m.CaptureFps) + ' / ' + num(m.encoderFps ?? m.EncoderFps) + target,
        ),
      );
      tr.appendChild(text('td', 'num', dips(m)));
      tr.appendChild(text('td', 'num', num(m.encoderQueueDepth)));
      tr.appendChild(text('td', 'num', '—'));
    } else {
      tr.appendChild(text('td', 'num', num(m.receivedFps) + ' / ' + num(m.decoderFps)));
      tr.appendChild(text('td', 'num', dips(m)));
      tr.appendChild(text('td', 'num', m.timeSinceLastFrameMs === undefined ? '—' : num(m.timeSinceLastFrameMs) + 'ms'));
      tr.appendChild(text('td', 'num', m.capToRenderMs === undefined ? '—' : num(m.capToRenderMs) + 'ms'));
    }

    const cfg = s.config || {};
    tr.appendChild(text('td', null, cfg.deliveryMode || cfg.Encoder || '—'));

    // The two freshness readings, side by side and labelled, never merged.
    const fresh = text('td', s.clientState === 'reporting' ? null : 'stale');
    fresh.textContent = 'c:' + freshness(s.clientState, s.clientAgeMs) + '  r:' + freshness(s.relayState, s.relayAgeMs);
    tr.appendChild(fresh);

    const v = document.createElement('td');
    v.className = 'verdict';
    v.textContent = s.verdict || '';
    if (s.findings && s.findings.length) {
      const ul = text('ul', 'findings');
      for (const f of s.findings.slice(0, 3)) {
        ul.appendChild(text('li', null, f.verdict));
      }
      v.appendChild(ul);
    }
    tr.appendChild(v);
    table.appendChild(tr);
  }
  return table;
}

function broadcastCard(b, ended) {
  const worst = b.worstViewer && rank(b.worstViewer) > rank(b.severity) ? b.worstViewer : b.severity;
  const card = document.createElement('details');
  card.className = 'bcast' + (worst === 'bad' ? ' has-bad' : worst === 'warn' ? ' has-warn' : '');
  // Severity decides the default; the operator's recorded disagreement, if any,
  // overrides it. See openOverrides above for why it is stored that way round.
  const byDefault = worst === 'bad' || worst === 'warn';
  card.dataset.key = b.broadcastKey;
  card.dataset.default = byDefault ? '1' : '0';
  card.open = openOverrides.has(b.broadcastKey) ? openOverrides.get(b.broadcastKey) : byDefault;

  const sum = document.createElement('summary');
  sum.appendChild(sev(worst));
  sum.appendChild(text('span', 'key', b.broadcastKey.slice(0, 8)));

  // Lifecycle is a SEPARATE dimension from severity, and it is labelled in
  // words: a broadcaster in the R1 grace period is `away`, which is not a
  // fault. An ended row's claim is past tense, because "is stuttering" and
  // "stuttered" are not the same statement and rendering them identically
  // invites acting on a problem that is already over.
  const life = ended
    ? 'ended ' + ago(b.endedAgoMs)
    : b.lifecycle === 'live' ? 'LIVE' : b.lifecycle;
  sum.appendChild(text('span', 'lifecycle', life));

  const facts = text('span', 'facts');
  facts.appendChild(text('span', null, b.viewers + ' viewer' + (b.viewers === 1 ? '' : 's')));
  if (!ended) facts.appendChild(text('span', null, 'up ' + dur(b.uptimeMs)));
  const m = b.metrics || {};
  if (m.ingressLossRatio !== undefined) {
    facts.appendChild(text('span', null, 'ingress loss ' + (m.ingressLossRatio * 100).toFixed(2) + '%'));
  }
  if (m.datagramsDropped) facts.appendChild(text('span', null, 'egress drops ' + num(m.datagramsDropped)));
  if (b.pod) facts.appendChild(text('span', null, b.pod + (b.role ? ' (' + b.role + ')' : '')));
  sum.appendChild(text('span', 'spacer'));
  sum.appendChild(facts);
  card.appendChild(sum);

  if (b.findings && b.findings.length) {
    const ul = text('ul', 'findings');
    for (const f of b.findings) ul.appendChild(text('li', null, f.verdict));
    card.appendChild(ul);
  }
  // The broadcaster and its viewers are ONE table: they are the same kind of
  // thing (both are sessions with tokens), distinguished by a role column,
  // with the broadcaster pinned first by the server's ordering.
  if (b.sessions && b.sessions.length) card.appendChild(sessionTable(b.sessions));
  return card;
}

function rank(s) {
  return { bad: 3, warn: 2, unknown: 1, ok: 0 }[s] || 0;
}

function render(snap) {
  const root = document.getElementById('root');
  // Read the operator's expand/collapse choices off the outgoing DOM before it
  // is discarded — the rebuild is what was destroying them.
  captureOpenState(root);
  root.textContent = '';

  // Bound the override map to what is still on the page. A broadcast that has
  // aged out of both groups can never be re-rendered, so its choice is dead
  // weight; keeping it would leak an entry per broadcast for the process's life.
  const alive = new Set();
  for (const b of snap.live || []) alive.add(b.broadcastKey);
  for (const b of snap.ended || []) alive.add(b.broadcastKey);
  for (const key of openOverrides.keys()) {
    if (!alive.has(key)) openOverrides.delete(key);
  }

  // Live first, ALWAYS as its own group. The grouping IS the precedence: a
  // live `warn` outranks an ended `bad`, because only the live one can still
  // be acted on. The two are never interleaved.
  root.appendChild(text('h2', null, 'Live'));
  if (!snap.live || snap.live.length === 0) {
    root.appendChild(text('div', 'empty', 'Nothing is streaming right now.'));
  } else {
    for (const b of snap.live) root.appendChild(broadcastCard(b, false));
  }

  if (snap.ended && snap.ended.length) {
    root.appendChild(text('h2', null, 'Recently ended'));
    root.appendChild(text('div', 'group-note',
      'Stored verdicts from finished broadcasts. Nothing here can still be acted on.'));
    const wrap = text('div', 'ended');
    for (const b of snap.ended) wrap.appendChild(broadcastCard(b, true));
    root.appendChild(wrap);
  }

  // The rebuild drops the found-marker with everything else, so re-apply it —
  // the same reason the open/collapsed state has to be carried across.
  markFound(root);
}

// --- Find a stream by its code -------------------------------------------
//
// The page can only ever show the OBFUSCATED broadcast key: the raw code is a
// join credential and telemetry is never told one. So the lookup runs on the
// server, one way — POST the code, get back the digest the relay would have
// published for it, highlight that row.
//
// POST, not GET: a join credential in a query string lands in browser history,
// the Referer header and any proxy log. It is also never kept — `foundKey` is a
// digest, which is what the page already displays everywhere else.
let foundKey = null;

function markFound(root) {
  for (const card of root.querySelectorAll('details.bcast')) {
    card.classList.toggle('found', foundKey !== null && card.dataset.key === foundKey);
  }
}

async function findByCode(ev) {
  ev.preventDefault();
  const msg = document.getElementById('find-msg');
  const code = document.getElementById('code').value.trim();
  if (!code) {
    foundKey = null;
    msg.textContent = '';
    markFound(document.getElementById('root'));
    return;
  }
  try {
    const res = await fetch('v1/resolve', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code }),
    });
    if (res.status === 501) {
      msg.textContent = 'lookup not configured';
      return;
    }
    if (!res.ok) throw new Error('HTTP ' + res.status);
    foundKey = (await res.json()).broadcastKey;
    const root = document.getElementById('root');
    markFound(root);
    const hit = root.querySelector('details.bcast.found');
    // A code that resolves to nothing on the page is the COMMON case, not an
    // error: the broadcast may have ended and aged out, or never existed. Say
    // which, rather than leaving the operator staring at an unchanged page.
    if (hit) {
      hit.open = true;
      hit.scrollIntoView({ block: 'nearest' });
      msg.textContent = '';
    } else {
      msg.textContent = 'no live or recent stream with that code';
    }
  } catch (e) {
    msg.textContent = 'lookup failed: ' + e.message;
  }
}

// The box only appears where the server can actually answer, so it never offers
// an action that cannot work.
async function initFind() {
  const form = document.getElementById('find');
  form.addEventListener('submit', findByCode);
  try {
    const probe = await fetch('v1/resolve', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code: '' }),
    });
    // 400 means it tried to parse the (empty) code — the lookup is live.
    if (probe.status !== 501) form.hidden = false;
  } catch { /* leave it hidden */ }
}

async function poll() {
  try {
    const res = await fetch('live', { cache: 'no-store' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const snap = await res.json();
    render(snap);
    document.getElementById('age').textContent =
      'updated ' + new Date(snap.atMs).toLocaleTimeString();
    document.getElementById('error').textContent = '';
  } catch (e) {
    // The page keeps showing the last good state and says the feed is stale.
    // Blanking it on one failed poll would be exactly the "absence of evidence
    // rendered as something else" the health model refuses to do.
    document.getElementById('error').textContent = 'feed unavailable: ' + e.message;
  }
}

initFind();
poll();
setInterval(poll, POLL_MS);
