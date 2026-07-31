package events

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type JournalOptions struct {
	MaxBytes int64
	MaxFiles int
}

type Journal struct {
	path        string
	options     JournalOptions
	file        *os.File
	stream      <-chan Event
	unsubscribe func()
	done        chan struct{}
	once        sync.Once
}

func NewJournal(bus *Bus, path string, options JournalOptions) (*Journal, error) {
	if bus == nil {
		return nil, fmt.Errorf("bus d'événements absent")
	}
	if options.MaxBytes < 1 || options.MaxFiles < 1 {
		return nil, fmt.Errorf("limites du journal invalides")
	}
	file, err := openJournal(path)
	if err != nil {
		return nil, err
	}
	stream, unsubscribe := bus.Subscribe(256)
	journal := &Journal{
		path: path, options: options, file: file,
		stream: stream, unsubscribe: unsubscribe, done: make(chan struct{}),
	}
	go journal.run()
	return journal, nil
}

func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	j.once.Do(j.unsubscribe)
	<-j.done
	return j.file.Close()
}

func (j *Journal) run() {
	defer close(j.done)
	for event := range j.stream {
		line, err := json.Marshal(event)
		if err != nil {
			continue
		}
		line = append(line, '\n')
		if err := j.rotateIfNeeded(int64(len(line))); err != nil {
			continue
		}
		_, _ = j.file.Write(line)
	}
}

func (j *Journal) rotateIfNeeded(next int64) error {
	info, err := j.file.Stat()
	if err != nil || info.Size()+next <= j.options.MaxBytes {
		return err
	}
	if err := j.file.Close(); err != nil {
		return err
	}
	for index := j.options.MaxFiles - 1; index >= 1; index-- {
		destination := fmt.Sprintf("%s.%d", j.path, index)
		if index == j.options.MaxFiles-1 {
			_ = os.Remove(destination)
		}
		source := j.path
		if index > 1 {
			source = fmt.Sprintf("%s.%d", j.path, index-1)
		}
		if err := os.Rename(source, destination); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if j.options.MaxFiles == 1 {
		_ = os.Remove(j.path)
	}
	file, err := openJournal(j.path)
	if err != nil {
		return err
	}
	j.file = file
	return nil
}

func openJournal(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("création du répertoire du journal: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ouverture du journal de session: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
