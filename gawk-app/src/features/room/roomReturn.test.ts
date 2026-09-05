// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest';
import { stashRoomReturn, takeRoomReturn } from './roomReturn';

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

  it('tolerates junk and the pre-revision bare-code shape', () => {
    sessionStorage.setItem('gawk:room-return', 'AB2CD3');
    expect(takeRoomReturn()).toBeNull();
    sessionStorage.setItem('gawk:room-return', '{"nickname":"x"}');
    expect(takeRoomReturn()).toBeNull();
  });
});
