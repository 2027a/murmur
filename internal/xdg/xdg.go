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
	if err := validateOrCreateBase(base, false); err != nil {
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

// RuntimeDir returns a private runtime directory, creating it when necessary.
func RuntimeDir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	fallback := base == ""
	if base == "" {
		base = os.TempDir()
	}
	if err := validateOrCreateBase(base, fallback); err != nil {
		return "", fmt.Errorf("répertoire runtime parent non sûr: %w", err)
	}
	if !fallback {
		info, err := os.Lstat(base)
		if err != nil || info.Mode().Perm() != 0o700 {
			return "", fmt.Errorf("répertoire runtime parent non sûr: permissions 0700 attendues")
		}
	}
	name := appName
	if fallback {
		name += "-" + strconv.Itoa(os.Getuid())
	}
	path := filepath.Join(base, name)
	if err := createSecureDir(path, true); err != nil {
		return "", fmt.Errorf("répertoire runtime Murmur non sûr: %w", err)
	}
	return path, nil
}

func validateOrCreateBase(path string, sharedTemporary bool) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("le chemin doit être absolu")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !sharedTemporary {
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
	if sharedTemporary {
		if uid != uint32(os.Geteuid()) && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("le répertoire temporaire partagé n'est pas protégé par sticky bit")
		}
		if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("le répertoire temporaire est modifiable sans sticky bit")
		}
		return nil
	}
	if uid != uint32(os.Geteuid()) {
		return fmt.Errorf("le répertoire n'appartient pas à l'utilisateur courant")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("le répertoire est modifiable par le groupe ou les autres")
	}
	return nil
}

func createSecureDir(path string, private bool) error {
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
	invalidMode := info.Mode().Perm()&0o022 != 0
	if private {
		invalidMode = info.Mode().Perm() != 0o700
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || uid != uint32(os.Geteuid()) || invalidMode {
		return fmt.Errorf("type, propriétaire ou permissions invalides")
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
