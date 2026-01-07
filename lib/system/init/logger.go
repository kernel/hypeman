package main

import (
	"fmt"
	"os"
	"time"
)

// Logger provides human-readable structured logging for the init process.
// Logs are written to serial console.
type Logger struct {
	console *os.File
}

// NewLogger creates a new logger that writes to serial console.
func NewLogger() *Logger {
	l := &Logger{}

	// Open serial console for output
	// ttyS0 for x86_64, ttyAMA0 for ARM64 (PL011 UART)
	if f, err := os.OpenFile("/dev/ttyAMA0", os.O_WRONLY, 0); err == nil {
		l.console = f
	} else if f, err := os.OpenFile("/dev/ttyS0", os.O_WRONLY, 0); err == nil {
		l.console = f
	} else {
		// Fallback to stdout
		l.console = os.Stdout
	}
	return l
}

// SetConsole sets the serial console for output.
func (l *Logger) SetConsole(path string) {
	if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
		l.console = f
	}
}

// Info logs an informational message.
// Format: 2024-12-23T10:15:30Z [INFO] [phase] message
func (l *Logger) Info(phase, msg string) {
	ts := time.Now().UTC().Format(time.RFC3339)
	line := fmt.Sprintf("%s [INFO] [%s] %s\n", ts, phase, msg)
	l.write(line)
}

// Error logs an error message.
// Format: 2024-12-23T10:15:30Z [ERROR] [phase] message: error
func (l *Logger) Error(phase, msg string, err error) {
	ts := time.Now().UTC().Format(time.RFC3339)
	var line string
	if err != nil {
		line = fmt.Sprintf("%s [ERROR] [%s] %s: %v\n", ts, phase, msg, err)
	} else {
		line = fmt.Sprintf("%s [ERROR] [%s] %s\n", ts, phase, msg)
	}
	l.write(line)
}

// Infof logs a formatted informational message.
func (l *Logger) Infof(phase, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	l.Info(phase, msg)
}

// write outputs a log line to serial console.
func (l *Logger) write(line string) {
	if l.console != nil {
		l.console.WriteString(line)
	}
}
