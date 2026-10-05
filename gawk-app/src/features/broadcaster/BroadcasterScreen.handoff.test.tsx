// @vitest-environment jsdom
//
// R67 (docs/69): the broadcaster page's desktop handoff. The offer under
// Start, the Opening modal after a click, the remembered "always", the
// automatic launch from a `?room=` link and the `?desktop=1` request. The
// launch itself is a spy: whether a browser hands `gawk://` to the OS is
// HO4's manual pass, not something jsdom can show.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';

const { launches } = vi.hoisted(() => ({ launches: [] as string[] }));

vi.mock('./workerBroadcastSession', () => ({
  createBroadcastSession: async () => {
    throw new Error('test bug: no broadcast expected');
  },
}));
vi.mock('../../lib/desktopLink', async (importActual) => ({
  ...(await importActual<typeof import('../../lib/desktopLink')>()),
  launchDesktopLink: (href: string) => launches.push(href),
}));

import { BroadcasterScreen } from './BroadcasterScreen';
import { useTransportStore } from '../../state/transportStore';

const WINDOWS_UA =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';
const ANDROID_UA =
  'Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36';
const IPAD_UA =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15';

function asDevice(ua: string, touchPoints = 0) {
  vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(ua);
  // jsdom has no maxTouchPoints to spy on.
  Object.defineProperty(navigator, 'maxTouchPoints', { value: touchPoints, configurable: true });
}

// What applyRouteRoom stashes for a `#/broadcast?room=` link.
function linkStash(code: string) {
  sessionStorage.setItem('gawk:room-return', JSON.stringify({ code, source: 'link' }));
}

const offer = () => screen.queryByRole('link', { name: /open in the desktop app/i });
const modal = () => screen.queryByTestId('desktop-handoff-modal');

beforeEach(() => {
  launches.length = 0;
  window.__GAWK_CONFIG__ = { requirePublishSecret: false };
  localStorage.clear();
  sessionStorage.clear();
  useTransportStore.getState().setSessionOverride(null);
  useTransportStore.getState().setRelayLinkNote(null);
  asDevice(WINDOWS_UA);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  Reflect.deleteProperty(navigator, 'maxTouchPoints');
});

describe('the offer (D1, D2, D3)', () => {
  it('sits under Start as a gawk:// link carrying the pending room and the name', () => {
    linkStash('vip-qy346he235');
    localStorage.setItem('gawk:nickname', 'Juho K');
    render(<BroadcasterScreen />);
    expect(offer()?.getAttribute('href')).toBe('gawk://broadcast?room=vip-qy346he235&nick=Juho%20K');
    expect(screen.getByRole('button', { name: /start a stream/i })).toBeTruthy();
  });

  it('prefers a ?nick= link’s name, and carries a non-default relay', () => {
    useTransportStore.getState().setSessionOverride('https://relay.friend.example');
    render(<BroadcasterScreen linkNickname="mumble-name" />);
    expect(offer()?.getAttribute('href')).toBe(
      'gawk://broadcast?nick=mumble-name&relay=https%3A%2F%2Frelay.friend.example',
    );
  });

  it('follows the room chip: dismissing the room drops it from the link', () => {
    linkStash('vip-qy346he235');
    render(<BroadcasterScreen />);
    fireEvent.click(screen.getByRole('button', { name: 'Don’t join the room' }));
    expect(offer()?.getAttribute('href')).toBe('gawk://broadcast');
  });

  it('is not offered on a phone, an iPad, or when the operator turned it off', () => {
    asDevice(ANDROID_UA);
    render(<BroadcasterScreen />);
    expect(offer()).toBeNull();
    cleanup();

    asDevice(IPAD_UA, 5);
    render(<BroadcasterScreen />);
    expect(offer()).toBeNull();
    cleanup();

    asDevice(WINDOWS_UA);
    window.__GAWK_CONFIG__ = { requirePublishSecret: false, desktopHandoff: false };
    render(<BroadcasterScreen />);
    expect(offer()).toBeNull();
  });
});

describe('the Opening modal (D4, D6, G7)', () => {
  it('opens on a click; the browser path underneath is untouched', () => {
    linkStash('vip-qy346he235');
    render(<BroadcasterScreen />);
    fireEvent.click(offer()!);
    const dialog = within(modal()!);
    expect(dialog.getByText('Opening the desktop app…')).toBeTruthy();
    expect(dialog.getByRole('link', { name: 'Get the app' })).toBeTruthy();

    fireEvent.click(dialog.getByRole('button', { name: 'Continue in the browser' }));
    expect(modal()).toBeNull();
    expect((screen.getByRole('button', { name: /start a stream/i }) as HTMLButtonElement).disabled).toBe(false);
    expect(screen.getByTestId('pending-room').textContent).toContain('vip-qy346he235');
    // The click's own navigation is the launch; nothing else fires.
    expect(launches).toEqual([]);
  });

  it('closes on Escape', () => {
    render(<BroadcasterScreen />);
    fireEvent.click(offer()!);
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(modal()).toBeNull();
  });

  it('remembers "always" from its checkbox', () => {
    render(<BroadcasterScreen />);
    fireEvent.click(offer()!);
    fireEvent.click(screen.getByRole('checkbox', { name: 'Always open broadcast links in the desktop app' }));
    expect(localStorage.getItem('gawk:desktop-handoff')).toBe('auto');
    fireEvent.click(screen.getByRole('checkbox', { name: 'Always open broadcast links in the desktop app' }));
    expect(localStorage.getItem('gawk:desktop-handoff')).toBeNull();
  });
});

describe('automatic launch (D5)', () => {
  beforeEach(() => localStorage.setItem('gawk:desktop-handoff', 'auto'));

  it('a ?room= link launches the app once and shows the Opened modal', () => {
    linkStash('vip-qy346he235');
    render(<BroadcasterScreen />);
    expect(launches).toEqual(['gawk://broadcast?room=vip-qy346he235']);
    const dialog = within(modal()!);
    expect(dialog.getByText('Opened the desktop app')).toBeTruthy();
    expect(dialog.getByRole('link', { name: 'Open again' }).getAttribute('href')).toBe(
      'gawk://broadcast?room=vip-qy346he235',
    );
  });

  it('not twice for the same room in this tab, and not on reload', () => {
    linkStash('vip-qy346he235');
    render(<BroadcasterScreen />);
    cleanup();
    // The same link again in this tab.
    linkStash('VIP-qy346he235');
    render(<BroadcasterScreen />);
    cleanup();
    // A reload: the stash was read and cleared.
    render(<BroadcasterScreen />);
    expect(launches).toHaveLength(1);
    expect(modal()).toBeNull();
  });

  it('never for a plain #/broadcast, a room hop, or an older tab’s stash', () => {
    render(<BroadcasterScreen />);
    cleanup();
    sessionStorage.setItem('gawk:room-return', JSON.stringify({ code: 'AB2CD3', nickname: 'x', source: 'room' }));
    render(<BroadcasterScreen />);
    cleanup();
    sessionStorage.setItem('gawk:room-return', JSON.stringify({ code: 'CD2EF3', nickname: 'x' }));
    render(<BroadcasterScreen />);
    expect(launches).toEqual([]);
  });

  it('never with a dropped relay, on a phone, or with the switch off', () => {
    useTransportStore.getState().setRelayLinkNote('This link named a server it couldn’t be understood as');
    linkStash('room-one');
    render(<BroadcasterScreen />);
    cleanup();
    useTransportStore.getState().setRelayLinkNote(null);

    asDevice(ANDROID_UA);
    linkStash('room-two');
    render(<BroadcasterScreen />);
    cleanup();

    asDevice(WINDOWS_UA);
    window.__GAWK_CONFIG__ = { requirePublishSecret: false, desktopHandoff: false };
    linkStash('room-three');
    render(<BroadcasterScreen />);
    expect(launches).toEqual([]);
  });

  it('"Stop doing this" forgets the choice', () => {
    linkStash('vip-qy346he235');
    render(<BroadcasterScreen />);
    fireEvent.click(within(modal()!).getByRole('button', { name: 'Stop doing this' }));
    expect(modal()).toBeNull();
    expect(localStorage.getItem('gawk:desktop-handoff')).toBeNull();
  });

  it('a room hop that already launched shows the modal without launching again', () => {
    sessionStorage.setItem(
      'gawk:room-return',
      JSON.stringify({ code: 'AB2CD3', nickname: 'tuhis', source: 'room', handoff: 'auto' }),
    );
    render(<BroadcasterScreen />);
    expect(within(modal()!).getByText('Opened the desktop app')).toBeTruthy();
    expect(launches).toEqual([]);
  });
});

describe('?desktop=1 (D9)', () => {
  it('launches once and shows the Opening modal, storing nothing', () => {
    linkStash('vip-qy346he235');
    render(<BroadcasterScreen linkDesktop />);
    expect(launches).toEqual(['gawk://broadcast?room=vip-qy346he235']);
    expect(within(modal()!).getByText('Opening the desktop app…')).toBeTruthy();
    expect(localStorage.getItem('gawk:desktop-handoff')).toBeNull();
  });

  it('works without a room, and wins over a stored "always"', () => {
    localStorage.setItem('gawk:desktop-handoff', 'auto');
    render(<BroadcasterScreen linkDesktop />);
    expect(launches).toEqual(['gawk://broadcast']);
    expect(within(modal()!).getByText('Opening the desktop app…')).toBeTruthy();
    expect(
      (screen.getByRole('checkbox', { name: 'Always open broadcast links in the desktop app' }) as HTMLInputElement).checked,
    ).toBe(true);
  });

  it('is ignored where the offer is not shown, or with a dropped relay', () => {
    asDevice(ANDROID_UA);
    render(<BroadcasterScreen linkDesktop />);
    cleanup();
    asDevice(WINDOWS_UA);
    useTransportStore.getState().setRelayLinkNote('This deployment only allows its own server.');
    render(<BroadcasterScreen linkDesktop />);
    expect(launches).toEqual([]);
    expect(modal()).toBeNull();
  });

  it('a room hop that clicked "…or in the desktop app" opens the Opening modal without launching again', () => {
    sessionStorage.setItem(
      'gawk:room-return',
      JSON.stringify({ code: 'AB2CD3', nickname: 'tuhis', source: 'room', handoff: 'opening' }),
    );
    render(<BroadcasterScreen />);
    expect(within(modal()!).getByText('Opening the desktop app…')).toBeTruthy();
    expect(launches).toEqual([]);
  });
});
