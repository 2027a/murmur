package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

type Lock struct {
	mu   sync.Mutex
	file *os.File
}

func AcquireLock(runtimeDir string) (*Lock, error) {
	path := filepath.Join(runtimeDir, "session.lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, errs.Wrap(errs.Unavailable, "session-lock", "ouverture du verrou impossible", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := validateLockDescriptor(fd); err != nil {
		file.Close()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errs.New(errs.Unavailable, "session-lock", "une session est déjà active")
		}
		return nil, errs.Wrap(errs.Unavailable, "session-lock", "acquisition du verrou impossible", err)
	}
	return &Lock{file: file}, nil
}

// AdoptLock takes ownership of an inherited descriptor and acquires its lock.
// expectedPath, when provided, must name the same file as the descriptor.
func AdoptLock(file *os.File, expectedPath ...string) (*Lock, error) {
	if file == nil {
		return nil, errs.New(errs.Internal, "session-lock", "descripteur de verrou hérité manquant")
	}
	syscall.CloseOnExec(int(file.Fd()))
	fail := func(err error) (*Lock, error) {
		_ = file.Close()
		return nil, err
	}
	if len(expectedPath) > 1 {
		return fail(errs.New(errs.Internal, "session-lock", "plusieurs chemins de verrou attendus"))
	}
	if err := validateLockDescriptor(int(file.Fd())); err != nil {
		return fail(err)
	}
	info, err := file.Stat()
	if err != nil {
		return fail(errs.Wrap(errs.Internal, "session-lock", "descripteur de verrou hérité invalide", err))
	}
	if len(expectedPath) == 1 {
		expected, err := os.Lstat(expectedPath[0])
		if err != nil {
			return fail(errs.Wrap(errs.Internal, "session-lock", "verrou de session attendu inaccessible", err))
		}
		if expected.Mode()&os.ModeSymlink != 0 || !expected.Mode().IsRegular() || !os.SameFile(info, expected) {
			return fail(errs.New(errs.Internal, "session-lock", "le descripteur hérité ne correspond pas au verrou de session attendu"))
		}
	} else {
		resolved, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", file.Fd()))
		if err != nil {
			return fail(errs.Wrap(errs.Internal, "session-lock", "cible du descripteur de verrou invérifiable", err))
		}
		if filepath.Base(resolved) != "session.lock" {
			return fail(errs.New(errs.Internal, "session-lock", "le descripteur hérité ne référence pas session.lock"))
		}
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fail(errs.New(errs.Unavailable, "session-lock", "le verrou de session hérité est détenu par un autre processus"))
		}
		return fail(errs.Wrap(errs.Internal, "session-lock", "reprise du verrou hérité impossible", err))
	}
	if _, err := file.Stat(); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return fail(errs.Wrap(errs.Internal, "session-lock", "descripteur de verrou hérité invalide", err))
	}
	return &Lock{file: file}, nil
}

func validateLockDescriptor(fd int) error {
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return errs.Wrap(errs.Internal, "session-lock", "inspection du verrou impossible", err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o777 != 0o600 || stat.Nlink != 1 {
		return errs.New(errs.Unavailable, "session-lock", "le verrou doit être un fichier régulier 0600 appartenant à l'utilisateur et sans lien supplémentaire")
	}
	return nil
}

// File returns the descriptor to pass through exec.Cmd.ExtraFiles.
func (l *Lock) File() *os.File {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file
}

// Detach closes this process's descriptor without explicitly unlocking it.
// The lock remains held by a descriptor inherited by the session process.
func (l *Lock) Detach() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *Lock) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
