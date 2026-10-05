// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest';
import { parseRoute } from '../../routing';
import { applyRouteRoom, stashRoomReturn, takeRoomReturn } from './roomReturn';

describe('roomReturn', () => {
  beforeEach(() => sessionStorage.clear());

  it('round-trips the code and nickname, and reads once', () => {
    stashRoomReturn({ code: 'AB2CD3', nickname: 'tuhis', source: 'room' });
    expect(takeRoomReturn()).toEqual({ code: 'AB2CD3', nickname: 'tuhis', source: 'room' });
    expect(takeRoomReturn()).toBeNull();
  });

  it('a guest stays a guest', () => {
    stashRoomReturn({ code: 'devroom', nickname: null, source: 'room' });
    expect(takeRoomReturn()).toEqual({ code: 'devroom', nickname: null, source: 'room' });
  });

  it('a ?room= link is the hop with no nickname asked yet', () => {
    stashRoomReturn({ code: 'vip-abc', nickname: undefined, source: 'link' });
    expect(takeRoomReturn()).toEqual({ code: 'vip-abc', nickname: undefined, source: 'link' });
  });

  it('applyRouteRoom stashes a broadcast link’s room and strips it from the URL', () => {
    window.history.replaceState(null, '', '/#/broadcast?room=vip-abc&relay=https%3A%2F%2Fx.example');
    applyRouteRoom(parseRoute(window.location.hash));
    expect(window.location.hash).toBe('#/broadcast?relay=https%3A%2F%2Fx.example');
    expect(takeRoomReturn()).toEqual({ code: 'vip-abc', nickname: undefined, source: 'link' });

    // Nothing to do without one, or on another route.
    for (const hash of ['#/broadcast', '#/broadcast?room=no%20spaces', '#/room/vip-abc?room=other']) {
      window.history.replaceState(null, '', `/${hash}`);
      applyRouteRoom(parseRoute(window.location.hash));
      expect(window.location.hash).toBe(hash);
      expect(takeRoomReturn()).toBeNull();
    }
  });

  it('carries where the hop came from and a launch already made (R67 D5)', () => {
    stashRoomReturn({ code: 'AB2CD3', nickname: 'tuhis', source: 'room', handoff: 'opening' });
    expect(takeRoomReturn()).toEqual({ code: 'AB2CD3', nickname: 'tuhis', source: 'room', handoff: 'opening' });
    stashRoomReturn({ code: 'AB2CD3', nickname: null, source: 'room', handoff: 'auto' });
    expect(takeRoomReturn()).toMatchObject({ handoff: 'auto' });
    // An older tab's stash has no source: it never counts as a link from
    // outside, so it can never launch the app by itself.
    sessionStorage.setItem('gawk:room-return', JSON.stringify({ code: 'AB2CD3', nickname: 'x' }));
    expect(takeRoomReturn()).toEqual({ code: 'AB2CD3', nickname: 'x', source: 'room' });
    sessionStorage.setItem('gawk:room-return', JSON.stringify({ code: 'AB2CD3', source: 'elsewhere', handoff: 'now' }));
    expect(takeRoomReturn()).toEqual({ code: 'AB2CD3', nickname: undefined, source: 'room' });
  });

  it('tolerates junk and the pre-revision bare-code shape', () => {
    sessionStorage.setItem('gawk:room-return', 'AB2CD3');
    expect(takeRoomReturn()).toBeNull();
    sessionStorage.setItem('gawk:room-return', '{"nickname":"x"}');
    expect(takeRoomReturn()).toBeNull();
  });
});
