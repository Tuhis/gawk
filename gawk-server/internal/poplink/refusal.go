package poplink

import (
	"errors"

	"github.com/Tuhis/gawk/gawk-server/pops"
)

// RefusalReason names a pops refusal for ReasonHeader, so the console can
// tell refusals that share a status apart ("" for anything else).
func RefusalReason(err error) string {
	switch {
	case errors.Is(err, pops.ErrPairingUnknown):
		return ReasonUnknown
	case errors.Is(err, pops.ErrPairingUsed):
		return ReasonUsed
	case errors.Is(err, pops.ErrPairingExpired):
		return ReasonPairingExpired
	case errors.Is(err, pops.ErrRevoked):
		return ByeRevoked
	case errors.Is(err, pops.ErrExpired):
		return ByeExpired
	case errors.Is(err, pops.ErrCredentialNotCurrent):
		return ByeRepair
	}
	return ""
}
