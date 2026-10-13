//go:build !unix

package dunetask

func setBuffers(fd uintptr, size int) error { return nil }
