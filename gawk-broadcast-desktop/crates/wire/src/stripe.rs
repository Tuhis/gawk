//! Striped delivery (R30, docs/35), mirroring gawk-server/wire/stripe.go.
//! This producer neither sends nor receives StripeState — it is viewer↔relay —
//! but the vectors are mirrored by rule, exactly like the carrier framing.

use crate::error::WireError;
use crate::{TYPE_STRIPE_STATE, VERSION};

/// Exact size of a StripeState datagram.
pub const STRIPE_STATE_SIZE: usize = 5;

/// Bounds a viewer's stripe width. Evidence-bound, not tuned (docs/35 §11).
pub const MAX_STRIPE_LEGS: u8 = 4;

const STRIPE_FLAG_STRIPED: u8 = 0x01;

/// The primary-suppression signal: level state, re-sent at 1 Hz while striped.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct StripeState {
    /// Whether delta datagrams should be suppressed on the session this
    /// message arrived on.
    pub striped: bool,
    /// The viewer's current stripe width, informational. In
    /// `[1, MAX_STRIPE_LEGS]` when striped, 0 otherwise.
    pub stripe_n: u8,
}

fn validate(s: &StripeState) -> Result<(), WireError> {
    if s.striped {
        if s.stripe_n < 1 || s.stripe_n > MAX_STRIPE_LEGS {
            return Err(WireError::BadChunkCount {
                index: s.stripe_n.into(),
                count: MAX_STRIPE_LEGS.into(),
            });
        }
        return Ok(());
    }
    if s.stripe_n != 0 {
        return Err(WireError::BadChunkCount {
            index: s.stripe_n.into(),
            count: 0,
        });
    }
    Ok(())
}

/// Appends a StripeState datagram.
pub fn append_stripe_state(dst: &mut Vec<u8>, s: &StripeState) -> Result<(), WireError> {
    validate(s)?;
    let flags = if s.striped { STRIPE_FLAG_STRIPED } else { 0 };
    dst.extend_from_slice(&[VERSION, TYPE_STRIPE_STATE, flags, s.stripe_n, 0]);
    Ok(())
}

/// Parses a StripeState datagram. Strict: unknown flag bits are rejected —
/// a future revision gates on a new RelayCapabilities bit, never on old
/// parsers guessing.
pub fn parse_stripe_state(msg: &[u8]) -> Result<StripeState, WireError> {
    if msg.len() != STRIPE_STATE_SIZE {
        return Err(WireError::ShortDatagram {
            len: msg.len(),
            need: STRIPE_STATE_SIZE,
        });
    }
    if msg[0] != VERSION {
        return Err(WireError::BadVersion(msg[0]));
    }
    if msg[1] != TYPE_STRIPE_STATE {
        return Err(WireError::BadType {
            got: msg[1],
            want: TYPE_STRIPE_STATE,
        });
    }
    if msg[2] & !STRIPE_FLAG_STRIPED != 0 {
        return Err(WireError::UnknownStripeFlags(msg[2]));
    }
    let s = StripeState {
        striped: msg[2] & STRIPE_FLAG_STRIPED != 0,
        stripe_n: msg[3],
    };
    validate(&s)?;
    Ok(s)
}

/// The stripe ordinal of a delta datagram: data chunk i has ordinal i, parity
/// symbol r over an n-chunk frame has ordinal n+r. Leg j of stripe N carries
/// the datagrams with ordinal % N == j.
pub fn stripe_ordinal(chunk_index: u16, chunk_count: u16, parity_index: Option<u8>) -> u32 {
    match parity_index {
        Some(r) => u32::from(chunk_count) + u32::from(r),
        None => u32::from(chunk_index),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stripe_state_shape_rules() {
        // Striped names its width…
        assert!(
            append_stripe_state(
                &mut Vec::new(),
                &StripeState {
                    striped: true,
                    stripe_n: 0
                }
            )
            .is_err()
        );
        assert!(
            append_stripe_state(
                &mut Vec::new(),
                &StripeState {
                    striped: true,
                    stripe_n: MAX_STRIPE_LEGS + 1
                }
            )
            .is_err()
        );
        // …an unstriped one names none.
        assert!(
            append_stripe_state(
                &mut Vec::new(),
                &StripeState {
                    striped: false,
                    stripe_n: 1
                }
            )
            .is_err()
        );

        let mut msg = Vec::new();
        append_stripe_state(
            &mut msg,
            &StripeState {
                striped: true,
                stripe_n: 3,
            },
        )
        .unwrap();
        assert_eq!(
            parse_stripe_state(&msg).unwrap(),
            StripeState {
                striped: true,
                stripe_n: 3
            }
        );

        // Unknown flag bits are rejected, never masked.
        let mut bad = msg.clone();
        bad[2] |= 0x02;
        assert_eq!(
            parse_stripe_state(&bad),
            Err(WireError::UnknownStripeFlags(0x03))
        );
    }

    #[test]
    fn ordinals_put_parity_after_data() {
        assert_eq!(stripe_ordinal(0, 9, None), 0);
        assert_eq!(stripe_ordinal(8, 9, None), 8);
        assert_eq!(stripe_ordinal(0, 9, Some(0)), 9);
        assert_eq!(stripe_ordinal(0, 9, Some(1)), 10);
    }
}
