/*
MIT License

Copyright (c) 2025 Pavel Konovalov

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package llog

import (
	"fmt"
	"log"
	"os"
	"strings"
)

const (
	Ldate         = log.Ldate             // the date in the local time zone: 2009/01/23
	Ltime         = log.Ltime             // the time in the local time zone: 01:23:23
	Lmicroseconds = log.Lmicroseconds     // microsecond resolution: 01:23:23.123123.  assumes Ltime.
	Llongfile     = log.Llongfile         // full file name and line number: /a/b/c/d.go:23
	Lshortfile    = log.Lshortfile        // final file name element and line number: d.go:23. overrides Llongfile
	LUTC          = log.LUTC              // if Ldate or Ltime is set, use UTC rather than the local time zone
	Lmsgprefix    = log.Lmsgprefix        // move the "prefix" from the beginning of the line to before the message
	LstdFlags     = log.Ldate | log.Ltime // initial values for the standard logger
)
const (
	ColorRed   = "\033[31m"
	ColorReset = "\033[0m"
	ColorGreen = "\033[32m"
)

// Level type
type Level uint32

type LevelLog struct {
	Trace    *log.Logger // Tracing dataflow
	Debug    *log.Logger // Just about anything
	Info     *log.Logger // Important information
	Warning  *log.Logger // Be concerned
	Error    *log.Logger // Critical problem
	curLevel Level
	prefix   string
}

var Logger *LevelLog

func init() {
	Logger = NewLevelLog(Ldate | Ltime | Lmicroseconds)
}

// NewLevelLog constructor, flag: log.Ldate|log.Ltime|log.Lmicroseconds
func NewLevelLog(flag int) *LevelLog {
	return &LevelLog{

		Warning:  log.New(os.Stdout, "WARN:  ", flag),
		Debug:    log.New(os.Stdout, "DEBUG: ", flag),
		Trace:    log.New(os.Stdout, "TRACE: ", flag),
		Info:     log.New(os.Stdout, "INFO:  ", flag),
		Error:    log.New(os.Stderr, "ERROR: ", flag),
		curLevel: InfoLevel,
	}
}

// These are the different logging levels
const (
	ErrorLevel Level = iota
	// WarnLevel level. Non-critical entries that deserve eyes.
	WarnLevel
	// InfoLevel level. General operational entries about what's going on inside the
	// application.
	InfoLevel
	// DebugLevel level. Usually only enabled when debugging. Very verbose logging.
	DebugLevel
	TraceLevel
)

// Convert the Level to a string. E.g. DebugLevel becomes "debug"
func (level Level) String() string {
	if b, err := level.MarshalText(); err == nil {
		return string(b)
	} else {
		return "unknown"
	}
}

func (level Level) UpperString() string {
	return strings.ToUpper(level.String())
}

// ParseLevel takes a string level and returns the log level constant
func ParseLevel(level string) (Level, error) {
	switch strings.ToLower(level) {
	case "error":
		return ErrorLevel, nil
	case "warn", "warning":
		return WarnLevel, nil
	case "info":
		return InfoLevel, nil
	case "debug":
		return DebugLevel, nil
	case "trace", "debug2":
		return TraceLevel, nil
	}

	var l Level
	return l, fmt.Errorf("not a valid log Level: %q", level)
}

func (level Level) MarshalText() ([]byte, error) {
	switch level {
	case DebugLevel:
		return []byte("debug"), nil
	case TraceLevel:
		return []byte("trace"), nil
	case InfoLevel:
		return []byte("info"), nil
	case WarnLevel:
		return []byte("warning"), nil
	case ErrorLevel:
		return []byte("error"), nil
	}

	return nil, fmt.Errorf("not a valid log level %d", level)
}

// UnmarshalText implements encoding.TextUnmarshaler
func (level *Level) UnmarshalText(text []byte) error {
	l, err := ParseLevel(string(text))
	if err != nil {
		return err
	}

	*level = l

	return nil
}

func (l *LevelLog) SetPrefix(prefix string) {
	l.prefix = prefix
}

func (l *LevelLog) SetLevel(level Level) {
	l.curLevel = level
}

func (l *LevelLog) Errorf(format string, args ...any) {
	l.Error.Printf(ColorRed+l.prefix+format+ColorReset, args...)
}

func (l *LevelLog) Fatalf(format string, args ...any) {
	l.Error.Fatalf(ColorRed+l.prefix+format+ColorReset, args...)
}

func (l *LevelLog) Infof(format string, args ...any) {
	if l.IsLevelEnabled(InfoLevel) {
		l.Info.Printf(l.prefix+format, args...)
	}
}

func (l *LevelLog) Debugf(format string, args ...any) {
	if l.IsLevelEnabled(DebugLevel) {
		l.Debug.Printf(l.prefix+format, args...)
	}
}

func (l *LevelLog) Tracef(format string, args ...any) {
	if l.IsLevelEnabled(TraceLevel) {
		l.Trace.Printf(l.prefix+format, args...)
	}
}

func (l *LevelLog) Warnf(format string, args ...any) {
	if l.IsLevelEnabled(WarnLevel) {
		l.Warning.Printf(l.prefix+format, args...)
	}
}

func (l *LevelLog) DebugColourf(format string, args ...any) {
	if l.IsLevelEnabled(DebugLevel) {
		l.Debug.Printf(ColorGreen+l.prefix+format+ColorReset, args...)
	}
}

// IsLevelEnabled checks if the log level of the logger is greater than the level param
func (l *LevelLog) IsLevelEnabled(level Level) bool {
	return l.curLevel >= level
}

// GetLevel returns the current logger level.
func (l *LevelLog) GetLevel() Level {
	return l.curLevel
}

// GetLevelString returns the current logger level as a string.
func (l *LevelLog) GetLevelString() string {
	return strings.ToUpper(l.curLevel.String())
}
