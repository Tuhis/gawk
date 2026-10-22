package serve

import (
	"context"
	"log/slog"
)

// wirePops installs the home side of R74 PoP relays (docs/76) when -pops is
// on. A stub until W2-link fills it: Run calls it after wireSubsystems, so
// the cluster and the ban source it builds on are already wired.
func wirePops(ctx context.Context, w Wired, log *slog.Logger) error { return nil }
