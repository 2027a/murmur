package xdg

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestStateHomeRejectsRelativeBase(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/state")
	if _, err := StateHome(); err == nil {
		t.Fatal("relative XDG_STATE_HOME accepted")
	}
}

func TestOpenPrivateDirCreatesPrivateChain(t *testing.T) {
	root := privateTempDir(t)
	base := filepath.Join(root, "state", "nested")
	dir, err := OpenPrivateDir(base, "murmur", "recordings")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	for _, path := range []string{filepath.Dir(base), base, filepath.Join(base, "murmur"), dir.Path()} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			t.Fatalf("répertoire créé %q = %v", path, info.Mode())
		}
	}
	file, name, err := dir.CreateTemp("recording-", ".wav")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(dir.Path(), name))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("fichier privé = %v", info.Mode())
	}
}

func TestOpenPrivateDirRejectsSymlinkAtEveryLevel(t *testing.T) {
	for _, test := range []struct {
		name      string
		directory func(root, link string) string
	}{
		{name: "intermediate", directory: func(root, link string) string { return filepath.Join(root, link, "recordings") }},
		{name: "final", directory: func(root, link string) string { return filepath.Join(root, link) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			target := t.TempDir()
			if err := os.Chmod(target, 0o750); err != nil {
				t.Fatal(err)
			}
			link := "linked"
			if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenPrivateDir(test.directory(root, link)); err == nil {
				t.Fatal("symbolic link accepted")
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o750 {
				t.Fatalf("target mode changed to %o", info.Mode().Perm())
			}
		})
	}
}

func TestOpenPrivateDirRejectsModeAndInjectedOwnerWithoutChangingMode(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPrivateDir(directory); err == nil {
		t.Fatal("group-writable directory accepted")
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o770 {
		t.Fatalf("directory mode changed to %o", info.Mode().Perm())
	}

	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	wrongUID := uint32(os.Geteuid()) + 1
	if _, err := openPrivateDir(directory, nil, false, wrongUID); err == nil {
		t.Fatal("directory owned by another injected UID accepted")
	}
}

func TestPrivateDirDetectsPathSubstitutionAndStaysAnchored(t *testing.T) {
	root := privateTempDir(t)
	path := filepath.Join(root, "recordings")
	dir, err := OpenPrivateDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	moved := filepath.Join(root, "validated")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement")
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, path); err != nil {
		t.Fatal(err)
	}
	file, name, err := dir.CreateTemp("recording-", ".wav")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, name)); err != nil {
		t.Fatalf("anchored file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(replacement, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file escaped into replacement: %v", err)
	}
	if err := dir.Verify(); err == nil {
		t.Fatal("path substitution not detected")
	}
}

func TestConfigDir(t *testing.T) {
	t.Run("XDG_CONFIG_HOME", func(t *testing.T) {
		base := privateTempDir(t)
		t.Setenv("XDG_CONFIG_HOME", base)
		got, err := ConfigDir()
		if err != nil || got != filepath.Join(base, "murmur") {
			t.Fatalf("ConfigDir() = %q, %v", got, err)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", t.TempDir())
		got, err := ConfigDir()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(got, filepath.Join(".config", "murmur")) {
			t.Fatalf("ConfigDir() = %q", got)
		}
	})
}

func TestRuntimeDirRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", base)
	if err := os.Symlink(t.TempDir(), filepath.Join(base, "murmur")); err != nil {
		t.Fatal(err)
	}
	if _, err := RuntimeDir(); err == nil {
		t.Fatal("runtime symlink accepted")
	}
}

func TestRuntimeDirRejectsSymlinkedParentComponent(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRuntimeDir(filepath.Join(link, "runtime"), "", uint32(os.Geteuid())); err == nil {
		t.Fatal("symlinked runtime parent component accepted")
	}
}

func TestRuntimeDirRejectsWritableParentComponent(t *testing.T) {
	root := t.TempDir()
	unsafe := filepath.Join(root, "unsafe")
	base := filepath.Join(unsafe, "runtime")
	if err := os.Mkdir(unsafe, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRuntimeDir(base, "", uint32(os.Geteuid())); err == nil {
		t.Fatal("runtime below writable parent component accepted")
	}
}

func TestConfigDirAcceptsSafeSymlink(t *testing.T) {
	base := privateTempDir(t)
	t.Setenv("XDG_CONFIG_HOME", base)
	target := t.TempDir()
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "murmur")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	got, err := ConfigDir()
	if err != nil || got != path {
		t.Fatalf("ConfigDir() = %q, %v", got, err)
	}
}

func TestConfigDirRejectsUnsafeSymlinkTarget(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	target := t.TempDir()
	if err := os.Chmod(target, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(base, "murmur")); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigDir(); err == nil {
		t.Fatal("unsafe config symlink target accepted")
	}
}

func TestPrivateDirsRejectUnsafeModes(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", base)
	path := filepath.Join(base, "murmur")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RuntimeDir(); err == nil {
		t.Fatal("runtime directory with mode 0755 accepted")
	}

	configBase := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configBase)
	configPath := filepath.Join(configBase, "murmur")
	if err := os.Mkdir(configPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configPath, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigDir(); err == nil {
		t.Fatal("configuration directory with mode 0777 accepted")
	}
}

func TestRuntimeDirIgnoresSquattedLegacyFallback(t *testing.T) {
	root := privateTempDir(t)
	legacyParent := filepath.Join(root, "tmp")
	if err := os.Mkdir(legacyParent, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(legacyParent, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	victimUID := uint32(os.Geteuid())
	legacy := filepath.Join(legacyParent, "murmur-"+strconv.FormatUint(uint64(victimUID), 10))
	if err := os.Mkdir(legacy, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(legacy, 0o777); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(legacy)
	if err != nil {
		t.Fatal(err)
	}
	systemBase := filepath.Join(root, "run-user-victim")
	if err := os.Mkdir(systemBase, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", legacyParent)
	dir, err := resolveRuntimeDir("", systemBase, victimUID)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(systemBase, "murmur") {
		t.Fatalf("runtime = %q", dir)
	}
	info, err := os.Stat(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != before.Mode() {
		t.Fatalf("legacy squatted path mode changed from %v to %v", before.Mode(), info.Mode())
	}
}

func TestRuntimeDirPermissions(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", base)
	dir, err := RuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := ownerUID(info)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || uid != uint32(os.Geteuid()) {
		t.Fatalf("runtime final mode=%v uid=%d", info.Mode(), uid)
	}
}

func TestRuntimeDirSystemFallbackPolicy(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "missing")
		_, err := resolveRuntimeDir("", base, uint32(os.Geteuid()))
		if err == nil || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") || !strings.Contains(err.Error(), "0700") {
			t.Fatalf("error = %v", err)
		}
		if _, statErr := os.Stat(base); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("missing system runtime was created: %v", statErr)
		}
	})

	t.Run("mode", func(t *testing.T) {
		base := t.TempDir()
		if err := os.Chmod(base, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveRuntimeDir("", base, uint32(os.Geteuid())); err == nil {
			t.Fatal("system runtime with mode 0755 accepted")
		}
	})

	t.Run("injected owner", func(t *testing.T) {
		base := t.TempDir()
		wrongUID := uint32(os.Geteuid()) + 1
		if _, err := resolveRuntimeDir("", base, wrongUID); err == nil {
			t.Fatal("system runtime owned by another injected UID accepted")
		}
		if _, err := os.Stat(filepath.Join(base, "murmur")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("application runtime created under wrong owner: %v", err)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		base := t.TempDir()
		if err := os.Chmod(base, 0o700); err != nil {
			t.Fatal(err)
		}
		var first string
		for i := 0; i < 3; i++ {
			dir, err := resolveRuntimeDir("", base, uint32(os.Geteuid()))
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				first = dir
			} else if dir != first {
				t.Fatalf("runtime changed from %q to %q", first, dir)
			}
		}
		if first != filepath.Join(base, "murmur") {
			t.Fatalf("runtime = %q", first)
		}
	})
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("content = %q", data)
	}
	info, _ := os.Stat(path)
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("permissions = %o, want 600", got)
	}
}

func TestWriteFileAtomicFailedRenameLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "destination")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(destination, []byte("secret"), 0o600); err == nil {
		t.Fatal("expected rename failure")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "destination" || !entries[0].IsDir() {
		t.Fatalf("partial files left behind: %#v", entries)
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
