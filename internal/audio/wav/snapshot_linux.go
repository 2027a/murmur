//go:build linux

package wav

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

const (
	memfdCloseOnExec = 0x0001
	memfdAllowSeals  = 0x0002
	fcntlAddSeals    = 1033
	sealSeal         = 0x0001
	sealShrink       = 0x0002
	sealGrow         = 0x0004
	sealWrite        = 0x0008
)

func createImmutableSnapshot(source *os.File, size int64, name string) (*os.File, error) {
	fd, err := memfdCreate("murmur-wav", memfdCloseOnExec|memfdAllowSeals)
	if err != nil {
		return nil, fmt.Errorf("création du snapshot anonyme: %w", err)
	}
	handle := os.NewFile(uintptr(fd), name)
	if handle == nil {
		_ = syscall.Close(fd)
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
	seals := uintptr(sealSeal | sealShrink | sealGrow | sealWrite)
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), fcntlAddSeals, seals); errno != 0 {
		return nil, fmt.Errorf("scellement du snapshot WAV: %w", errno)
	}
	ok = true
	return handle, nil
}
