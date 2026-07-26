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

function sessionTable(sessions) {
  const table = document.createElement('table');
  const head = document.createElement('tr');
  for (const h of ['', 'role', 'session', 'client', 'fps in/dec', 'stall', 'latency', 'delivery', 'relay', 'verdict']) {
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
      tr.appendChild(text('td', 'num', num(m.captureFps ?? m.CaptureFps) + ' / ' + num(m.encoderFps ?? m.EncoderFps)));
      tr.appendChild(text('td', 'num', num(m.encoderQueueDepth)));
      tr.appendChild(text('td', 'num', '—'));
    } else {
      tr.appendChild(text('td', 'num', num(m.receivedFps) + ' / ' + num(m.decoderFps)));
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
  card.open = worst === 'bad' || worst === 'warn';

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
  root.textContent = '';

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

poll();
setInterval(poll, POLL_MS);
