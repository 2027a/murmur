package logging

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	debugLevel = iota
	infoLevel
	warnLevel
	errorLevel

	operationalLogName = "murmur.log"
	maxMessageBytes    = 512
)

// Component is deliberately closed by normalizeComponent before persistence.
type Component string

const (
	ComponentInternal       Component = "internal"
	ComponentLogging        Component = "logging"
	ComponentSession        Component = "session"
	ComponentSessionJournal Component = "session-journal"
	ComponentUI             Component = "ui"
	ComponentOSD            Component = "osd"
	ComponentDBus           Component = "dbus"
)

type loggerState struct {
	sync.Mutex
	level  int
	output io.Writer
	file   *RotatingFile
}

var configured = loggerState{level: infoLevel, output: os.Stderr}

type entry struct {
	Time      time.Time `json:"time"`
	Level     string    `json:"level"`
	Component Component `json:"component"`
	Message   string    `json:"message"`
}

// Path returns the operational log path in an already validated private runtime.
func Path(runtimeDir string) string {
	return filepath.Join(runtimeDir, operationalLogName)
}

// Configure changes the minimum level without changing the current destination.
func Configure(level string) {
	configured.Lock()
	configured.level = parseLevel(level)
	configured.Unlock()
}

// ConfigureFile installs an independent bounded writer for operational logs.
func ConfigureFile(level, path string, maxBytes int64, maxFiles int) error {
	file, err := NewRotatingFile(path, maxBytes, maxFiles)
	if err != nil {
		return err
	}
	configured.Lock()
	previous := configured.file
	configured.level = parseLevel(level)
	configured.output = file
	configured.file = file
	configured.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	return nil
}

// Close flushes the current file destination. Logging errors are best effort and
// are never fed back through the logger.
func Close() error {
	configured.Lock()
	file := configured.file
	configured.file = nil
	configured.output = os.Stderr
	configured.Unlock()
	if file == nil {
		return nil
	}
	return file.Close()
}

// OpenDestination prepares the private destination before a detached child has
// its raw stderr redirected to /dev/null. The child later installs its rotating
// writer on the same path.
func OpenDestination(path string) (*os.File, error) {
	return openFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
}

func Debugf(component Component, format string, args ...any) {
	logf(debugLevel, "debug", component, format, args...)
}

func Infof(component Component, format string, args ...any) {
	logf(infoLevel, "info", component, format, args...)
}

func Warnf(component Component, format string, args ...any) {
	logf(warnLevel, "warn", component, format, args...)
}

func Errorf(component Component, format string, args ...any) {
	logf(errorLevel, "error", component, format, args...)
}

func logf(level int, name string, component Component, format string, args ...any) {
	configured.Lock()
	defer configured.Unlock()
	if level < configured.level || configured.output == nil {
		return
	}
	message := fmt.Sprintf(format, args...)
	if len(message) > maxMessageBytes {
		message = message[:maxMessageBytes] + "..."
	}
	line, err := json.Marshal(entry{
		Time: time.Now().UTC(), Level: name,
		Component: normalizeComponent(component), Message: message,
	})
	if err != nil {
		return
	}
	line = append(line, '\n')
	_, _ = configured.output.Write(line)
}

func parseLevel(level string) int {
	switch level {
	case "debug":
		return debugLevel
	case "warn":
		return warnLevel
	case "error":
		return errorLevel
	default:
		return infoLevel
	}
}

func normalizeComponent(component Component) Component {
	switch component {
	case ComponentInternal, ComponentLogging, ComponentSession, ComponentSessionJournal,
		ComponentUI, ComponentOSD, ComponentDBus:
		return component
	default:
		return ComponentInternal
	}
}

// RotatingFile is a generic size-bounded writer. It is intentionally independent
// from the event journal: a failure in either channel cannot disable the other.
type RotatingFile struct {
	sync.Mutex
	path     string
	maxBytes int64
	maxFiles int
	file     *os.File
	size     int64
	closed   bool
}

func NewRotatingFile(path string, maxBytes int64, maxFiles int) (*RotatingFile, error) {
	if maxBytes < 1 || maxFiles < 1 {
		return nil, fmt.Errorf("limites de rotation invalides")
	}
	file, err := openFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &RotatingFile{path: path, maxBytes: maxBytes, maxFiles: maxFiles, file: file, size: info.Size()}, nil
}

func (w *RotatingFile) Write(data []byte) (int, error) {
	w.Lock()
	defer w.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if int64(len(data)) > w.maxBytes {
		return 0, fmt.Errorf("entrée de log supérieure à la limite")
	}
	if w.size+int64(len(data)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := w.file.Write(data)
	w.size += int64(written)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	return written, err
}

func (w *RotatingFile) Close() error {
	w.Lock()
	defer w.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.file.Close()
}

func (w *RotatingFile) rotate() error {
	if w.maxFiles == 1 {
		if err := w.file.Truncate(0); err != nil {
			return err
		}
		if _, err := w.file.Seek(0, io.SeekEnd); err != nil {
			return err
		}
		w.size = 0
		return nil
	}
	for index := w.maxFiles - 1; index >= 1; index-- {
		destination := fmt.Sprintf("%s.%d", w.path, index)
		source := w.path
		if index > 1 {
			source = fmt.Sprintf("%s.%d", w.path, index-1)
		}
		if err := os.Rename(source, destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	file, err := openFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		_ = os.Rename(w.path+".1", w.path)
		return err
	}
	previous := w.file
	w.file = file
	w.size = 0
	return previous.Close()
}

func openFile(path string, flags int) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("descripteur de log invalide")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	stat, statOK := info.Sys().(*syscall.Stat_t)
	if !statOK || !info.Mode().IsRegular() || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		_ = file.Close()
		return nil, fmt.Errorf("destination de log non régulière, non privée ou non détenue")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	info, err = file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	stat, statOK = info.Sys().(*syscall.Stat_t)
	if !statOK || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		_ = file.Close()
		return nil, fmt.Errorf("destination de log non régulière, non privée ou non détenue")
	}
	return file, nil
}
