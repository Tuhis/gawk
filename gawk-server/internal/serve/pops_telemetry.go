package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopTelemetry turns on minting for PoP sessions (R77, docs/78) when
// home collects telemetry; without a key nothing changes and PoPs report
// nothing, as before.
func wirePopTelemetry(_ context.Context, w Wired, m *popsrv.Manager, log *slog.Logger) error {
	t := m.StartTelemetry(popsrv.TelemetryOptions{
		Key:              w.Cfg.TelemetryKey,
		ReportIntervalMs: w.Server.TelemetryReportIntervalMs(),
		AdvertiseURL:     w.Cfg.TelemetryAdvertiseURL,
		Reservations:     w.Reservations,
		ServesDownlink:   w.Server.PopServesDownlink,
	})
	if t != nil {
		log.Debug("pop telemetry wired: home mints PoP sessions' tokens")
	}
	return nil
}
