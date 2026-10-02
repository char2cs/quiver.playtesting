package admin

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func peerAllowed(c net.Conn) bool {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var gerr error
	if raw.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}) != nil || gerr != nil {
		return false
	}
	return int(cred.Uid) == os.Getuid()
}
