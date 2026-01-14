//go:build unix

package imc

import (
	"golang.org/x/sys/unix"
)

func setSocketOptions(fd uintptr) error {
	err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	if err != nil {
		return err
	}
	return unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
}
