//go:build windows

package discovery

import "syscall"

// setReuseAddr 在监听 socket 上打开 SO_REUSEADDR（Windows 系 fd 为 syscall.Handle）。
func setReuseAddr(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
