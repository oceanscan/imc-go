//go:build windows

package imc

import (
	"syscall"
)

func setSocketOptions(fd uintptr) error {
	// On Windows, SO_REUSEADDR allows multiple binds to the same port.
	// There is no SO_REUSEPORT equivalent in the same way as Linux.
	// Windows 10+ behavior for SO_REUSEADDR is generally sufficient for multicast.
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
