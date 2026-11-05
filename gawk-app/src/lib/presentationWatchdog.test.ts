// The tier-2 stall watchdog (docs/27 "Stall watchdog"), driven against a fake
// media element: an EventTarget with settable playhead/buffered/readyState and
// a play() that records calls. Real elements are useless here — jsdom has no
// media pipeline, and WebKit's pause-and-fire-`ended` on underrun is exactly
// the behavior Chrome (the only real browser in CI) does not have.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  PresentationWatchdog,
  CUSHION_GRACE_MS,
  RECOVERY_MIN_INTERVAL_MS,
  RECOVERY_RETRY_MS,
  type WatchdogMedia,
} from './presentationWatchdog';

const HAVE_CURRENT_DATA = 2;
const HAVE_ENOUGH_DATA = 4;

class FakeMedia extends EventTarget implements WatchdogMedia {
  ranges: Array<[number, number]> = [];
  private ct = 0;
  seeks: number[] = [];
  paused = false;
  ended = false;
  seeking = false;
  readyState = HAVE_ENOUGH_DATA;
  playCalls = 0;
  playRejects = false;

  get currentTime() {
    return this.ct;
  }
  set currentTime(v: number) {
    this.ct = v;
    this.seeks.push(v);
    this.ended = false;
  }
  // Positions the playhead without recording a seek (what playback does).
  at(t: number) {
    this.ct = t;
    return this;
  }
  get buffered() {
    const r = this.ranges;
    return { length: r.length, start: (i: number) => r[i][0], end: (i: number) => r[i][1] };
  }
  play() {
    this.playCalls++;
    if (this.playRejects) return Promise.reject(new DOMException('nope', 'NotAllowedError'));
    this.paused = false;
    return Promise.resolve();
  }
  fire(type: string) {
    this.dispatchEvent(new Event(type));
  }
  // WebKit's underrun: playhead at the buffered end, starved, paused, `ended`.
  underrun() {
    this.readyState = HAVE_CURRENT_DATA;
    this.paused = true;
    this.ended = true;
    this.fire('pause');
    this.fire('ended');
  }
}

let media: FakeMedia;
let dog: PresentationWatchdog;

beforeEach(() => {
  vi.useFakeTimers();
  media = new FakeMedia();
  media.ranges = [[0, 10]];
  media.at(9.9);
  dog = new PresentationWatchdog({ rejoinS: 0.1, now: () => Date.now() });
  dog.attach(media);
});

afterEach(() => {
  dog.detach();
  vi.useRealTimers();
});

describe('PresentationWatchdog — recovery', () => {
  it('re-plays an underrun (pause + ended) once data has arrived ahead of the playhead', async () => {
    dog.start();
    media.at(10).underrun();
    // Nothing ahead yet: waiting is correct, replaying would just re-stall.
    expect(media.playCalls).toBe(0);

    media.ranges = [[0, 11]];
    media.readyState = HAVE_ENOUGH_DATA;
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS);

    expect(media.playCalls).toBe(1);
    // `ended` was set: play() on an ended element restarts from the beginning,
    // so the recovery must seek first — into the newest range.
    expect(media.seeks).toHaveLength(1);
    expect(media.seeks[0]).toBeGreaterThanOrEqual(10);
    expect(media.seeks[0]).toBeLessThan(11);
    expect(dog.getStats()).toMatchObject({ recoveries: 1, seeks: 1, lastCause: 'ended' });
  });

  it('seeks over a hole into the newest buffered range and plays', async () => {
    dog.start();
    media.ranges = [
      [0, 10],
      [10.5, 14],
    ];
    media.at(10);
    media.readyState = HAVE_CURRENT_DATA;
    media.fire('waiting');
    await vi.advanceTimersByTimeAsync(0);

    expect(media.seeks).toEqual([14 - 0.1]);
    expect(dog.getStats()).toMatchObject({ recoveries: 1, seeks: 1, lastCause: 'waiting' });
  });

  it('lands recovery seeks at a rejoin offset changed mid-session (delivery-mode switch)', async () => {
    dog.setRejoinS(1.0);
    dog.start();
    media.ranges = [
      [0, 10],
      [10.5, 14],
    ];
    media.at(10);
    media.readyState = HAVE_CURRENT_DATA;
    media.fire('waiting');
    await vi.advanceTimersByTimeAsync(0);

    expect(media.seeks).toEqual([14 - 1.0]);
  });

  it('lands no earlier than the newest range start when the range is shorter than the rejoin offset', async () => {
    dog.start();
    media.ranges = [
      [0, 10],
      [12, 12.05],
    ];
    media.at(10);
    media.readyState = HAVE_CURRENT_DATA;
    media.fire('stalled');
    await vi.advanceTimersByTimeAsync(0);
    expect(media.seeks).toEqual([12]);
  });

  it('nudges a playhead stuck inside buffered data while playing', async () => {
    dog.start();
    media.ranges = [[0, 12]];
    media.at(10);
    media.readyState = HAVE_CURRENT_DATA;
    media.fire('waiting');
    await vi.advanceTimersByTimeAsync(0);
    // `waiting` with data ahead is not yet a stall — the element may simply be
    // decoding. Only a playhead that has not moved after a retry is stuck.
    expect(media.seeks).toEqual([]);
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS);
    expect(media.seeks).toHaveLength(1);
    expect(media.seeks[0]).toBeGreaterThan(10);
    expect(media.seeks[0]).toBeLessThan(12);
    expect(dog.getStats().recoveries).toBe(1);
  });

  it('stands down once the element reports `playing`', async () => {
    dog.start();
    media.at(10);
    media.readyState = HAVE_CURRENT_DATA;
    media.fire('waiting');
    media.ranges = [[0, 11]];
    media.readyState = HAVE_ENOUGH_DATA;
    media.fire('playing');
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS * 4);
    expect(media.seeks).toEqual([]);
    expect(media.playCalls).toBe(0);
    expect(dog.getStats().recoveries).toBe(0);
  });

  it('counts a rejected play() without throwing', async () => {
    dog.start();
    media.playRejects = true;
    media.ranges = [[0, 11]];
    media.at(10).underrun();
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS);
    expect(media.playCalls).toBe(1);
    expect(dog.getStats().playErrors).toBe(1);
  });
});

// The per-mode live-edge cushion (liveEdge.ts): after an underrun at the live
// edge, appends arrive at 1x, so whatever lies ahead when play() resumes is the
// cushion until the next seek. Rebuild it by waiting, never by seeking back
// over frames already shown.
describe('PresentationWatchdog — rebuilds the cushion after an underrun', () => {
  it('waits for the rejoin offset ahead, then resumes without rewinding (ended)', async () => {
    dog.setRejoinS(1.0);
    dog.start();
    media.ranges = [[0, 10.2]];
    media.at(10).underrun();
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS * 2);
    // 0.2 s ahead: enough to play, not enough to hold a 1 s cushion.
    expect(media.playCalls).toBe(0);

    media.ranges = [[0, 11.1]];
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS);
    expect(media.playCalls).toBe(1);
    // `ended` still needs a seek before play(), but never behind the playhead.
    expect(media.seeks).toHaveLength(1);
    expect(media.seeks[0]).toBeGreaterThanOrEqual(10);
    expect(11.1 - media.seeks[0]).toBeGreaterThanOrEqual(1.0 - 1e-9);
  });

  it('waits for the rejoin offset after a starvation pause without `ended`', async () => {
    dog.setRejoinS(1.0);
    dog.start();
    media.ranges = [[0, 10.3]];
    media.at(10);
    media.readyState = HAVE_CURRENT_DATA;
    media.paused = true;
    media.fire('pause');
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS * 2);
    expect(media.playCalls).toBe(0);

    media.ranges = [[0, 11.05]];
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS);
    expect(media.playCalls).toBe(1);
    expect(media.seeks).toEqual([]);
  });

  it('starts the grace clock at the pause, not at an earlier `waiting`', async () => {
    dog.setRejoinS(1.0);
    dog.start();
    // WebKit says `waiting` at the live edge while still playing, with nothing
    // ahead: the watchdog only polls for data. The underrun pause comes later.
    media.ranges = [[0, 10.3]];
    media.at(10.3);
    media.fire('waiting');
    await vi.advanceTimersByTimeAsync(3000);
    media.ranges = [[0, 10.5]];
    media.underrun();
    await vi.advanceTimersByTimeAsync(1100);
    // 0.2 s ahead of a 1 s cushion, 1.1 s into the pause: the wait must still
    // be running (measured from the `waiting`, it would already have given up).
    expect(media.playCalls).toBe(0);
  });

  it('gives up waiting for the full cushion after the grace period', async () => {
    dog.setRejoinS(1.0);
    dog.start();
    media.ranges = [[0, 10.3]];
    media.at(10).underrun();
    // The buffer never grows (e.g. MMS stopped asking for data while paused).
    await vi.advanceTimersByTimeAsync(1000 + CUSHION_GRACE_MS - RECOVERY_RETRY_MS);
    expect(media.playCalls).toBe(0);
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS * 2);
    expect(media.playCalls).toBe(1);
  });
});

describe('PresentationWatchdog — never fights the user or the armed state', () => {
  it('does nothing while not started (armed-but-paused, docs/27 Decision 5)', async () => {
    media.ranges = [
      [0, 10],
      [11, 20],
    ];
    media.at(10).underrun();
    media.fire('waiting');
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS * 10);
    expect(media.playCalls).toBe(0);
    expect(media.seeks).toEqual([]);
    expect(dog.getStats().recoveries).toBe(0);
  });

  it('treats a pause with healthy data ahead as deliberate and leaves it paused', async () => {
    dog.start();
    media.ranges = [[0, 12]];
    media.at(10);
    media.paused = true;
    media.fire('pause');
    // Later stall-ish noise while the user holds pause must not resume it.
    media.fire('stalled');
    media.ranges = [
      [0, 12],
      [13, 20],
    ];
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS * 10);
    expect(media.playCalls).toBe(0);
    expect(media.seeks).toEqual([]);
  });

  it('resumes guarding once the user plays again', async () => {
    dog.start();
    media.ranges = [[0, 12]];
    media.at(10);
    media.paused = true;
    media.fire('pause');
    media.paused = false;
    media.fire('play');
    media.fire('playing');
    media.ranges = [[0, 14]];
    media.at(12).underrun();
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS);
    expect(media.playCalls).toBe(1);
  });

  it('treats a pause right after `waiting` as starvation even if data has since arrived', async () => {
    dog.start();
    media.ranges = [[0, 10]];
    media.at(10);
    media.readyState = HAVE_CURRENT_DATA;
    media.fire('waiting');
    // An append lands between WebKit's underrun and the pause event's dispatch.
    media.ranges = [[0, 11]];
    media.readyState = HAVE_ENOUGH_DATA;
    media.paused = true;
    media.fire('pause');
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS);
    expect(media.playCalls).toBe(1);
  });

  it('stop() cancels a pending retry, and events after stop() are ignored', async () => {
    dog.start();
    media.at(10).underrun();
    dog.stop();
    media.ranges = [[0, 12]];
    media.readyState = HAVE_ENOUGH_DATA;
    media.fire('waiting');
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS * 10);
    expect(media.playCalls).toBe(0);
  });

  it('a fresh start() forgets an earlier deliberate pause', async () => {
    dog.start();
    media.ranges = [[0, 12]];
    media.at(10);
    media.paused = true;
    media.fire('pause');
    dog.stop();
    media.paused = false;
    dog.start();
    media.at(12).underrun();
    media.ranges = [[0, 13]];
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS);
    expect(media.playCalls).toBe(1);
  });
});

describe('PresentationWatchdog — rate limit', () => {
  it('spaces recoveries at least RECOVERY_MIN_INTERVAL_MS apart, however often the element stalls', async () => {
    dog.start();
    // A pathological element: every recovery immediately re-stalls behind a
    // newer range, forever.
    let end = 20;
    media.play = () => {
      media.playCalls++;
      media.paused = false;
      end += 1;
      media.ranges = [
        [0, media.currentTime],
        [media.currentTime + 0.5, end],
      ];
      queueMicrotask(() => media.underrun());
      return Promise.resolve();
    };
    media.ranges = [
      [0, 10],
      [10.5, end],
    ];
    media.at(10).underrun();
    await vi.advanceTimersByTimeAsync(RECOVERY_MIN_INTERVAL_MS * 5);
    // One immediately, then at most one per interval.
    expect(dog.getStats().recoveries).toBeGreaterThanOrEqual(2);
    expect(dog.getStats().recoveries).toBeLessThanOrEqual(6);
  });

  it('counters survive stop() — they describe the whole session', async () => {
    dog.start();
    media.ranges = [[0, 11]];
    media.at(10).underrun();
    await vi.advanceTimersByTimeAsync(RECOVERY_RETRY_MS);
    dog.stop();
    expect(dog.getStats().recoveries).toBe(1);
  });
});
