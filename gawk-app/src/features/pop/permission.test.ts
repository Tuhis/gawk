// docs/76 item 43.4: the browser's local-network permission, under either of
// its two names (M2), and what a browser that knows neither reports.
import { describe, expect, it } from 'vitest';

import { queryLocalNetworkPermission, type PermissionsLike } from './permission';

function perms(known: Record<string, PermissionState>): PermissionsLike & { asked: string[] } {
  const asked: string[] = [];
  return {
    asked,
    query: async ({ name }: { name: string }) => {
      asked.push(name);
      if (!(name in known)) throw new TypeError(`'${name}' is not a valid permission name`);
      return { state: known[name] } as PermissionStatus;
    },
  };
}

describe('queryLocalNetworkPermission', () => {
  for (const state of ['granted', 'prompt', 'denied'] as const) {
    it(`reports ${state} from local-network`, async () => {
      const p = perms({ 'local-network': state });
      expect(await queryLocalNetworkPermission(p)).toBe(state);
      expect(p.asked).toEqual(['local-network']);
    });
  }

  it('falls back to local-network-access when the first name is unknown', async () => {
    const p = perms({ 'local-network-access': 'denied' });
    expect(await queryLocalNetworkPermission(p)).toBe('denied');
    expect(p.asked).toEqual(['local-network', 'local-network-access']);
  });

  it('is unknown when the browser knows neither name (just probe)', async () => {
    expect(await queryLocalNetworkPermission(perms({}))).toBe('unknown');
  });

  it('is unknown without a Permissions API at all', async () => {
    expect(await queryLocalNetworkPermission(undefined)).toBe('unknown');
  });
});
