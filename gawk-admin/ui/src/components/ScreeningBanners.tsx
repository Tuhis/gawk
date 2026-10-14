import type { ScreeningStatus } from '../api/types.ts';
import { href } from '../router/router.ts';
import ui from '../styles/ui.module.css';

/**
 * The screening plane's two alarms (R40). Both say that broadcasts are NOT
 * being screened right now, which is the one thing an operator must never
 * have to infer from silence — the DSA posture docs/71 §1 builds on.
 *
 *   * **No pager** (D12): no enabled webhook would deliver
 *     `content_flag.raised`, so a sampler in `flag` mode has paused itself —
 *     detecting with nobody to tell is worse than not detecting.
 *   * **Missing shards** (D22): a sampler pod has stopped polling, and its
 *     fixed share of the fleet is unscreened until it returns. A pod on an
 *     unreachable node is not replaced on its own; the banner names the two
 *     recovery steps, as the alert's runbook does.
 *
 * Renders nothing when the plane is healthy.
 */
export function ScreeningBanners({ status }: { status: ScreeningStatus | null }) {
  if (!status) return null;
  const missing = [...status.missingShards].sort((a, b) => a - b);
  return (
    <>
      {!status.flagRoute ? (
        <p className={ui.alarm} role="alert">
          <strong>No pager for content flags.</strong> No enabled webhook delivers{' '}
          <code className={ui.mono}>content_flag.raised</code>, so a sampler in flag mode has
          paused and is screening nothing (observe mode is unaffected). Enable or add one in{' '}
          <a href={href('webhooks')}>Webhooks</a>.
        </p>
      ) : null}
      {missing.length > 0 ? (
        <p className={ui.alarm} role="alert">
          <strong>
            {missing.length === 1
              ? `Sampler shard ${missing[0]} of ${status.expectedShards} is missing.`
              : `Sampler shards ${missing.join(', ')} of ${status.expectedShards} are missing.`}
          </strong>{' '}
          {missing.length === 1 ? 'It has' : 'They have'} not polled for over 30 s, so{' '}
          {missing.length === 1 ? 'its' : 'their'} share of live broadcasts is not being
          screened. A pod on an unreachable node is not replaced by itself: force-delete it (
          <code className={ui.mono}>kubectl delete pod --grace-period=0 --force</code>) or taint
          the node <code className={ui.mono}>node.kubernetes.io/out-of-service</code>.
        </p>
      ) : null}
    </>
  );
}
