//go:build unix

package dunetask

import "golang.org/x/sys/unix"

func setBuffers(fd uintptr, size int) error {
	if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF, size); err != nil {
		return err
	}
	return unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, size)
}
