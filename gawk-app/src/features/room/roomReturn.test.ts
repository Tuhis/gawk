// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest';
import { parseRoute } from '../../routing';
import { applyRouteRoom, stashRoomReturn, takeRoomReturn } from './roomReturn';

describe('roomReturn', () => {
  beforeEach(() => sessionStorage.clear());

  it('round-trips the code and nickname, and reads once', () => {
    stashRoomReturn({ code: 'AB2CD3', nickname: 'tuhis' });
    expect(takeRoomReturn()).toEqual({ code: 'AB2CD3', nickname: 'tuhis' });
    expect(takeRoomReturn()).toBeNull();
  });

  it('a guest stays a guest', () => {
    stashRoomReturn({ code: 'devroom', nickname: null });
    expect(takeRoomReturn()).toEqual({ code: 'devroom', nickname: null });
  });

  it('a ?room= link is the hop with no nickname asked yet', () => {
    stashRoomReturn({ code: 'vip-abc', nickname: undefined });
    expect(takeRoomReturn()).toEqual({ code: 'vip-abc', nickname: undefined });
  });

  it('applyRouteRoom stashes a broadcast link’s room and strips it from the URL', () => {
    window.history.replaceState(null, '', '/#/broadcast?room=vip-abc&relay=https%3A%2F%2Fx.example');
    applyRouteRoom(parseRoute(window.location.hash));
    expect(window.location.hash).toBe('#/broadcast?relay=https%3A%2F%2Fx.example');
    expect(takeRoomReturn()).toEqual({ code: 'vip-abc', nickname: undefined });

    // Nothing to do without one, or on another route.
    for (const hash of ['#/broadcast', '#/broadcast?room=no%20spaces', '#/room/vip-abc?room=other']) {
      window.history.replaceState(null, '', `/${hash}`);
      applyRouteRoom(parseRoute(window.location.hash));
      expect(window.location.hash).toBe(hash);
      expect(takeRoomReturn()).toBeNull();
    }
  });

  it('tolerates junk and the pre-revision bare-code shape', () => {
    sessionStorage.setItem('gawk:room-return', 'AB2CD3');
    expect(takeRoomReturn()).toBeNull();
    sessionStorage.setItem('gawk:room-return', '{"nickname":"x"}');
    expect(takeRoomReturn()).toBeNull();
  });
});
