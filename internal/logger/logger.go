package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
)

// LogLevel represents the severity level of a log message
type LogLevel int

const (
	DEBUG LogLevel = iota
	INFO
	WARNING
	ERROR
	FATAL
)

var logLevelNames = map[LogLevel]string{
	DEBUG:   "DEBUG",
	INFO:    "INFO",
	WARNING: "WARNING",
	ERROR:   "ERROR",
	FATAL:   "FATAL",
}

var logLevelColors = map[LogLevel]string{
	DEBUG:   "\033[36m", // Cyan
	INFO:    "\033[32m", // Green
	WARNING: "\033[33m", // Yellow
	ERROR:   "\033[31m", // Red
	FATAL:   "\033[35m", // Magenta
}

const colorReset = "\033[0m"

// Logger is a custom logger with log level support
type Logger struct {
	logger    *log.Logger
	level     LogLevel
	useColors bool
	mu        sync.RWMutex
}

// New creates a new Logger instance
func New(output io.Writer, prefix string, level LogLevel, useColors bool) *Logger {
	return &Logger{
		logger:    log.New(output, prefix, log.LstdFlags),
		level:     level,
		useColors: useColors,
	}
}

// NewDefault creates a logger with default settings (INFO level, with colors)
func NewDefault(prefix string) *Logger {
	return New(os.Stdout, prefix, INFO, true)
}

// SetLevel changes the minimum log level
func (l *Logger) SetLevel(level LogLevel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// GetLevel returns the current log level
func (l *Logger) GetLevel() LogLevel {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.level
}

// ParseLogLevel converts a string to LogLevel
func ParseLogLevel(level string) LogLevel {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "DEBUG":
		return DEBUG
	case "INFO":
		return INFO
	case "WARNING", "WARN":
		return WARNING
	case "ERROR":
		return ERROR
	case "FATAL":
		return FATAL
	default:
		return INFO
	}
}

// logf is the internal logging function
func (l *Logger) logf(level LogLevel, format string, v ...interface{}) {
	l.mu.RLock()
	minLevel := l.level
	l.mu.RUnlock()

	if level < minLevel {
		return
	}

	levelName := logLevelNames[level]
	message := fmt.Sprintf(format, v...)

	var logLine string
	if l.useColors {
		color := logLevelColors[level]
		logLine = fmt.Sprintf("%s[%s]%s %s", color, levelName, colorReset, message)
	} else {
		logLine = fmt.Sprintf("[%s] %s", levelName, message)
	}

	l.logger.Output(3, logLine)

	// Fatal logs should exit the program
	if level == FATAL {
		os.Exit(1)
	}
}

// Debug logs a message at DEBUG level
func (l *Logger) Debug(v ...interface{}) {
	l.logf(DEBUG, "%s", fmt.Sprint(v...))
}

// Debugf logs a formatted message at DEBUG level
func (l *Logger) Debugf(format string, v ...interface{}) {
	l.logf(DEBUG, format, v...)
}

// Info logs a message at INFO level
func (l *Logger) Info(v ...interface{}) {
	l.logf(INFO, "%s", fmt.Sprint(v...))
}

// Infof logs a formatted message at INFO level
func (l *Logger) Infof(format string, v ...interface{}) {
	l.logf(INFO, format, v...)
}

// Warning logs a message at WARNING level
func (l *Logger) Warning(v ...interface{}) {
	l.logf(WARNING, "%s", fmt.Sprint(v...))
}

// Warningf logs a formatted message at WARNING level
func (l *Logger) Warningf(format string, v ...interface{}) {
	l.logf(WARNING, format, v...)
}

// Error logs a message at ERROR level
func (l *Logger) Error(v ...interface{}) {
	l.logf(ERROR, "%s", fmt.Sprint(v...))
}

// Errorf logs a formatted message at ERROR level
func (l *Logger) Errorf(format string, v ...interface{}) {
	l.logf(ERROR, format, v...)
}

// Fatal logs a message at FATAL level and exits the program
func (l *Logger) Fatal(v ...interface{}) {
	l.logf(FATAL, "%s", fmt.Sprint(v...))
}

// Fatalf logs a formatted message at FATAL level and exits the program
func (l *Logger) Fatalf(format string, v ...interface{}) {
	l.logf(FATAL, format, v...)
}

// Printf provides compatibility with standard logger (logs at INFO level)
func (l *Logger) Printf(format string, v ...interface{}) {
	l.logf(INFO, format, v...)
}

// Println provides compatibility with standard logger (logs at INFO level)
func (l *Logger) Println(v ...interface{}) {
	l.logf(INFO, "%s", fmt.Sprint(v...))
}

