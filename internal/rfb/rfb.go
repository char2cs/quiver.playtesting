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
	"time"
)

const (
	handshakeTimeout = 10 * time.Second
	versionLen       = 12
	secNone          = 1
	secVNCAuth       = 2
)

var serverVersion = []byte("RFB 003.008\n")

// Dial connects to addr and authenticates, returning the conn right before ClientInit.
func Dial(ctx context.Context, addr, password string) (net.Conn, error) {
	deadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var d net.Dialer
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
	if _, err := conn.Write(serverVersion); err != nil {
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
	var res [4]byte
	if _, err := io.ReadFull(conn, res[:]); err != nil {
		return err
	}
	if binary.BigEndian.Uint32(res[:]) != 0 {
		return errors.New("rfb: authentication failed")
	}
	return nil
}

// VNCResponse computes the VNC auth response: DES-ECB of the challenge with the
// bit-reversed password (first 8 bytes, zero padded) as key.
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
