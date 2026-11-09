//go:build darwin

package broker

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// peerCredUID reads the uid of the process on the other end of a unix socket
// with LOCAL_PEERCRED (the credentials the kernel recorded at connect time).
func peerCredUID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("peer credentials: not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		cred *unix.Xucred
		gerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, gerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if gerr != nil {
		return 0, gerr
	}
	return cred.Uid, nil
}
