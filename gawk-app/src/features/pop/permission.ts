// The browser's local-network permission (docs/76 item 43.4, M2). Chrome's
// Local Network Access gates a public page dialing a private address; the
// permission has shipped under two names, so both are asked, in order. A
// browser whose Permissions API knows neither throws a TypeError for the
// name, which reads as 'unknown': the client then just probes.

export type LocalNetworkPermission = 'granted' | 'prompt' | 'denied' | 'unknown';

export interface PermissionsLike {
  query(descriptor: { name: string }): Promise<PermissionStatus>;
}

export const LOCAL_NETWORK_PERMISSION_NAMES = ['local-network', 'local-network-access'] as const;

function defaultPermissions(): PermissionsLike | undefined {
  if (typeof navigator === 'undefined') return undefined;
  return (navigator as Navigator & { permissions?: PermissionsLike }).permissions;
}

export async function queryLocalNetworkPermission(
  permissions: PermissionsLike | undefined = defaultPermissions(),
): Promise<LocalNetworkPermission> {
  if (!permissions || typeof permissions.query !== 'function') return 'unknown';
  for (const name of LOCAL_NETWORK_PERMISSION_NAMES) {
    try {
      const status = await permissions.query({ name });
      const state = status?.state;
      if (state === 'granted' || state === 'prompt' || state === 'denied') return state;
      return 'unknown';
    } catch {
      // Not a name this browser knows: try the next one.
    }
  }
  return 'unknown';
}
