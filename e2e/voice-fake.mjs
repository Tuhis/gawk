// The in-page fake relay and bridge the voice browser checks share (R79 MV6
// voice-panel.mjs, R80 NS4 voice-rnnoise.mjs), and their small helpers.
// WebTransport itself is replaced in the page (addInitScript) by a stub that
// plays a relay room session (`/room/{code}`: RoomState with the voice
// capability, then a VoiceRosterUpdated, and a VoiceTicket for every
// VoiceJoin) and a bridge voice session (`/voice`: answers the VoiceHello
// with VoiceState and loops every VoiceFrame back as another speaker's).

import { existsSync, readdirSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';

export const PIN = 'ab'.repeat(32);

// A Chrome/Chromium executable: GAWK_E2E_CHROME, the usual system paths,
// then Playwright's cached Chromium; null when there is none.
export function chromePath() {
  const fixed = process.env.GAWK_E2E_CHROME
    ? [process.env.GAWK_E2E_CHROME]
    : ['/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/usr/bin/chromium', '/usr/bin/chromium-browser'];
  for (const c of fixed) if (existsSync(c)) return c;
  const cache = join(homedir(), '.cache', 'ms-playwright');
  if (existsSync(cache)) {
    for (const d of readdirSync(cache).filter((n) => /^chromium-\d+$/.test(n)).sort().reverse()) {
      for (const sub of ['chrome-linux64/chrome', 'chrome-linux/chrome']) {
        const p = join(cache, d, sub);
        if (existsSync(p)) return p;
      }
    }
  }
  return null;
}

// The in-page stub. Runs before any app code; plain JS, its own tiny
// encoders (the layouts are gawk-server/wire/room.go and voice.go's).
export function installFakeWebTransport() {
  const enc = new TextEncoder();
  const log = (window.__voiceE2E = {
    wantCaps: null,
    voiceJoins: 0,
    voiceDials: [],
    roomPins: null,
    helloFlags: null,
    controls: [],
    framesOut: 0,
    framesIn: 0,
    // performance.now() of every VoiceFrame with audio that left (not the
    // empty end-of-spurt terminators).
    frameTimes: [],
  });
  const u16 = (v) => [(v >> 8) & 255, v & 255];
  const u32 = (v) => [(v >>> 24) & 255, (v >>> 16) & 255, (v >>> 8) & 255, v & 255];
  const str8 = (s) => {
    const b = enc.encode(s);
    return [b.length, ...b];
  };
  const record = (msg) => new Uint8Array([...u16(msg.length), ...msg]);
  const roomState = () => [
    1, 0x14, 0x01 /* dynamic */, 0x02 /* voice */, ...u32(3), ...u16(7), ...str8('AB2CD3'), ...str8(''),
    0, 0, 0 /* no attachments */, ...u16(3),
    ...u16(7), 0, 0, ...str8('me'), ...str8(''),
    ...u16(8), 0, 0, ...str8('alice'), ...str8(''),
    ...u16(9), 0, 0, ...str8('a-very-long-nickname-x'), ...str8(''),
  ];
  const entry = (session, kind, pid, flags, name) => [...u32(session), kind, ...u16(pid), flags, ...str8(name)];
  const roster = (withMe) => {
    const entries = [
      entry(3, 0, 0, 0x01, 'bob'),
      entry(4, 0, 0, 0x02 | 0x01, 'Carol the Mumble regular'),
      entry(9, 1, 8, 0, 'alice [web]'),
      entry(11, 1, 9, 0x04, 'a-very-long-nickname-x [web]'),
    ];
    if (withMe) entries.push(entry(42, 1, 7, 0, 'me [web]'));
    return [1, 0x15, ...u32(3), 0x51, 0x01, ...u16(entries.length), ...entries.flat()];
  };
  const ticket = () => {
    const url = enc.encode('https://bridge.test:4443/voice');
    return [1, 0x15, ...u32(3), 0x50, ...u16(url.length), ...url, ...u16(4), 9, 9, 9, 9];
  };
  const voiceState = () => [1, 0x1c, 0x20, ...u32(42), ...str8('me [web]')];

  class FakeWebTransport {
    constructor(url, init) {
      this.url = url;
      const path = new URL(url).pathname;
      this.kind = path.startsWith('/room/') ? 'room' : path === '/voice' ? 'voice' : 'other';
      this.closed = new Promise((res) => (this._close = res));
      this.incomingUnidirectionalStreams = new ReadableStream({});
      let dg;
      this.datagrams = {
        maxDatagramSize: 1200,
        readable: new ReadableStream({ start: (c) => (dg = c) }),
        writable: new WritableStream({ write: (d) => this._datagram(new Uint8Array(d)) }),
      };
      this._dg = dg;
      if (this.kind === 'other') {
        // Anything else (probes, telemetry) finds no server.
        this.ready = Promise.reject(new Error('stubbed: no server'));
        this.ready.catch(() => {});
        this.closed = Promise.reject(new Error('stubbed'));
        this.closed.catch(() => {});
        return;
      }
      this.ready = Promise.resolve();
      const pins = (init?.serverCertificateHashes ?? []).length;
      if (this.kind === 'room') log.roomPins = pins;
      else log.voiceDials.push({ url, pins });
    }
    async createBidirectionalStream() {
      let ctl;
      const readable = new ReadableStream({ start: (c) => (ctl = c) });
      let buf = new Uint8Array(0);
      const onRecord = (rec) => this._record(rec, (msg) => ctl.enqueue(record(msg)));
      const writable = new WritableStream({
        write(chunk) {
          const next = new Uint8Array(buf.length + chunk.length);
          next.set(buf);
          next.set(chunk, buf.length);
          buf = next;
          while (buf.length >= 2) {
            const n = (buf[0] << 8) | buf[1];
            if (buf.length < 2 + n) break;
            onRecord(buf.slice(2, 2 + n));
            buf = buf.slice(2 + n);
          }
        },
      });
      return { readable, writable };
    }
    _record(rec, send) {
      const type = rec[1];
      if (this.kind === 'room') {
        if (type === 0x13) {
          log.wantCaps = rec[4];
          send(roomState());
          send(roster(false));
        } else if (type === 0x16 && rec[2] === 0x50) {
          log.voiceJoins++;
          send(ticket());
        }
        return;
      }
      if (type === 0x1a) {
        log.helloFlags = rec[3];
        send(voiceState());
        // The relay's roster now lists us (the bridge registered the session).
        window.__voiceE2ERoomSend?.(roster(true));
      } else if (type === 0x1d) {
        log.controls.push(rec[2]);
      }
    }
    _datagram(d) {
      if (this.kind !== 'voice' || d[1] !== 0x1b) return;
      log.framesOut++;
      // 12: VOICE_FRAME_HEADER_SIZE; a terminator is a bare header.
      if (d.length > 12) log.frameTimes.push(performance.now());
      // Loop it back as bob's (session 3): the speaking indicator lights.
      const back = d.slice();
      back.set(u32(3), 4);
      log.framesIn++;
      try {
        this._dg.enqueue(back);
      } catch {
        // the session is gone
      }
    }
    close() {
      this._close({ closeCode: 0, reason: '' });
      try {
        this._dg.close();
      } catch {
        // already closed
      }
    }
  }
  // Remember the room session's sender so the bridge can push a roster.
  const origCreate = FakeWebTransport.prototype.createBidirectionalStream;
  FakeWebTransport.prototype.createBidirectionalStream = async function () {
    const s = await origCreate.call(this);
    if (this.kind === 'room') {
      const reader = s.readable;
      let ctl;
      const tee = new ReadableStream({
        start: (c) => (ctl = c),
      });
      const r = reader.getReader();
      void (async () => {
        for (;;) {
          const { value, done } = await r.read();
          if (done) break;
          ctl.enqueue(value);
        }
      })();
      window.__voiceE2ERoomSend = (msg) => ctl.enqueue(record(msg));
      return { readable: tee, writable: s.writable };
    }
    return s;
  };
  window.WebTransport = FakeWebTransport;
}

export async function waitForHttp(url, ms) {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    try {
      if ((await fetch(url)).ok) return;
    } catch {
      // not up yet
    }
    await sleep(200);
  }
  throw new Error(`${url} did not come up`);
}

// Horizontal overflow of the page, the people panel or anything in the voice
// panel at this width, as a message, or null.
export async function overflowAt(page, width) {
  const r = await page.evaluate(() => {
    const doc = document.documentElement;
    const panel = document.querySelector('[aria-label="People and chat"]');
    const voice = document.querySelector('[data-testid="voice-panel"]');
    const offenders = [...(voice?.querySelectorAll('*') ?? [])]
      .filter((el) => {
        const b = el.getBoundingClientRect();
        return b.width > 0 && (b.right > window.innerWidth + 0.5 || b.left < -0.5);
      })
      .map((el) => `${el.tagName.toLowerCase()}.${el.className}`.slice(0, 80));
    return {
      doc: doc.scrollWidth - doc.clientWidth,
      panel: panel ? panel.scrollWidth - panel.clientWidth : 0,
      offenders: offenders.slice(0, 5),
    };
  });
  if (r.doc > 0 || r.panel > 0 || r.offenders.length > 0) {
    return `horizontal overflow at ${width}px: page +${r.doc}px, panel +${r.panel}px, ${r.offenders.join(', ')}`;
  }
  return null;
}
