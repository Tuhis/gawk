// @vitest-environment jsdom
//
// docs/88 BR3, §7: the broadcaster's Delivery control under Stream quality —
// the native Quality page's label, choices and hints word for word, the hint
// under the selected choice, persisted, and applied live through onChange
// (the pipeline takes it at the next keyframe, no restart).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';

import { DeliveryPicker } from './DeliveryPicker';
import { useBroadcastSettingsStore } from '../../state/broadcastSettingsStore';
import { DELIVERIES, DELIVERY_HINTS, DELIVERY_LABEL, DELIVERY_LABELS } from '../../transport/carrier-sender';

const RELIABLE_HINT = 'If your connection drops some video, viewers wait a moment for it instead of losing the picture.';
const LOWEST_HINT = 'Viewers never wait for dropped video. If your connection drops some, they see brief freezes.';

beforeEach(() => {
  localStorage.clear();
  useBroadcastSettingsStore.setState({ uplinkDelivery: null });
});

afterEach(cleanup);

describe('DeliveryPicker', () => {
  it('reads "Delivery" with Reliable · Lowest latency, Lowest latency selected by default, its hint shown', () => {
    render(<DeliveryPicker />);
    const group = screen.getByRole('radiogroup', { name: 'Delivery' });
    expect(group).toBeTruthy();
    const radios = screen.getAllByRole('radio');
    expect(radios.map((r) => r.textContent)).toEqual(['Reliable', 'Lowest latency']);
    expect(screen.getByRole('radio', { name: 'Lowest latency' }).getAttribute('aria-checked')).toBe('true');
    expect(screen.getByRole('radio', { name: 'Reliable' }).getAttribute('aria-checked')).toBe('false');
    expect(screen.getByText(LOWEST_HINT)).toBeTruthy();
    expect(screen.queryByText(RELIABLE_HINT)).toBeNull();
  });

  it('picking Reliable persists it, shows its hint and tells the live pipeline', () => {
    const onChange = vi.fn();
    render(<DeliveryPicker onChange={onChange} />);
    fireEvent.click(screen.getByRole('radio', { name: 'Reliable' }));
    expect(onChange).toHaveBeenCalledWith('reliable');
    expect(localStorage.getItem('gawk.uplinkDelivery')).toBe('reliable');
    expect(screen.getByRole('radio', { name: 'Reliable' }).getAttribute('aria-checked')).toBe('true');
    expect(screen.getByText(RELIABLE_HINT)).toBeTruthy();
  });

  it('the selected hint describes the group for screen readers', () => {
    render(<DeliveryPicker />);
    const group = screen.getByRole('radiogroup', { name: 'Delivery' });
    const id = group.getAttribute('aria-describedby');
    expect(id && document.getElementById(id)?.textContent).toBe(LOWEST_HINT);
  });
});

// docs/57 principle 3 (docs/88 BR3): no network jargon in a main-flow string.
// The native app's test and list (crates/engine/src/lossnotice.rs
// `main_flow_strings_carry_no_jargon`, which covers its Delivery control),
// plus "datagram", over the browser's Delivery control. Diagnostics are
// exempt. The upload notice (RU9's copy, unchanged here) keeps its own test.
describe('main-flow strings carry no jargon (principle 3)', () => {
  const BANNED = ['awdl', 'channel', 'packet', 'uplink', 'quic', 'carrier', 'dscp', 'ghz', 'band', 'datagram'];
  it.each([DELIVERY_LABEL, ...DELIVERIES.map((d) => DELIVERY_LABELS[d]), ...DELIVERIES.map((d) => DELIVERY_HINTS[d])])(
    '%s',
    (text) => {
      for (const b of BANNED) expect(text.toLowerCase(), `says ${b}`).not.toContain(b);
    },
  );
});
