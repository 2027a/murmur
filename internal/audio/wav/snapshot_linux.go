//go:build linux

package wav

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func createImmutableSnapshot(source *os.File, size int64, name string) (*os.File, error) {
	fd, err := unix.MemfdCreate("murmur-wav", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("création du snapshot anonyme: %w", err)
	}
	handle := os.NewFile(uintptr(fd), name)
	if handle == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("descripteur de snapshot invalide")
	}
	ok := false
	defer func() {
		if !ok {
			_ = handle.Close()
		}
	}()
	if err := handle.Chmod(0o600); err != nil {
		return nil, err
	}
	if _, err := io.CopyN(handle, io.NewSectionReader(source, 0, size), size); err != nil {
		return nil, err
	}
	seals := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, seals); err != nil {
		return nil, fmt.Errorf("scellement du snapshot WAV: %w", err)
	}
	ok = true
	return handle, nil
}
