package dunetask

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Level is a log level (the names are Python's, as the Python SDK prints
// them).
type Level int

const (
	LevelDebug Level = 10
	LevelInfo  Level = 20
	LevelWarn  Level = 30
	LevelError Level = 40
	LevelCrit  Level = 50
)

func (l Level) String() string {
	switch {
	case l >= LevelCrit:
		return "CRITICAL"
	case l >= LevelError:
		return "ERROR"
	case l >= LevelWarn:
		return "WARNING"
	case l >= LevelInfo:
		return "INFO"
	}
	return "DEBUG"
}

// ParseLevel parses DEBUG, INFO, WARNING (WARN), ERROR or CRITICAL.
func ParseLevel(s string) (Level, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return LevelDebug, true
	case "INFO":
		return LevelInfo, true
	case "WARNING", "WARN":
		return LevelWarn, true
	case "ERROR":
		return LevelError, true
	case "CRITICAL", "FATAL":
		return LevelCrit, true
	}
	return LevelInfo, false
}

// Output is where log lines go: "LEVEL logger: text" lines, with a time
// stamp first unless the program runs under DUNE's Supervisors.Process
// (DUNE_TASK_NAME set: DUNE stamps the lines it logs).
type Output struct {
	mu    sync.Mutex
	w     io.Writer
	level Level
	stamp bool
}

// NewOutput returns an output to w (os.Stderr if nil) at the given level.
func NewOutput(w io.Writer, level Level, stamp bool) *Output {
	if w == nil {
		w = os.Stderr
	}
	return &Output{w: w, level: level, stamp: stamp}
}

var defaultOutput = func() *Output {
	level, _ := ParseLevel(os.Getenv("DUNE_LOG"))
	return NewOutput(os.Stderr, level, strings.TrimSpace(os.Getenv("DUNE_TASK_NAME")) == "")
}()

// SetLevel changes the level of the output.
func (o *Output) SetLevel(l Level) {
	o.mu.Lock()
	o.level = l
	o.mu.Unlock()
}

// Logger writes lines of one logger name to an Output.
type Logger struct {
	out  *Output
	name string
}

// Logger returns a logger of the given name.
func (o *Output) Logger(name string) *Logger { return &Logger{out: o, name: name} }

// Enabled tells whether lines of this level are written.
func (l *Logger) Enabled(level Level) bool {
	l.out.mu.Lock()
	defer l.out.mu.Unlock()
	return level >= l.out.level
}

// Log writes a line.
func (l *Logger) Log(level Level, format string, args ...any) {
	o := l.out
	o.mu.Lock()
	defer o.mu.Unlock()
	if level < o.level {
		return
	}
	text := format
	if len(args) > 0 {
		text = fmt.Sprintf(format, args...)
	}
	var b strings.Builder
	if o.stamp {
		b.WriteString(time.Now().Format("2006-01-02 15:04:05,000 "))
	}
	b.WriteString(level.String())
	b.WriteByte(' ')
	b.WriteString(l.name)
	b.WriteString(": ")
	b.WriteString(text)
	b.WriteByte('\n')
	io.WriteString(o.w, b.String())
}

func (l *Logger) Debugf(format string, args ...any) { l.Log(LevelDebug, format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.Log(LevelInfo, format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.Log(LevelWarn, format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.Log(LevelError, format, args...) }
