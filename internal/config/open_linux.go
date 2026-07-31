//go:build linux

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type configFileIdentity struct {
	device uint64
	inode  uint64
	size   int64
	mtime  syscall.Timespec
	ctime  syscall.Timespec
}

const MaxConfigBytes = 1 << 20

var errUnsafePathComponent = errors.New("composant de chemin non sûr")

var configFstat = func(fd int) (syscall.Stat_t, error) {
	var stat syscall.Stat_t
	err := syscall.Fstat(fd, &stat)
	return stat, err
}

// openConfigFile follows configuration symlinks atomically, then validates the
// final descriptor. A second opening after decoding detects path retargeting.
func openConfigFile(path string) (*os.File, configFileIdentity, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, configFileIdentity{}, fmt.Errorf("résolution du chemin de configuration: %w", err)
	}
	fd, err := syscall.Open(absolute, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, configFileIdentity{}, fmt.Errorf("ouverture de la configuration: %w", err)
	}
	file := os.NewFile(uintptr(fd), absolute)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, configFileIdentity{}, fmt.Errorf("descripteur de configuration invalide")
	}
	stat, err := configFstat(int(file.Fd()))
	if err != nil {
		_ = file.Close()
		return nil, configFileIdentity{}, fmt.Errorf("inspection de la configuration: %w", err)
	}
	if err := validateConfigFileStat(stat); err != nil {
		_ = file.Close()
		return nil, configFileIdentity{}, err
	}
	return file, configIdentityOf(stat), nil
}

func openAbsoluteFileNoFollow(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("le chemin doit être absolu")
	}
	components := strings.Split(strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)), string(filepath.Separator))
	if len(components) == 0 || components[0] == "" {
		return nil, fmt.Errorf("chemin de fichier invalide")
	}

	dirFD, err := syscall.Open(string(filepath.Separator), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("ouverture de la racine: %w", err)
	}
	for _, component := range components[:len(components)-1] {
		nextFD, openErr := syscall.Openat(dirFD, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		_ = syscall.Close(dirFD)
		if openErr != nil {
			if errors.Is(openErr, syscall.ELOOP) || errors.Is(openErr, syscall.ENOTDIR) {
				return nil, fmt.Errorf("%w %q: %v", errUnsafePathComponent, component, openErr)
			}
			return nil, fmt.Errorf("ouverture du composant de chemin %q: %w", component, openErr)
		}
		dirFD = nextFD
	}

	name := components[len(components)-1]
	fd, openErr := syscall.Openat(dirFD, name, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	_ = syscall.Close(dirFD)
	if openErr != nil {
		if errors.Is(openErr, syscall.ELOOP) {
			return nil, fmt.Errorf("%w %q: %v", errUnsafePathComponent, name, openErr)
		}
		return nil, fmt.Errorf("ouverture de %q: %w", name, openErr)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("descripteur de fichier invalide")
	}
	return file, nil
}

func validateConfigFileStat(stat syscall.Stat_t) error {
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return fmt.Errorf("config.toml doit être un fichier régulier")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("config.toml n'appartient pas à l'utilisateur courant")
	}
	if stat.Mode&0o022 != 0 {
		return fmt.Errorf("config.toml est modifiable par le groupe ou les autres")
	}
	return nil
}

func configIdentityOf(stat syscall.Stat_t) configFileIdentity {
	return configFileIdentity{
		device: uint64(stat.Dev), inode: uint64(stat.Ino), size: stat.Size,
		mtime: stat.Mtim, ctime: stat.Ctim,
	}
}

func verifyConfigFile(file *os.File, path string, identity configFileIdentity) error {
	stat, err := configFstat(int(file.Fd()))
	if err != nil {
		return fmt.Errorf("réinspection de la configuration: %w", err)
	}
	if err := validateConfigFileStat(stat); err != nil {
		return err
	}
	current, currentIdentity, err := openConfigFile(path)
	if err != nil {
		return fmt.Errorf("réouverture de contrôle de config.toml: %w", err)
	}
	if err := current.Close(); err != nil {
		return fmt.Errorf("fermeture du contrôle de config.toml: %w", err)
	}
	if currentIdentity.device != identity.device || currentIdentity.inode != identity.inode {
		return fmt.Errorf("config.toml a été remplacé pendant son chargement")
	}
	if inspected := configIdentityOf(stat); inspected != identity {
		if inspected.device != identity.device || inspected.inode != identity.inode {
			return fmt.Errorf("inode de config.toml instable")
		}
		return fmt.Errorf("contenu de config.toml modifié pendant son chargement")
	}
	return nil
}
