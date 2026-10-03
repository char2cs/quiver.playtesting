// Package rfb implements the minimal RFB 3.8 handshake pieces the gateway needs.
package rfb

import (
	"context"
	"crypto/des"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"syscall"
	"time"
)

const (
	handshakeTimeout = 10 * time.Second
	versionLen       = 12
	secNone          = 1
	secVNCAuth       = 2
)

var (
	serverVersion = []byte("RFB 003.008\n")
	version37     = []byte("RFB 003.007\n")
)

var metadataV6 = netip.MustParseAddr("fd00:ec2::254")

// CheckIP rejects addresses a VNC target must never have: unspecified, link-local
// (cloud metadata lives at 169.254.169.254), multicast and broadcast. RFC1918 and
// loopback stay allowed because VMs sit on the LAN.
func CheckIP(ip netip.Addr) error {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() ||
		ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || ip == metadataV6 {
		return errors.New("rfb: target address not allowed")
	}
	return nil
}

// checkDial runs on the resolved address of every connection attempt, so DNS
// rebinding cannot swap in a banned address after validation.
func checkDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("rfb: target address not allowed")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errors.New("rfb: target address not allowed")
	}
	return CheckIP(ip)
}

// Dial connects to addr and authenticates, returning the conn right before ClientInit.
func Dial(ctx context.Context, addr, password string) (net.Conn, error) {
	deadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	d := net.Dialer{Control: checkDial}
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("rfb dial: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	if err := clientHandshake(conn, password, deadline); err != nil {
		stop()
		conn.Close()
		return nil, err
	}
	if !stop() {
		conn.Close()
		return nil, errors.New("rfb dial: canceled")
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func clientHandshake(conn net.Conn, password string, deadline time.Time) error {
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	var ver [versionLen]byte
	if _, err := io.ReadFull(conn, ver[:]); err != nil {
		return fmt.Errorf("rfb version: %w", err)
	}
	if string(ver[:8]) != "RFB 003." || ver[11] != '\n' || ver[8] != '0' || ver[9] != '0' || ver[10] < '7' || ver[10] > '9' {
		return errors.New("rfb: unsupported server version")
	}
	minor := ver[10]
	reply := serverVersion
	if minor == '7' {
		reply = version37
	}
	if _, err := conn.Write(reply); err != nil {
		return err
	}
	var n [1]byte
	if _, err := io.ReadFull(conn, n[:]); err != nil {
		return err
	}
	if n[0] == 0 {
		return errors.New("rfb: server refused connection")
	}
	types := make([]byte, n[0])
	if _, err := io.ReadFull(conn, types); err != nil {
		return err
	}
	var hasNone, hasVNC bool
	for _, t := range types {
		hasNone = hasNone || t == secNone
		hasVNC = hasVNC || t == secVNCAuth
	}
	var chosen byte
	switch {
	case hasVNC:
		chosen = secVNCAuth
	case hasNone && password == "":
		chosen = secNone
	default:
		return errors.New("rfb: no acceptable security type")
	}
	if _, err := conn.Write([]byte{chosen}); err != nil {
		return err
	}
	if chosen == secVNCAuth {
		var challenge [16]byte
		if _, err := io.ReadFull(conn, challenge[:]); err != nil {
			return err
		}
		resp, err := VNCResponse(password, challenge)
		if err != nil {
			return err
		}
		if _, err := conn.Write(resp[:]); err != nil {
			return err
		}
	}
	if chosen == secNone && minor == '7' {
		return nil
	}
	var res [4]byte
	if _, err := io.ReadFull(conn, res[:]); err != nil {
		return err
	}
	if binary.BigEndian.Uint32(res[:]) != 0 {
		return errors.New("rfb: authentication failed")
	}
	return nil
}

// VNCResponse computes the VNC auth response. DES-ECB is mandated by the VNC
// authentication protocol (RFC 6143 7.2.2), it is not our choice and it is weak,
// so VM passwords only gate access on a trusted LAN. The key is the bit-reversed
// password (first 8 bytes, zero padded).
func VNCResponse(password string, challenge [16]byte) ([16]byte, error) {
	var key [8]byte
	copy(key[:], password)
	for i, b := range key {
		var r byte
		for j := 0; j < 8; j++ {
			r |= (b >> j & 1) << (7 - j)
		}
		key[i] = r
	}
	c, err := des.NewCipher(key[:])
	if err != nil {
		return [16]byte{}, err
	}
	var out [16]byte
	c.Encrypt(out[:8], challenge[:8])
	c.Encrypt(out[8:], challenge[8:])
	return out, nil
}

// Accept runs the server side of the handshake against an untrusted client.
func Accept(conn net.Conn) error {
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return err
	}
	if _, err := conn.Write(serverVersion); err != nil {
		return err
	}
	var ver [versionLen]byte
	if _, err := io.ReadFull(conn, ver[:]); err != nil {
		return fmt.Errorf("rfb version: %w", err)
	}
	if string(ver[:]) != string(serverVersion) {
		return errors.New("rfb: unsupported client version")
	}
	if _, err := conn.Write([]byte{1, secNone}); err != nil {
		return err
	}
	var sel [1]byte
	if _, err := io.ReadFull(conn, sel[:]); err != nil {
		return err
	}
	if sel[0] != secNone {
		return errors.New("rfb: unsupported security type")
	}
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return err
	}
	return conn.SetDeadline(time.Time{})
}
