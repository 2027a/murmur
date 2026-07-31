package audio

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

const (
	defaultDiagnosticFiles = 10
	defaultDiagnosticBytes = 256 << 20
)

type RetentionOptions struct {
	Keep      bool
	Directory string
	MaxFiles  int
	MaxBytes  int64
}

func (r RetentionOptions) validate() error {
	if r.MaxFiles < 0 || r.MaxBytes < 0 {
		return errs.New(errs.ConfigInvalid, "audio", "les limites de rétention ne peuvent pas être négatives")
	}
	return nil
}

func (r RetentionOptions) normalized() (RetentionOptions, error) {
	if err := r.validate(); err != nil {
		return RetentionOptions{}, err
	}
	if !r.Keep {
		return r, nil
	}
	if r.Directory == "" {
		state := os.Getenv("XDG_STATE_HOME")
		if state == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return RetentionOptions{}, errs.Wrap(errs.AudioFailed, "audio/retention", "impossible de résoudre le répertoire de diagnostic", err)
			}
			state = filepath.Join(home, ".local", "state")
		}
		r.Directory = filepath.Join(state, "murmur", "recordings")
	}
	if r.MaxFiles == 0 {
		r.MaxFiles = defaultDiagnosticFiles
	}
	if r.MaxBytes == 0 {
		r.MaxBytes = defaultDiagnosticBytes
	}
	return r, nil
}

func preserveOrRemove(path, tempDir string, options RetentionOptions) (string, error) {
	options, err := options.normalized()
	if err != nil {
		_ = removeCapture(tempDir)
		return "", err
	}
	if !options.Keep {
		return "", removeCapture(tempDir)
	}
	info, err := os.Stat(path)
	if err != nil {
		_ = removeCapture(tempDir)
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", errs.Wrap(errs.AudioFailed, "audio/retention", "impossible d'inspecter la capture", err)
	}
	if info.Size() > options.MaxBytes {
		_ = removeCapture(tempDir)
		return "", errs.New(errs.AudioFailed, "audio/retention", "capture trop volumineuse pour la limite de diagnostic")
	}
	if err := os.MkdirAll(options.Directory, 0o700); err != nil {
		_ = removeCapture(tempDir)
		return "", errs.Wrap(errs.AudioFailed, "audio/retention", "impossible de créer le répertoire de diagnostic", err)
	}
	if err := os.Chmod(options.Directory, 0o700); err != nil {
		_ = removeCapture(tempDir)
		return "", errs.Wrap(errs.AudioFailed, "audio/retention", "permissions du répertoire de diagnostic invalides", err)
	}
	destination, err := copyPrivate(path, options.Directory)
	if err != nil {
		_ = removeCapture(tempDir)
		return "", errs.Wrap(errs.AudioFailed, "audio/retention", "impossible de conserver la capture de diagnostic", err)
	}
	_ = removeCapture(tempDir)
	if err := pruneDiagnostics(options); err != nil {
		return destination, err
	}
	return destination, nil
}

func RetainCapture(path, tempDir string, options RetentionOptions) (string, error) {
	return preserveOrRemove(path, tempDir, options)
}

func copyPrivate(source, directory string) (string, error) {
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(directory, "recording-*.wav")
	if err != nil {
		return "", err
	}
	destination := out.Name()
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return "", err
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(source); err != nil {
		return "", err
	}
	ok = true
	return destination, nil
}

type diagnosticFile struct {
	path    string
	size    int64
	modTime time.Time
}

func pruneDiagnostics(options RetentionOptions) error {
	entries, err := os.ReadDir(options.Directory)
	if err != nil {
		return errs.Wrap(errs.AudioFailed, "audio/retention", "impossible de borner les diagnostics", err)
	}
	files := make([]diagnosticFile, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "recording-") || !strings.HasSuffix(entry.Name(), ".wav") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return errs.Wrap(errs.AudioFailed, "audio/retention", "impossible d'inspecter les diagnostics", err)
		}
		file := diagnosticFile{path: filepath.Join(options.Directory, entry.Name()), size: info.Size(), modTime: info.ModTime()}
		files = append(files, file)
		total += file.size
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].path < files[j].path
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	for len(files) > options.MaxFiles || total > options.MaxBytes {
		victim := files[0]
		files = files[1:]
		if err := os.Remove(victim.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errs.Wrap(errs.AudioFailed, "audio/retention", "impossible de supprimer un ancien diagnostic", err)
		}
		total -= victim.size
	}
	return nil
}
