package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pion/logging"
)

// pionLogs routes pion's logging into the gateway's slog at debug level.
// pion's own default writes errors to stderr, and its "errors" include every
// closed socket at a session's end, which would bury the gateway's own
// lines; the session lifecycle the gateway logs itself says what matters.
type pionLogs struct{ log *slog.Logger }

func (f pionLogs) NewLogger(scope string) logging.LeveledLogger {
	return pionLogger{log: f.log.With("pion", scope)}
}

type pionLogger struct{ log *slog.Logger }

func (l pionLogger) Trace(string)          {}
func (l pionLogger) Tracef(string, ...any) {}
func (l pionLogger) Debug(string)          {}
func (l pionLogger) Debugf(string, ...any) {}
func (l pionLogger) Info(m string)         { l.log.Debug(m) }
func (l pionLogger) Infof(f string, a ...any) {
	l.log.Debug(fmt.Sprintf(f, a...))
}
func (l pionLogger) Warn(m string) { l.log.Debug(m) }
func (l pionLogger) Warnf(f string, a ...any) {
	l.log.Debug(fmt.Sprintf(f, a...))
}
func (l pionLogger) Error(m string) { l.log.Debug(m, "level", "error") }
func (l pionLogger) Errorf(f string, a ...any) {
	l.log.Debug(fmt.Sprintf(f, a...), "level", "error")
}

// scrubIDs drops the raw broadcast ID from the engine's log lines. The
// engine logs it as broadcast_id (its native shells show the code to their
// own user); in an operator's log a joinable code is the one thing CLAUDE.md
// says never to expose, and the session's own key-hash attribute already
// says which broadcast a line is about.
type scrubIDs struct{ slog.Handler }

func (h scrubIDs) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key != "broadcast_id" {
			out.AddAttrs(a)
		}
		return true
	})
	return h.Handler.Handle(ctx, out)
}

func (h scrubIDs) WithAttrs(as []slog.Attr) slog.Handler {
	kept := as[:0:0]
	for _, a := range as {
		if a.Key != "broadcast_id" {
			kept = append(kept, a)
		}
	}
	return scrubIDs{h.Handler.WithAttrs(kept)}
}

func (h scrubIDs) WithGroup(name string) slog.Handler { return scrubIDs{h.Handler.WithGroup(name)} }
