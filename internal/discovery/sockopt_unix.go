//go:build !windows

package discovery

import "syscall"

// setReuseAddr 在监听 socket 上打开 SO_REUSEADDR（unix 系 fd 为 int）。
func setReuseAddr(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
