//go:build !linux && !darwin

package api

import (
	"errors"
	"net"
)

// peerCredUID is not implemented on this OS: every socket peer is refused
// (fail closed).
func peerCredUID(net.Conn) (uint32, error) {
	return 0, errors.New("peer credentials are not supported on this OS")
}
