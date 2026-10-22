// @vitest-environment jsdom
//
// docs/76 §7.3: the in-session chip on a PoP with its opt-out, the prompt
// toast, the denied note, and an opted-out PoP offered back. All of it renders
// through the session screens' ServerIndicator.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';

import { ServerIndicator } from '../servers/ServerIndicator';
import { useTransportStore } from '../../state/transportStore';
import { usePopStore } from './popStore';
import { OPT_OUT_KEY, resetSwitchForTest } from './switch';

beforeEach(() => {
  localStorage.clear();
  resetSwitchForTest();
  useTransportStore.getState().clearOfferOverride();
});
afterEach(() => {
  cleanup();
  useTransportStore.getState().clearOfferOverride();
  resetSwitchForTest();
});

describe('the local relay chip', () => {
  it('names the PoP on a switched session, and "Use … instead" goes home and remembers it', () => {
    act(() => {
      useTransportStore.getState().setOfferOverride('https://192.168.1.20:4433', [], 'pop-uid-1', 'Assembly 2026');
      usePopStore.setState({ phase: 'switched' });
    });
    render(<ServerIndicator />);
    const chip = screen.getByTestId('local-relay-chip');
    expect(chip.textContent).toContain('Assembly 2026 · local relay');
    fireEvent.click(screen.getByRole('button', { name: /Assembly 2026 · local relay/ }));
    fireEvent.click(screen.getByRole('menuitem', { name: `Use ${window.location.host} instead` }));
    expect(useTransportStore.getState().resolvedSource).not.toBe('pop');
    expect(JSON.parse(localStorage.getItem(OPT_OUT_KEY) ?? '[]')).toEqual(['pop-uid-1']);
  });

  it('strips control and bidi characters from the offered name', () => {
    act(() => {
      useTransportStore.getState().setOfferOverride('https://192.168.1.20:4433', [], 'h', 'Party‮ evil');
    });
    render(<ServerIndicator />);
    expect(screen.getByTestId('local-relay-chip').textContent).not.toContain('‮');
  });

  it('shows the prompt toast while the browser asks', () => {
    act(() => usePopStore.setState({ toast: 'Assembly 2026 has a local relay. Your browser will ask…' }));
    render(<ServerIndicator />);
    expect(screen.getByRole('status').textContent).toContain('Your browser will ask');
  });

  it('shows the denied note on home', () => {
    act(() => usePopStore.setState({ phase: 'denied', note: 'Your browser blocked the local relay.' }));
    render(<ServerIndicator />);
    expect(screen.getByText('Your browser blocked the local relay.')).toBeTruthy();
  });

  it('offers an opted-out PoP back', () => {
    act(() => usePopStore.setState({ declined: { handle: 'pop-uid-1', name: 'Assembly 2026' } }));
    render(<ServerIndicator />);
    expect(screen.getByText(/Assembly 2026 has a local relay\./)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Use it' })).toBeTruthy();
  });

  it('renders nothing on the default server with nothing to say', () => {
    const { container } = render(<ServerIndicator />);
    expect(container.textContent).toBe('');
  });
});
