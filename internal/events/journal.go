package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type JournalOperation string

const (
	JournalMarshal JournalOperation = "marshal"
	JournalStat    JournalOperation = "stat"
	JournalRename  JournalOperation = "rename"
	JournalWrite   JournalOperation = "write"
	JournalReopen  JournalOperation = "reopen"
	JournalClose   JournalOperation = "close"
)

type JournalOptions struct {
	MaxBytes  int64
	MaxFiles  int
	OnFailure func(JournalOperation)

	operations *journalOperations
}

type JournalError struct {
	Operation JournalOperation
	Err       error
}

func (e *JournalError) Error() string {
	return fmt.Sprintf("journal de session (%s): %v", e.Operation, e.Err)
}

func (e *JournalError) Unwrap() error { return e.Err }

// JournalFailureMessage is safe for logs and persisted results: it only uses a
// closed operation name, never an event, an underlying error or a path.
func JournalFailureMessage(operation JournalOperation) string {
	switch operation {
	case JournalMarshal, JournalStat, JournalRename, JournalWrite, JournalReopen, JournalClose:
		return fmt.Sprintf("journal de session dégradé (%s)", operation)
	default:
		return "journal de session dégradé"
	}
}

type journalOperations struct {
	stat   func(*os.File) (os.FileInfo, error)
	rename func(string, string) error
	remove func(string) error
	write  func(*os.File, []byte) (int, error)
	open   func(string, int) (*os.File, error)
	close  func(*os.File) error
}

func defaultJournalOperations() *journalOperations {
	return &journalOperations{
		stat:   func(file *os.File) (os.FileInfo, error) { return file.Stat() },
		rename: os.Rename,
		remove: os.Remove,
		write:  func(file *os.File, data []byte) (int, error) { return file.Write(data) },
		open: func(path string, flags int) (*os.File, error) {
			file, err := os.OpenFile(path, flags, 0o600)
			if err != nil {
				return nil, err
			}
			if err := file.Chmod(0o600); err != nil {
				_ = file.Close()
				return nil, err
			}
			return file, nil
		},
		close: func(file *os.File) error { return file.Close() },
	}
}

func (operations *journalOperations) withDefaults() *journalOperations {
	defaults := defaultJournalOperations()
	if operations == nil {
		return defaults
	}
	copy := *operations
	if copy.stat == nil {
		copy.stat = defaults.stat
	}
	if copy.rename == nil {
		copy.rename = defaults.rename
	}
	if copy.remove == nil {
		copy.remove = defaults.remove
	}
	if copy.write == nil {
		copy.write = defaults.write
	}
	if copy.open == nil {
		copy.open = defaults.open
	}
	if copy.close == nil {
		copy.close = defaults.close
	}
	return &copy
}

type Journal struct {
	path        string
	options     JournalOptions
	operations  *journalOperations
	file        *os.File
	stream      <-chan Event
	unsubscribe func()
	done        chan struct{}

	errorMu   sync.Mutex
	firstErr  error
	closeErr  error
	closeOnce sync.Once
}

func NewJournal(bus *Bus, path string, options JournalOptions) (*Journal, error) {
	if bus == nil {
		return nil, fmt.Errorf("bus d'événements absent")
	}
	if options.MaxBytes < 1 || options.MaxFiles < 1 {
		return nil, fmt.Errorf("limites du journal invalides")
	}
	operations := options.operations.withDefaults()
	file, err := openJournal(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, operations)
	if err != nil {
		return nil, err
	}
	stream, unsubscribe := bus.Subscribe(256)
	journal := &Journal{
		path: path, options: options, operations: operations, file: file,
		stream: stream, unsubscribe: unsubscribe, done: make(chan struct{}),
	}
	go journal.run()
	return journal, nil
}

// Err returns the first journal failure. Filesystem causes can contain the
// configured journal path and are intended for local diagnostics only.
func (j *Journal) Err() error {
	if j == nil {
		return nil
	}
	j.errorMu.Lock()
	defer j.errorMu.Unlock()
	return j.firstErr
}

func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	j.closeOnce.Do(func() {
		j.unsubscribe()
		<-j.done
		if err := j.operations.close(j.file); err != nil {
			j.recordFailure(JournalClose, err)
		}
		j.closeErr = j.Err()
	})
	return j.closeErr
}

func (j *Journal) run() {
	defer close(j.done)
	for event := range j.stream {
		line, err := json.Marshal(event)
		if err != nil {
			j.recordFailure(JournalMarshal, err)
			continue
		}
		line = append(line, '\n')
		if err := j.rotateIfNeeded(int64(len(line))); err != nil {
			j.recordError(err)
		}
		written, err := j.operations.write(j.file, line)
		if err == nil && written != len(line) {
			err = io.ErrShortWrite
		}
		if err != nil {
			j.recordFailure(JournalWrite, err)
		}
	}
}

func (j *Journal) rotateIfNeeded(next int64) error {
	info, err := j.operations.stat(j.file)
	if err != nil {
		return &JournalError{Operation: JournalStat, Err: err}
	}
	if info.Size()+next <= j.options.MaxBytes {
		return nil
	}

	if j.options.MaxFiles == 1 {
		file, err := openJournal(j.path, os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_WRONLY, j.operations)
		if err != nil {
			return &JournalError{Operation: JournalReopen, Err: err}
		}
		previous := j.file
		j.file = file
		if err := j.operations.close(previous); err != nil {
			return &JournalError{Operation: JournalClose, Err: err}
		}
		return nil
	}

	for index := j.options.MaxFiles - 1; index >= 1; index-- {
		destination := fmt.Sprintf("%s.%d", j.path, index)
		if index == j.options.MaxFiles-1 {
			if err := j.operations.remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
				return &JournalError{Operation: JournalRename, Err: err}
			}
		}
		source := j.path
		if index > 1 {
			source = fmt.Sprintf("%s.%d", j.path, index-1)
		}
		if err := j.operations.rename(source, destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return &JournalError{Operation: JournalRename, Err: err}
		}
	}

	file, err := openJournal(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, j.operations)
	if err != nil {
		// The old descriptor remains writable after rename. Restore its active
		// name when possible; a later event will retry rotation either way.
		rollbackErr := j.operations.rename(j.path+".1", j.path)
		return &JournalError{Operation: JournalReopen, Err: errors.Join(err, rollbackErr)}
	}
	previous := j.file
	j.file = file
	if err := j.operations.close(previous); err != nil {
		return &JournalError{Operation: JournalClose, Err: err}
	}
	return nil
}

func (j *Journal) recordFailure(operation JournalOperation, err error) {
	if err != nil {
		j.recordError(&JournalError{Operation: operation, Err: err})
	}
}

func (j *Journal) recordError(err error) {
	if err == nil {
		return
	}
	j.errorMu.Lock()
	if j.firstErr != nil {
		j.errorMu.Unlock()
		return
	}
	j.firstErr = err
	j.errorMu.Unlock()

	if j.options.OnFailure != nil {
		operation := JournalOperation("")
		var journalErr *JournalError
		if errors.As(err, &journalErr) {
			operation = journalErr.Operation
		}
		func() {
			defer func() { _ = recover() }()
			j.options.OnFailure(operation)
		}()
	}
}

func openJournal(path string, flags int, operations *journalOperations) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("création du répertoire du journal: %w", err)
	}
	file, err := operations.open(path, flags)
	if err != nil {
		return nil, fmt.Errorf("ouverture du journal de session: %w", err)
	}
	return file, nil
}
