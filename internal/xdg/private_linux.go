//go:build linux

package xdg

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type dirIdentity struct {
	device uint64
	inode  uint64
}

// PrivateDir is a private directory reached without following symbolic links.
// Its methods keep pathname-sensitive operations anchored to the validated fd.
type PrivateDir struct {
	file        *os.File
	base        string
	children    []string
	path        string
	expectedUID uint32
	identity    dirIdentity
}

// OpenPrivateDir opens or creates a private directory below base. Existing base
// and child directories must belong to the effective user and must not be
// writable by group or others. The final directory must already have mode 0700.
func OpenPrivateDir(base string, children ...string) (*PrivateDir, error) {
	return openPrivateDir(base, children, true, uint32(os.Geteuid()))
}

func openPrivateDir(base string, children []string, create bool, expectedUID uint32) (*PrivateDir, error) {
	if !filepath.IsAbs(base) {
		return nil, fmt.Errorf("le chemin de base doit être absolu")
	}
	base = filepath.Clean(base)
	baseComponents := pathComponents(base)
	if len(baseComponents) == 0 {
		return nil, fmt.Errorf("la racine ne peut pas être un répertoire privé")
	}
	components := append([]string(nil), baseComponents...)
	for _, child := range children {
		if child == "" || child == "." || child == ".." || filepath.Base(child) != child || strings.ContainsRune(child, filepath.Separator) {
			return nil, fmt.Errorf("composant privé invalide %q", child)
		}
		components = append(components, child)
	}

	dirFD, err := syscall.Open(string(filepath.Separator), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("ouverture de la racine: %w", err)
	}
	for index, component := range components {
		nextFD, openErr := syscall.Openat(dirFD, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		created := false
		if errors.Is(openErr, syscall.ENOENT) && create {
			mkdirErr := syscall.Mkdirat(dirFD, component, 0o700)
			if mkdirErr == nil {
				created = true
			} else if !errors.Is(mkdirErr, syscall.EEXIST) {
				_ = syscall.Close(dirFD)
				return nil, fmt.Errorf("création sûre de %q: %w", component, mkdirErr)
			}
			nextFD, openErr = syscall.Openat(dirFD, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		}
		_ = syscall.Close(dirFD)
		if openErr != nil {
			return nil, fmt.Errorf("composant de répertoire non sûr %q: %w", component, openErr)
		}

		managed := index >= len(baseComponents)-1
		final := index == len(components)-1
		stat, statErr := statFD(nextFD)
		if statErr == nil {
			if managed || created {
				statErr = validatePrivateDirStat(stat, expectedUID, final && !created)
			} else {
				statErr = validatePathAncestorStat(stat)
			}
		}
		if statErr == nil && created && stat.Mode&0o777 != 0o700 {
			statErr = syscall.Fchmod(nextFD, 0o700)
			if statErr == nil {
				stat, statErr = statFD(nextFD)
			}
			if statErr == nil {
				statErr = validatePrivateDirStat(stat, expectedUID, true)
			}
		}
		if statErr != nil {
			_ = syscall.Close(nextFD)
			return nil, fmt.Errorf("répertoire privé %q invalide: %w", component, statErr)
		}
		dirFD = nextFD
	}

	path := filepath.Join(append([]string{base}, children...)...)
	file := os.NewFile(uintptr(dirFD), path)
	if file == nil {
		_ = syscall.Close(dirFD)
		return nil, fmt.Errorf("descripteur de répertoire privé invalide")
	}
	stat, err := statFD(dirFD)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &PrivateDir{
		file: file, base: base, children: append([]string(nil), children...), path: path,
		expectedUID: expectedUID, identity: identityOf(stat),
	}, nil
}

func pathComponents(path string) []string {
	trimmed := strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, string(filepath.Separator))
}

func statFD(fd int) (syscall.Stat_t, error) {
	var stat syscall.Stat_t
	err := syscall.Fstat(fd, &stat)
	return stat, err
}

func validatePrivateDirStat(stat syscall.Stat_t, expectedUID uint32, exactMode bool) error {
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return fmt.Errorf("le chemin n'est pas un répertoire réel")
	}
	if stat.Uid != expectedUID {
		return fmt.Errorf("le répertoire n'appartient pas à l'utilisateur effectif")
	}
	if stat.Mode&0o022 != 0 {
		return fmt.Errorf("le répertoire est modifiable par le groupe ou les autres")
	}
	if exactMode && stat.Mode&0o777 != 0o700 {
		return fmt.Errorf("le répertoire final doit avoir le mode 0700")
	}
	return nil
}

func validatePathAncestorStat(stat syscall.Stat_t) error {
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return fmt.Errorf("le composant n'est pas un répertoire réel")
	}
	if stat.Mode&0o022 != 0 && stat.Mode&syscall.S_ISVTX == 0 {
		return fmt.Errorf("le composant est modifiable sans sticky bit")
	}
	return nil
}

func identityOf(stat syscall.Stat_t) dirIdentity {
	return dirIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}
}

func (d *PrivateDir) Path() string { return d.path }

func (d *PrivateDir) Close() error { return d.file.Close() }

// Lock serializes a complete store transaction with other processes holding the
// same directory inode. Callers must pair it with Unlock.
func (d *PrivateDir) Lock() error {
	return syscall.Flock(int(d.file.Fd()), syscall.LOCK_EX)
}

func (d *PrivateDir) Unlock() error {
	return syscall.Flock(int(d.file.Fd()), syscall.LOCK_UN)
}

// ensurePrivateChild creates or opens one private child while staying anchored
// to the already validated parent descriptor.
func (d *PrivateDir) ensurePrivateChild(name string) (string, error) {
	if !safeNamePart(name) {
		return "", fmt.Errorf("nom de répertoire privé invalide")
	}
	created := false
	if err := syscall.Mkdirat(int(d.file.Fd()), name, 0o700); err == nil {
		created = true
	} else if !errors.Is(err, syscall.EEXIST) {
		return "", fmt.Errorf("création sûre de %q: %w", name, err)
	}
	fd, err := syscall.Openat(int(d.file.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("ouverture sûre de %q: %w", name, err)
	}
	defer syscall.Close(fd)
	stat, err := statFD(fd)
	if err == nil && created && stat.Mode&0o777 != 0o700 {
		err = syscall.Fchmod(fd, 0o700)
		if err == nil {
			stat, err = statFD(fd)
		}
	}
	if err == nil {
		err = validatePrivateDirStat(stat, d.expectedUID, true)
	}
	if err != nil {
		return "", fmt.Errorf("répertoire privé %q invalide: %w", name, err)
	}
	return filepath.Join(d.path, name), nil
}

// Verify checks both the held directory and the current no-symlink path.
func (d *PrivateDir) Verify() error {
	stat, err := statFD(int(d.file.Fd()))
	if err != nil {
		return fmt.Errorf("inspection du store privé: %w", err)
	}
	if err := validatePrivateDirStat(stat, d.expectedUID, true); err != nil {
		return err
	}
	if identityOf(stat) != d.identity {
		return fmt.Errorf("inode du store privé instable")
	}
	current, err := openPrivateDir(d.base, d.children, false, d.expectedUID)
	if err != nil {
		return fmt.Errorf("réouverture du store privé: %w", err)
	}
	defer current.Close()
	if current.identity != d.identity {
		return fmt.Errorf("store privé remplacé pendant l'opération")
	}
	return nil
}

// CreateTemp creates a new regular file in the validated directory with mode
// 0600. The random name and O_EXCL avoid ever chmodding an existing target.
func (d *PrivateDir) CreateTemp(prefix, suffix string) (*os.File, string, error) {
	if !safeNamePart(prefix) || !safeNamePart(suffix) {
		return nil, "", fmt.Errorf("patron de fichier privé invalide")
	}
	for attempts := 0; attempts < 100; attempts++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", fmt.Errorf("génération du nom privé: %w", err)
		}
		name := prefix + hex.EncodeToString(random[:]) + suffix
		fd, err := syscall.Openat(int(d.file.Fd()), name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
		if errors.Is(err, syscall.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		stat, statErr := statFD(fd)
		if statErr == nil {
			statErr = validatePrivateFileStat(stat, d.expectedUID)
		}
		if statErr == nil && stat.Mode&0o777 != 0o600 {
			statErr = syscall.Fchmod(fd, 0o600)
			if statErr == nil {
				stat, statErr = statFD(fd)
			}
			if statErr == nil {
				statErr = validatePrivateFileStat(stat, d.expectedUID)
			}
		}
		if statErr != nil || stat.Mode&0o777 != 0o600 {
			_ = syscall.Close(fd)
			_ = syscall.Unlinkat(int(d.file.Fd()), name)
			if statErr == nil {
				statErr = fmt.Errorf("le fichier privé doit avoir le mode 0600")
			}
			return nil, "", statErr
		}
		file := os.NewFile(uintptr(fd), filepath.Join(d.path, name))
		if file == nil {
			_ = syscall.Close(fd)
			_ = syscall.Unlinkat(int(d.file.Fd()), name)
			return nil, "", fmt.Errorf("descripteur de fichier privé invalide")
		}
		return file, name, nil
	}
	return nil, "", fmt.Errorf("impossible d'allouer un nom de fichier privé")
}

func safeNamePart(value string) bool {
	return value == filepath.Base(value) && value != "." && value != ".." && !strings.ContainsRune(value, filepath.Separator)
}

func validatePrivateFileStat(stat syscall.Stat_t, expectedUID uint32) error {
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 {
		return fmt.Errorf("le fichier privé n'est pas un fichier régulier unique")
	}
	if stat.Uid != expectedUID {
		return fmt.Errorf("le fichier privé n'appartient pas à l'utilisateur effectif")
	}
	if stat.Mode&0o077 != 0 {
		return fmt.Errorf("le fichier privé est accessible au groupe ou aux autres")
	}
	return nil
}

// ReadDir returns entries from a fresh descriptor anchored to the validated
// directory, leaving its iteration state untouched.
func (d *PrivateDir) ReadDir() ([]os.DirEntry, error) {
	fd, err := syscall.Openat(int(d.file.Fd()), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	duplicate := os.NewFile(uintptr(fd), d.path)
	if duplicate == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("duplication du store privé impossible")
	}
	defer duplicate.Close()
	return duplicate.ReadDir(-1)
}

// OpenFile opens and validates an existing retained file without following it.
func (d *PrivateDir) OpenFile(name string) (*os.File, error) {
	if !safeNamePart(name) {
		return nil, fmt.Errorf("nom de fichier privé invalide")
	}
	fd, err := syscall.Openat(int(d.file.Fd()), name, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	stat, err := statFD(fd)
	if err == nil {
		err = validatePrivateFileStat(stat, d.expectedUID)
	}
	if err == nil && stat.Mode&0o777 != 0o600 {
		err = fmt.Errorf("le fichier privé doit avoir le mode 0600")
	}
	if err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(d.path, name))
	if file == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("descripteur de fichier privé invalide")
	}
	return file, nil
}

func (d *PrivateDir) Remove(name string) error {
	if !safeNamePart(name) {
		return fmt.Errorf("nom de fichier privé invalide")
	}
	return syscall.Unlinkat(int(d.file.Fd()), name)
}

func (d *PrivateDir) Sync() error { return d.file.Sync() }
