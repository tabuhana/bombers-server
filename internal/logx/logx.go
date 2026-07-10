// Package logx is the server's small leveled, optionally-colored logger. Every
// call prints one line to os.Stdout in the form
//
//	[<timestamp>][<LEVEL>]: <message>
//
// Color (24-bit truecolor SGR) turns on only when stdout is a real terminal AND
// NO_COLOR is unset, so piped or redirected logs stay plain and greppable. The
// timestamp layout is switchable at runtime (the console's `logtime` command)
// and guarded by a mutex because many goroutines read it while the console may
// be writing it. It has no third-party dependencies — a leaf package the whole
// tree can import without cycles.
package logx

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Timestamp format names accepted by SetTimeFormat / LOG_TIME_FORMAT, mapped to
// their Go reference layouts. datetime is the default.
const (
	FormatTime     = "time"
	FormatDateTime = "datetime"
	FormatISO      = "iso"
)

var layouts = map[string]string{
	FormatTime:     "15:04:05",
	FormatDateTime: "2006-01-02 15:04:05",
	FormatISO:      time.RFC3339,
}

var (
	writeMu sync.Mutex   // serializes writes so concurrent lines don't tear
	fmtMu   sync.RWMutex // guards timeFormat (console mutates while logs read)

	timeFormat   = FormatDateTime
	colorEnabled bool
	interactive  bool
)

// level is a log severity: its fixed-width label (padded so the ":" aligns) and
// the truecolor it paints in when color is enabled.
type level struct {
	label   string
	r, g, b uint8
	bold    bool
}

var (
	lvlInfo  = level{label: "INFO ", r: 124, g: 182, b: 156}
	lvlWarn  = level{label: "WARN ", r: 224, g: 177, b: 92}
	lvlError = level{label: "ERROR", r: 212, g: 106, b: 79}
	lvlFatal = level{label: "FATAL", r: 224, g: 72, b: 52, bold: true}
)

// Init configures the logger from the environment. Call it once from main after
// godotenv.Load(): it reads LOG_TIME_FORMAT (time|datetime|iso, default
// datetime) and enables color when stdout is a character device and NO_COLOR is
// unset. An unrecognized LOG_TIME_FORMAT is ignored (the default stands) rather
// than failing boot.
func Init() {
	interactive = stdoutIsCharDevice()
	colorEnabled = interactive && os.Getenv("NO_COLOR") == ""
	if name := os.Getenv("LOG_TIME_FORMAT"); name != "" {
		_ = SetTimeFormat(name)
	}
}

// stdoutIsCharDevice reports whether stdout looks like an interactive terminal,
// the same probe console.Interactive uses for stdin.
func stdoutIsCharDevice() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Info logs at INFO. Warn/Error/Fatal are the same at their level; Fatal exits
// the process with status 1 after printing.
func Info(format string, args ...any)  { emit(lvlInfo, format, args...) }
func Warn(format string, args ...any)  { emit(lvlWarn, format, args...) }
func Error(format string, args ...any) { emit(lvlError, format, args...) }

func Fatal(format string, args ...any) {
	emit(lvlFatal, format, args...)
	os.Exit(1)
}

func emit(l level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ts := timestamp()

	tsPart, label := ts, l.label
	if colorEnabled {
		tsPart = faint(ts)
		label = l.paint()
	}

	writeMu.Lock()
	fmt.Fprintf(os.Stdout, "[%s][%s]: %s\n", tsPart, label, msg)
	writeMu.Unlock()
}

func timestamp() string {
	fmtMu.RLock()
	layout := layouts[timeFormat]
	fmtMu.RUnlock()
	return time.Now().Format(layout)
}

// SetTimeFormat switches the timestamp layout for every subsequent log line.
// name must be one of time|datetime|iso; anything else is rejected and the
// current format is left unchanged.
func SetTimeFormat(name string) error {
	if _, ok := layouts[name]; !ok {
		return fmt.Errorf("unknown log time format %q (use time|datetime|iso)", name)
	}
	fmtMu.Lock()
	timeFormat = name
	fmtMu.Unlock()
	return nil
}

// TimeFormat returns the current timestamp format name.
func TimeFormat() string {
	fmtMu.RLock()
	defer fmtMu.RUnlock()
	return timeFormat
}

// ColorEnabled reports whether output is colored (stdout is a terminal and
// NO_COLOR is unset). The banner uses it to pick colored vs plain rows.
func ColorEnabled() bool { return colorEnabled }

// Interactive reports whether stdout is a character device (a real terminal).
// main gates the startup banner on it so piped logs stay clean.
func Interactive() bool { return interactive }

// Paint wraps text in a 24-bit truecolor foreground SGR when color is enabled,
// otherwise returns text unchanged. Used by the startup banner.
func Paint(text string, r, g, b uint8) string {
	if !colorEnabled {
		return text
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", r, g, b, text)
}

// paint renders the level's label in its color (bold for FATAL).
func (l level) paint() string {
	body := fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", l.r, l.g, l.b, l.label)
	if l.bold {
		return "\x1b[1m" + body
	}
	return body
}

// faint dims text with the ANSI faint attribute so it recedes against any
// background; \x1b[22m resets intensity without disturbing color.
func faint(s string) string {
	return "\x1b[2m" + s + "\x1b[22m"
}
