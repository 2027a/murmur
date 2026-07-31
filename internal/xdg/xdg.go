package xdg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const appName = "murmur"

// StateHome returns the absolute XDG state base without creating it.
func StateHome() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("résolution du répertoire d'état: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("XDG_STATE_HOME doit être absolu")
	}
	return filepath.Clean(base), nil
}

// ConfigDir returns Murmur's XDG configuration directory.
func ConfigDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		var err error
		base, err = os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("résolution du répertoire de configuration: %w", err)
		}
	}
	if err := validateOrCreateBase(base); err != nil {
		return "", fmt.Errorf("répertoire de configuration non sûr: %w", err)
	}
	path := filepath.Join(base, appName)
	if err := createSecureConfigDir(path); err != nil {
		return "", fmt.Errorf("répertoire de configuration Murmur non sûr: %w", err)
	}
	return path, nil
}

func createSecureConfigDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("création atomique impossible: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	uid, err := ownerUID(info)
	if err != nil {
		return err
	}
	if uid != uint32(os.Geteuid()) {
		return fmt.Errorf("le chemin n'appartient pas à l'utilisateur courant")
	}
	if info.Mode()&os.ModeSymlink == 0 {
		if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("type, propriétaire ou permissions invalides")
		}
		return nil
	}

	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("résolution du lien symbolique impossible: %w", err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return err
	}
	targetUID, err := ownerUID(targetInfo)
	if err != nil {
		return err
	}
	if !targetInfo.IsDir() || targetUID != uint32(os.Geteuid()) || targetInfo.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("cible du lien: type, propriétaire ou permissions invalides")
	}
	return nil
}

// RuntimeDir returns Murmur's deterministic private runtime directory. The
// system fallback parent must already exist; only the Murmur child is created.
func RuntimeDir() (string, error) {
	euid := uint32(os.Geteuid())
	systemBase := filepath.Join("/run/user", strconv.FormatUint(uint64(euid), 10))
	return resolveRuntimeDir(os.Getenv("XDG_RUNTIME_DIR"), systemBase, euid)
}

func resolveRuntimeDir(configuredBase, systemBase string, expectedUID uint32) (string, error) {
	base := configuredBase
	createBase := true
	if base == "" {
		base = systemBase
		createBase = false
	}
	parent, err := openPrivateDir(base, nil, createBase, expectedUID)
	if err != nil {
		if configuredBase == "" {
			return "", fmt.Errorf("XDG_RUNTIME_DIR absent et runtime système %q non sûr ou indisponible: %w; définir XDG_RUNTIME_DIR vers un répertoire privé absolu en 0700", base, err)
		}
		return "", fmt.Errorf("répertoire runtime parent non sûr: %w", err)
	}
	defer parent.Close()
	path, err := parent.ensurePrivateChild(appName)
	if err != nil {
		return "", fmt.Errorf("répertoire runtime Murmur non sûr: %w", err)
	}
	return path, nil
}

func validateOrCreateBase(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("le chemin doit être absolu")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("création atomique impossible: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	uid, err := ownerUID(info)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("le chemin n'est pas un répertoire réel")
	}
	if uid != uint32(os.Geteuid()) {
		return fmt.Errorf("le répertoire n'appartient pas à l'utilisateur courant")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("le répertoire est modifiable par le groupe ou les autres")
	}
	return nil
}

func ownerUID(info os.FileInfo) (uint32, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("propriétaire du chemin indéterminable")
	}
	return stat.Uid, nil
}

// WriteFileAtomic replaces path only after all data has been written and synced.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("création du fichier temporaire: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	if err = tmp.Chmod(perm); err != nil {
		return fmt.Errorf("permissions du fichier temporaire: %w", err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("écriture du fichier temporaire: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("synchronisation du fichier temporaire: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("fermeture du fichier temporaire: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("remplacement atomique de %s: %w", path, err)
	}

	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("ouverture du répertoire parent: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("synchronisation du répertoire parent: %w", err)
	}
	return nil
}
