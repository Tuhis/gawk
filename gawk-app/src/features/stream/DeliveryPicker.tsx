import styles from './stream.module.css';
import { useBroadcastSettingsStore } from '../../state/broadcastSettingsStore';
import {
  DEFAULT_DELIVERY,
  DELIVERIES,
  DELIVERY_HINTS,
  DELIVERY_LABEL,
  DELIVERY_LABELS,
  type Delivery,
} from '../../transport/carrier-sender';

interface Props {
  // Invoked after the store updates so a live pipeline can apply the change
  // (at its next keyframe, docs/88 D6).
  onChange?: (delivery: Delivery) => void;
}

// The Delivery setting (docs/88 §7, BR3): the native Quality page's control
// and words. The choice is a request — on a relay without carriers, or a
// browser OD4 leaves out, deltas still go as datagrams; Diagnostics shows the
// delivery in use beside it. Never disabled: a change applies live.
export function DeliveryPicker({ onChange }: Props) {
  const stored = useBroadcastSettingsStore((s) => s.uplinkDelivery);
  const setUplinkDelivery = useBroadcastSettingsStore((s) => s.setUplinkDelivery);
  const delivery = stored ?? DEFAULT_DELIVERY;
  const hintId = 'delivery-hint';

  return (
    <div className={styles.field}>
      <span id="delivery-label" className={styles.fieldLabel}>
        {DELIVERY_LABEL}
      </span>
      <div className={styles.segment} role="radiogroup" aria-labelledby="delivery-label" aria-describedby={hintId}>
        {DELIVERIES.map((d) => (
          <button
            key={d}
            type="button"
            role="radio"
            aria-checked={delivery === d}
            className={styles.segmentBtn}
            onClick={() => {
              setUplinkDelivery(d);
              onChange?.(d);
            }}
          >
            {DELIVERY_LABELS[d]}
          </button>
        ))}
      </div>
      <p id={hintId} className={styles.fieldHint}>
        {DELIVERY_HINTS[delivery]}
      </p>
    </div>
  );
}
