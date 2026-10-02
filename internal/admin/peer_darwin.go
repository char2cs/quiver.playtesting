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
	var cred *unix.Xucred
	var gerr error
	if raw.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}) != nil || gerr != nil {
		return false
	}
	return int(cred.Uid) == os.Getuid()
}
