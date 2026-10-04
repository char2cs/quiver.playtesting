package recorder

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// ErrProtocol marks a server that sent something the capture client does not accept.
var ErrProtocol = errors.New("recorder: unexpected data from the VNC server")

const (
	encRaw         = 0
	encCopyRect    = 1
	encDesktopSize = -223
	maxCutText     = 1 << 20
)

// 32 bits per pixel, depth 24, little endian, true colour, 8 bits each, R at 16, G at 8, B at 0:
// pixels arrive as B, G, R, unused, which is what ffmpeg reads as bgra.
var pixelFormat = []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}

func setup() []byte {
	b := append([]byte{0, 0, 0, 0}, pixelFormat...)
	b = append(b, 2, 0, 0, 3)
	for _, e := range []int32{encRaw, encCopyRect, encDesktopSize} {
		b = binary.BigEndian.AppendUint32(b, uint32(e))
	}
	return b
}

func request(incremental bool, w, h int) []byte {
	b := []byte{3, 0}
	if incremental {
		b[1] = 1
	}
	b = binary.BigEndian.AppendUint16(b, 0)
	b = binary.BigEndian.AppendUint16(b, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(w))
	return binary.BigEndian.AppendUint16(b, uint16(h))
}

func protoErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, a...))
}

// Run joins the VNC session on conn (already authenticated) as a shared viewer and keeps fb up to date.
// ready is called once, as soon as the screen size is known. Run returns nil when ctx ends and an error
// when the connection or the server fails. It closes conn on return.
func Run(ctx context.Context, conn net.Conn, ready func(*Framebuffer)) error {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	err := run(conn, ready)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func run(conn net.Conn, ready func(*Framebuffer)) error {
	br := bufio.NewReaderSize(conn, 64<<10)
	if _, err := conn.Write([]byte{1}); err != nil {
		return err
	}
	var si [24]byte
	if _, err := io.ReadFull(br, si[:]); err != nil {
		return err
	}
	if n := binary.BigEndian.Uint32(si[20:]); n > 1<<16 {
		return protoErr("desktop name too long")
	} else if _, err := br.Discard(int(n)); err != nil {
		return err
	}
	fb, err := NewFramebuffer(int(binary.BigEndian.Uint16(si[0:])), int(binary.BigEndian.Uint16(si[2:])))
	if err != nil {
		return protoErr("%v", err)
	}
	w, h := fb.Size()
	if _, err := conn.Write(append(setup(), request(false, w, h)...)); err != nil {
		return err
	}
	ready(fb)

	var raw []byte
	for {
		t, err := br.ReadByte()
		if err != nil {
			return err
		}
		switch t {
		case 0:
			if raw, err = readUpdate(br, fb, raw); err != nil {
				return err
			}
			w, h := fb.Size()
			if _, err := conn.Write(request(true, w, h)); err != nil {
				return err
			}
		case 2:
		case 3:
			var hd [7]byte
			if _, err := io.ReadFull(br, hd[:]); err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(hd[3:])
			if n > maxCutText {
				return protoErr("cut text too long")
			}
			if _, err := br.Discard(int(n)); err != nil {
				return err
			}
		default:
			return protoErr("message type %d", t)
		}
	}
}

func readUpdate(br *bufio.Reader, fb *Framebuffer, raw []byte) ([]byte, error) {
	var hd [3]byte
	if _, err := io.ReadFull(br, hd[:]); err != nil {
		return raw, err
	}
	resized := false
	for n := int(binary.BigEndian.Uint16(hd[1:])); n > 0; n-- {
		var r [12]byte
		if _, err := io.ReadFull(br, r[:]); err != nil {
			return raw, err
		}
		x, y := int(binary.BigEndian.Uint16(r[0:])), int(binary.BigEndian.Uint16(r[2:]))
		w, h := int(binary.BigEndian.Uint16(r[4:])), int(binary.BigEndian.Uint16(r[6:]))
		switch int32(binary.BigEndian.Uint32(r[8:])) {
		case encRaw:
			if sw, sh := fb.Size(); x+w > sw || y+h > sh {
				return raw, protoErr("raw rectangle outside the screen")
			}
			if need := w * h * 4; cap(raw) < need {
				raw = make([]byte, need)
			} else {
				raw = raw[:need]
			}
			if _, err := io.ReadFull(br, raw); err != nil {
				return raw, err
			}
			if err := fb.PutRaw(x, y, w, h, raw); err != nil {
				return raw, protoErr("%v", err)
			}
		case encCopyRect:
			var s [4]byte
			if _, err := io.ReadFull(br, s[:]); err != nil {
				return raw, err
			}
			if err := fb.Copy(int(binary.BigEndian.Uint16(s[0:])), int(binary.BigEndian.Uint16(s[2:])), w, h, x, y); err != nil {
				return raw, protoErr("%v", err)
			}
		case encDesktopSize:
			if resized {
				return raw, protoErr("several desktop size rectangles in one update")
			}
			resized = true
			if sw, sh := fb.Size(); sw == w && sh == h {
				break
			}
			if err := fb.Resize(w, h); err != nil {
				return raw, protoErr("%v", err)
			}
		default:
			return raw, protoErr("encoding %d", int32(binary.BigEndian.Uint32(r[8:])))
		}
	}
	return raw, nil
}
