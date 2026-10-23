// @vitest-environment jsdom
//
// docs/76 §7.3: a broadcaster on a PoP reads "Live on Assembly 2026's local
// relay" and "On the party wall · Hide"; Hide and Show go through the wall
// command and are remembered for the next /publish.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';

import { useTransportStore } from '../../state/transportStore';
import { usePopStore } from './popStore';
import { resetSwitchForTest } from './switch';
import { PartyWallLine } from './PartyWallLine';
import { isWallHidden, type WallCommand } from './wall';

const INFO = { linked: true, onWall: true, name: 'Assembly 2026', homeOrigin: 'https://gawk.ioio.fi', wallCode: 'assembly-2026' };

function onPop(info = INFO) {
  act(() => usePopStore.setState({ info: { relayUrl: useTransportStore.getState().serverUrl, info } }));
}

beforeEach(() => {
  localStorage.clear();
  resetSwitchForTest();
});
afterEach(() => {
  cleanup();
  resetSwitchForTest();
});

describe('PartyWallLine', () => {
  it('renders nothing off a PoP', () => {
    const { container } = render(<PartyWallLine broadcastId="K7XQ2M" resumeTokenHex={'ab'.repeat(16)} label="" />);
    expect(container.textContent).toBe('');
  });

  it('says where the broadcast is live and that it is on the wall; Hide hides and is remembered', async () => {
    onPop();
    const send = vi.fn(async (_cmd: WallCommand) => true);
    render(<PartyWallLine broadcastId="K7XQ2M" resumeTokenHex={'ab'.repeat(16)} label="tuhis" send={send} />);
    expect(screen.getByText('Live on Assembly 2026’s local relay')).toBeTruthy();
    expect(screen.getByText(/On the party wall/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Hide' }));
    await waitFor(() => expect(screen.getByText(/Hidden from the party wall/)).toBeTruthy());
    expect(send).toHaveBeenCalledWith(expect.objectContaining({ hide: true, wallCode: 'assembly-2026', broadcastId: 'K7XQ2M' }));
    expect(isWallHidden('assembly-2026')).toBe(true);

    fireEvent.click(screen.getByRole('button', { name: 'Show' }));
    await waitFor(() => expect(screen.getByText(/On the party wall/)).toBeTruthy());
    expect(send).toHaveBeenLastCalledWith(expect.objectContaining({ hide: false, label: 'tuhis' }));
    expect(isWallHidden('assembly-2026')).toBe(false);
  });

  // The PoP sends the publish session's 0x18 again whenever its wall state
  // changes, so the newest one is the truth, and it says what the PoP did.
  it('a fresh LocalRelayInfo replaces this tab’s own Hide', async () => {
    onPop();
    const send = vi.fn(async (_cmd: WallCommand) => true);
    render(<PartyWallLine broadcastId="K7XQ2M" resumeTokenHex={'ab'.repeat(16)} label="" send={send} />);
    fireEvent.click(screen.getByRole('button', { name: 'Hide' }));
    await waitFor(() => expect(screen.getByText(/Hidden from the party wall/)).toBeTruthy());
    onPop({ ...INFO });
    expect(screen.getByText(/On the party wall/)).toBeTruthy();
    onPop({ ...INFO, onWall: false });
    expect(screen.getByText(/Hidden from the party wall/)).toBeTruthy();
  });

  it('a refused Hide leaves the state as it was', async () => {
    onPop();
    const send = vi.fn(async () => false);
    render(<PartyWallLine broadcastId="K7XQ2M" resumeTokenHex={'ab'.repeat(16)} label="" send={send} />);
    fireEvent.click(screen.getByRole('button', { name: 'Hide' }));
    await waitFor(() => expect(send).toHaveBeenCalled());
    expect(screen.getByText(/On the party wall/)).toBeTruthy();
    expect(isWallHidden('assembly-2026')).toBe(false);
  });

  it('a PoP without a wall shows only where the broadcast is live', () => {
    onPop({ ...INFO, onWall: false, wallCode: '' });
    render(<PartyWallLine broadcastId="K7XQ2M" resumeTokenHex={null} label="" />);
    expect(screen.getByText('Live on Assembly 2026’s local relay')).toBeTruthy();
    expect(screen.queryByText(/party wall/)).toBeNull();
  });
});
