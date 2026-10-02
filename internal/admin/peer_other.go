//go:build !linux && !darwin

package admin

import "net"

// The 0600 socket mode is the only guard on other platforms.
func peerAllowed(net.Conn) bool { return true }
