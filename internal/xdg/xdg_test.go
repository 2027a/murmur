package xdg

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestConfigDir(t *testing.T) {
	t.Run("XDG_CONFIG_HOME", func(t *testing.T) {
		base := t.TempDir()
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

func TestConfigDirAcceptsSafeSymlink(t *testing.T) {
	base := t.TempDir()
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

func TestRuntimeDirRejectsUnsafeFallback(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", base)
	path := filepath.Join(base, "murmur-"+strconv.Itoa(os.Getuid()))
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := RuntimeDir(); err == nil {
		t.Fatal("unsafe fallback directory accepted")
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
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("permissions = %o, want 700", got)
	}
}

func TestRuntimeDirFallback(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())
	dir, err := RuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.Base(dir), "murmur-") {
		t.Fatalf("RuntimeDir() = %q", dir)
	}
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
