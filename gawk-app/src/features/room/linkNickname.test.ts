// @vitest-environment jsdom
import { describe, expect, it } from 'vitest';
import { parseRoute } from '../../routing';
import { applyRouteNick } from './linkNickname';

describe('applyRouteNick', () => {
  it('strips a room or broadcast link’s ?nick= from the URL, keeping the rest', () => {
    window.history.replaceState(null, '', '/#/room/AB2CD3?nick=tuhis&relay=https%3A%2F%2Fx.example');
    applyRouteNick(parseRoute(window.location.hash));
    expect(window.location.hash).toBe('#/room/AB2CD3?relay=https%3A%2F%2Fx.example');

    window.history.replaceState(null, '', '/#/broadcast?nick=tuhis');
    applyRouteNick(parseRoute(window.location.hash));
    expect(window.location.hash).toBe('#/broadcast');
  });

  it('leaves other routes alone', () => {
    for (const hash of ['#/room/AB2CD3', '#/view/AB2CD3?nick=x']) {
      window.history.replaceState(null, '', `/${hash}`);
      applyRouteNick(parseRoute(window.location.hash));
      expect(window.location.hash).toBe(hash);
    }
  });
});
