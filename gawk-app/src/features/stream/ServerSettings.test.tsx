// @vitest-environment jsdom
//
// The debug pages' Server URL field. The store normalizes every value it is
// handed (and falls back to the default for an invalid one), so the field
// must hold what is being typed and hand the store only the finished value.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';

import { ServerSettings } from './ServerSettings';
import { useTransportStore } from '../../state/transportStore';

const DEFAULT_URL = 'https://localhost:4433';

function urlInput(): HTMLInputElement {
  return screen.getByLabelText('Server URL') as HTMLInputElement;
}

function manualEntry() {
  return useTransportStore.getState().servers.find((e) => e.label === 'Manual (dev)');
}

beforeEach(() => {
  localStorage.clear();
  window.__GAWK_CONFIG__ = {};
  const s = useTransportStore.getState();
  s.setSessionOverride(null);
  s.reloadFromStorage();
  s.selectServer('default');
});

afterEach(() => {
  cleanup();
  delete window.__GAWK_CONFIG__;
  localStorage.clear();
});

describe('ServerSettings server URL', () => {
  it('keeps a partial URL while typing instead of snapping it to a normalized one', () => {
    render(<ServerSettings disabled={false} />);
    expect(urlInput().value).toBe(DEFAULT_URL);

    fireEvent.change(urlInput(), { target: { value: 'h' } });
    expect(urlInput().value).toBe('h');

    // Mid-edit of the port: one digit deleted must not drop the port.
    fireEvent.change(urlInput(), { target: { value: 'https://localhost:443' } });
    expect(urlInput().value).toBe('https://localhost:443');

    fireEvent.change(urlInput(), { target: { value: 'https://relay.test:4433/' } });
    expect(urlInput().value).toBe('https://relay.test:4433/');

    // Nothing reached the store (or its persisted entry) while typing.
    expect(useTransportStore.getState().serverUrl).toBe(DEFAULT_URL);
    expect(manualEntry()).toBeUndefined();
  });

  it('commits the typed URL on blur', () => {
    render(<ServerSettings disabled={false} />);
    fireEvent.change(urlInput(), { target: { value: 'https://relay.test:4433/' } });
    fireEvent.blur(urlInput());

    expect(useTransportStore.getState().serverUrl).toBe('https://relay.test:4433');
    expect(manualEntry()?.url).toBe('https://relay.test:4433');
    expect(urlInput().value).toBe('https://relay.test:4433');
  });

  it('commits the typed URL on Enter', () => {
    render(<ServerSettings disabled={false} />);
    fireEvent.change(urlInput(), { target: { value: 'https://relay.test:4433' } });
    fireEvent.keyDown(urlInput(), { key: 'Enter' });

    expect(useTransportStore.getState().serverUrl).toBe('https://relay.test:4433');
  });

  it('follows a change made elsewhere while the field is not being edited', () => {
    render(<ServerSettings disabled={false} />);
    act(() => {
      useTransportStore.getState().setServerUrl('https://other.test:4433');
    });
    expect(urlInput().value).toBe('https://other.test:4433');
  });
});
