//go:build linux && amd64

package wav

import (
	"syscall"
	"unsafe"
)

func memfdCreate(name string, flags uintptr) (int, error) {
	label, err := syscall.BytePtrFromString(name)
	if err != nil {
		return -1, err
	}
	fd, _, errno := syscall.Syscall(319, uintptr(unsafe.Pointer(label)), flags, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}
