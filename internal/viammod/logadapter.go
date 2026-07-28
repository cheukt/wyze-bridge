package viammod

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"go.viam.com/rdk/logging"
)

// viamLogWriter is a zerolog.LevelWriter that forwards each zerolog event to a
// Viam logging.Logger, routing by level. zerolog emits one JSON object per
// event; we extract the message and remaining fields and hand a readable line
// to the Viam logger, which supplies its own timestamp and level. The zerolog
// side passes everything through (Trace) and the Viam logger does the real
// level filtering — so log_level is a Viam concern, never a zerolog global.
type viamLogWriter struct {
	l logging.Logger
}

// newZerologToViam builds a zerolog.Logger that forwards to the given Viam
// logger. It is set to TraceLevel so nothing is dropped before reaching Viam.
func newZerologToViam(l logging.Logger) zerolog.Logger {
	return zerolog.New(viamLogWriter{l: l}).Level(zerolog.TraceLevel)
}

// captureZerologGlobals points the zerolog package-global logger at zl and
// widens the global level gate to Trace, so the few internal call sites that
// log through zerolog's global (e.g. internal/wyzeapi/state.go) also reach the
// Viam logger. Both are deliberate global writes but benign: viam-server uses
// zap, not rs/zerolog, so nothing else in the process reads these; widening
// the gate is non-destructive (Viam does the real filtering). Idempotent — the
// latest-constructed service wins, which is the AlwaysRebuild behavior we want.
func captureZerologGlobals(zl zerolog.Logger) {
	zlog.Logger = zl
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
}

// Write satisfies io.Writer; zerolog calls WriteLevel when the writer is a
// LevelWriter, but io.Writer is required by the interface. Treat level-less
// writes as info.
func (w viamLogWriter) Write(p []byte) (int, error) {
	return w.WriteLevel(zerolog.InfoLevel, p)
}

// WriteLevel routes the event to the matching Viam logger method.
func (w viamLogWriter) WriteLevel(level zerolog.Level, p []byte) (int, error) {
	msg := formatEvent(p)
	switch level {
	case zerolog.TraceLevel, zerolog.DebugLevel:
		w.l.Debug(msg)
	case zerolog.InfoLevel, zerolog.NoLevel:
		w.l.Info(msg)
	case zerolog.WarnLevel:
		w.l.Warn(msg)
	default: // Error, Fatal, Panic
		// go2rtc emits transient connect/discovery failures at Error level on
		// every lazy dial retry (e.g. "streams: wyze: connect failed: discovery
		// timeout"). These are expected during camera backoff and are not
		// operator-actionable, so route them to Debug; genuine errors stay loud.
		if isTransientGo2rtcError(msg) {
			w.l.Debug(msg)
		} else {
			w.l.Error(msg)
		}
	}
	return len(p), nil
}

// transientGo2rtcErrors are substrings of go2rtc-originated error messages that
// represent expected, self-healing retry conditions rather than real faults.
var transientGo2rtcErrors = []string{
	"connect failed",
	"discovery timeout",
}

// isTransientGo2rtcError reports whether msg matches a known-transient go2rtc
// connect/discovery failure that should be downgraded out of the Error stream.
func isTransientGo2rtcError(msg string) bool {
	for _, s := range transientGo2rtcErrors {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// formatEvent turns a zerolog JSON event into a readable line: the message
// followed by the remaining fields as sorted key=value pairs. The level and
// timestamp keys are dropped (Viam adds its own). If the bytes aren't JSON,
// the trimmed raw line is returned.
func formatEvent(p []byte) string {
	var m map[string]interface{}
	if err := json.Unmarshal(p, &m); err != nil {
		return strings.TrimSpace(string(p))
	}

	msg, _ := m[zerolog.MessageFieldName].(string)
	delete(m, zerolog.MessageFieldName)
	delete(m, zerolog.LevelFieldName)
	delete(m, zerolog.TimestampFieldName)

	if len(m) == 0 {
		return msg
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(msg)
	for _, k := range keys {
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		fmt.Fprintf(&b, "%v", m[k])
	}
	return b.String()
}
