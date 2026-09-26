package audio

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"git.2027a.net/2027a/murmur/internal/audio/wav"
	"git.2027a.net/2027a/murmur/internal/errs"
	"git.2027a.net/2027a/murmur/internal/xdg"
)

const (
	defaultDiagnosticFiles = 10
	defaultDiagnosticBytes = 256 << 20
)

var (
	errRetentionSourceChanged   = errors.New("capture source remplacée pendant la rétention")
	beforeRetentionSourceRemove = func() {}
)

type retentionSourceIdentity struct {
	device uint64
	inode  uint64
}

type RetentionOptions struct {
	Keep      bool
	Directory string
	MaxFiles  int
	MaxBytes  int64
	storeBase string
	storePath []string
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
		state, err := xdg.StateHome()
		if err != nil {
			return RetentionOptions{}, errs.Wrap(errs.AudioFailed, "audio/retention", "impossible de résoudre le répertoire de diagnostic", err)
		}
		r.storeBase = state
		r.storePath = []string{"murmur", "recordings"}
		r.Directory = filepath.Join(append([]string{state}, r.storePath...)...)
	} else {
		if !filepath.IsAbs(r.Directory) {
			return RetentionOptions{}, errs.New(errs.AudioFailed, "audio/retention", "le répertoire de diagnostic doit être absolu")
		}
		r.Directory = filepath.Clean(r.Directory)
		r.storeBase = filepath.Dir(r.Directory)
		r.storePath = []string{filepath.Base(r.Directory)}
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
	store, err := xdg.OpenPrivateDir(options.storeBase, options.storePath...)
	if err != nil {
		_ = removeCapture(tempDir)
		return "", errs.Wrap(errs.AudioFailed, "audio/retention", "répertoire de diagnostic non sûr", err)
	}
	defer store.Close()
	name, err := retainInStore(path, store, options)
	if err != nil {
		if !errors.Is(err, errRetentionSourceChanged) {
			_ = removeCapture(tempDir)
		}
		return "", errs.Wrap(errs.AudioFailed, "audio/retention", "impossible de conserver la capture de diagnostic", err)
	}
	_ = removeCapture(tempDir)
	return filepath.Join(store.Path(), name), nil
}

func RetainCapture(path, tempDir string, options RetentionOptions) (string, error) {
	return preserveOrRemove(path, tempDir, options)
}

func preserveValidatedOrRemove(file wav.File, tempDir string, options RetentionOptions) (string, error) {
	reader, handle, openErr := file.OpenRIFF()
	if openErr != nil {
		closeErr := file.Close()
		removeErr := removeCapture(tempDir)
		return "", errors.Join(openErr, closeErr, removeErr)
	}
	options, normalizeErr := options.normalized()
	if normalizeErr != nil {
		closeErr := handle.Close()
		fileCloseErr := file.Close()
		removeErr := removeCapture(tempDir)
		return "", errors.Join(normalizeErr, closeErr, fileCloseErr, removeErr)
	}
	store, storeErr := xdg.OpenPrivateDir(options.storeBase, options.storePath...)
	if storeErr != nil {
		closeErr := handle.Close()
		fileCloseErr := file.Close()
		removeErr := removeCapture(tempDir)
		return "", errors.Join(errs.Wrap(errs.AudioFailed, "audio/retention", "répertoire de diagnostic non sûr", storeErr), closeErr, fileCloseErr, removeErr)
	}
	name, retainErr := retainReaderInStore(reader, store, options)
	storeCloseErr := store.Close()
	handleCloseErr := handle.Close()
	fileCloseErr := file.Close()
	if retainErr != nil {
		removeErr := removeCapture(tempDir)
		return "", errors.Join(errs.Wrap(errs.AudioFailed, "audio/retention", "impossible de conserver la capture de diagnostic", retainErr), storeCloseErr, handleCloseErr, fileCloseErr, removeErr)
	}
	removeErr := removeCapture(tempDir)
	return filepath.Join(store.Path(), name), errors.Join(storeCloseErr, handleCloseErr, fileCloseErr, removeErr)
}

func retainInStore(source string, store *xdg.PrivateDir, options RetentionOptions) (name string, err error) {
	in, identity, err := openRetentionSource(source, options.MaxBytes)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	if err := store.Lock(); err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, store.Unlock()) }()

	name, err = copyPrivate(in, store, options.MaxBytes)
	if err != nil {
		return "", err
	}
	createdName := name
	keep := false
	defer func() {
		if !keep {
			_ = store.Remove(createdName)
		}
	}()
	if err := pruneDiagnostics(store, options, name); err != nil {
		return "", err
	}
	if err := store.Sync(); err != nil {
		return "", err
	}
	if err := store.Verify(); err != nil {
		return "", err
	}
	if err := removeRetentionSource(source, identity); err != nil {
		return "", err
	}
	if err := store.Verify(); err != nil {
		return "", err
	}
	keep = true
	return name, nil
}

func retainReaderInStore(reader io.Reader, store *xdg.PrivateDir, options RetentionOptions) (name string, err error) {
	if err := store.Lock(); err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, store.Unlock()) }()
	name, err = copyPrivate(reader, store, options.MaxBytes)
	if err != nil {
		return "", err
	}
	createdName := name
	keep := false
	defer func() {
		if !keep {
			_ = store.Remove(createdName)
		}
	}()
	if err := pruneDiagnostics(store, options, name); err != nil {
		return "", err
	}
	if err := store.Sync(); err != nil {
		return "", err
	}
	if err := store.Verify(); err != nil {
		return "", err
	}
	keep = true
	return name, nil
}

func openRetentionSource(path string, maximum int64) (*os.File, retentionSourceIdentity, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, retentionSourceIdentity{}, err
	}
	in := os.NewFile(uintptr(fd), path)
	if in == nil {
		_ = syscall.Close(fd)
		return nil, retentionSourceIdentity{}, fmt.Errorf("descripteur de capture invalide")
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		_ = in.Close()
		return nil, retentionSourceIdentity{}, err
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		_ = in.Close()
		return nil, retentionSourceIdentity{}, fmt.Errorf("la capture à retenir doit être un fichier régulier")
	}
	if stat.Size > maximum {
		_ = in.Close()
		return nil, retentionSourceIdentity{}, errs.New(errs.AudioFailed, "audio/retention", "capture trop volumineuse pour la limite de diagnostic")
	}
	return in, retentionSourceIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, nil
}

func removeRetentionSource(path string, expected retentionSourceIdentity) error {
	beforeRetentionSourceRemove()
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return errors.Join(errRetentionSourceChanged, err)
	}
	var stat syscall.Stat_t
	statErr := syscall.Fstat(fd, &stat)
	closeErr := syscall.Close(fd)
	if statErr != nil || closeErr != nil {
		return errors.Join(errRetentionSourceChanged, statErr, closeErr)
	}
	current := retentionSourceIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}
	if current != expected {
		return errRetentionSourceChanged
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return nil
}

func copyPrivate(in io.Reader, store *xdg.PrivateDir, maximum int64) (string, error) {
	out, name, err := store.CreateTemp("recording-", ".wav")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = store.Remove(name)
		}
	}()
	limit := maximum
	if limit < math.MaxInt64 {
		limit++
	}
	written, err := io.Copy(out, io.LimitReader(in, limit))
	if err != nil {
		return "", err
	}
	if written > maximum {
		return "", errs.New(errs.AudioFailed, "audio/retention", "capture trop volumineuse pour la limite de diagnostic")
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

type diagnosticFile struct {
	name    string
	size    int64
	modTime time.Time
}

func pruneDiagnostics(store *xdg.PrivateDir, options RetentionOptions, current string) error {
	entries, err := store.ReadDir()
	if err != nil {
		return errs.Wrap(errs.AudioFailed, "audio/retention", "impossible de borner les diagnostics", err)
	}
	files := make([]diagnosticFile, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "recording-") || !strings.HasSuffix(entry.Name(), ".wav") {
			continue
		}
		file, err := store.OpenFile(entry.Name())
		if err != nil {
			return errs.Wrap(errs.AudioFailed, "audio/retention", "impossible d'inspecter les diagnostics", err)
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil {
			return errs.Wrap(errs.AudioFailed, "audio/retention", "impossible d'inspecter les diagnostics", errors.Join(statErr, closeErr))
		}
		diagnostic := diagnosticFile{name: entry.Name(), size: info.Size(), modTime: info.ModTime()}
		files = append(files, diagnostic)
		if diagnostic.size > math.MaxInt64-total {
			total = math.MaxInt64
		} else {
			total += diagnostic.size
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].name == current || files[j].name == current {
			return files[j].name == current
		}
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].name < files[j].name
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	for len(files) > options.MaxFiles || total > options.MaxBytes {
		if len(files) == 0 || files[0].name == current {
			return errs.New(errs.AudioFailed, "audio/retention", "impossible de respecter les limites sans supprimer la capture courante")
		}
		victim := files[0]
		files = files[1:]
		if err := store.Remove(victim.name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errs.Wrap(errs.AudioFailed, "audio/retention", "impossible de supprimer un ancien diagnostic", err)
		}
		total -= victim.size
	}
	return nil
}
