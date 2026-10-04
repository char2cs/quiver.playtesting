package rfbtest

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// FB is a fake VNC server with a real screen: a solid colour that tests can change and resize.
// It answers FramebufferUpdateRequests like a real server, so recorder code can be tested against it.
type FB struct {
	t        testing.TB
	ln       net.Listener
	password string

	mu      sync.Mutex
	w, h    int
	b, g, r byte
	gen     int
	shared  bool
	encs    []int32
	clients int
	conns   []net.Conn
	closed  bool
}

func NewFB(t testing.TB, password string, w, h int) *FB {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &FB{t: t, ln: ln, password: password, w: w, h: h, gen: 1}
	t.Cleanup(f.Close)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			if f.closed {
				f.mu.Unlock()
				c.Close()
				return
			}
			f.conns = append(f.conns, c)
			f.mu.Unlock()
			go f.handle(c)
		}
	}()
	return f
}

// Close stops listening and drops every connected client. It is safe to call more than once.
func (f *FB) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	f.ln.Close()
	for _, c := range conns {
		c.Close()
	}
}

func (f *FB) Addr() string { return f.ln.Addr().String() }

func (f *FB) Fill(r, g, b byte) {
	f.mu.Lock()
	f.r, f.g, f.b = r, g, b
	f.gen++
	f.mu.Unlock()
}

func (f *FB) Resize(w, h int) {
	f.mu.Lock()
	f.w, f.h = w, h
	f.gen++
	f.mu.Unlock()
}

func (f *FB) Shared() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shared
}

func (f *FB) Encodings() []int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int32(nil), f.encs...)
}

func (f *FB) Clients() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clients
}

func (f *FB) handle(c net.Conn) {
	defer c.Close()
	f.mu.Lock()
	w, h := f.w, f.h
	f.mu.Unlock()
	shared, ok := handshake(c, f.password, w, h)
	if !ok {
		return
	}
	f.mu.Lock()
	f.shared = shared
	f.clients++
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.clients--; f.mu.Unlock() }()

	reqs := make(chan bool, 16)
	done := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(done)
		var t [1]byte
		for {
			if _, err := io.ReadFull(c, t[:]); err != nil {
				return
			}
			switch t[0] {
			case 0:
				io.ReadFull(c, make([]byte, 19))
			case 2:
				var hd [3]byte
				if _, err := io.ReadFull(c, hd[:]); err != nil {
					return
				}
				n := int(binary.BigEndian.Uint16(hd[1:]))
				buf := make([]byte, 4*n)
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				encs := make([]int32, n)
				for i := range encs {
					encs[i] = int32(binary.BigEndian.Uint32(buf[4*i:]))
				}
				f.mu.Lock()
				f.encs = encs
				f.mu.Unlock()
			case 3:
				var rq [9]byte
				if _, err := io.ReadFull(c, rq[:]); err != nil {
					return
				}
				select {
				case reqs <- rq[0] != 0:
				case <-stop:
					return
				}
			default:
				return
			}
		}
	}()

	sent, knownW, knownH := 0, w, h
	for {
		var inc bool
		select {
		case inc = <-reqs:
		case <-done:
			return
		}
		for inc {
			f.mu.Lock()
			g := f.gen
			f.mu.Unlock()
			if g != sent {
				break
			}
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
		f.mu.Lock()
		cw, ch, gen := f.w, f.h, f.gen
		b, g, r := f.b, f.g, f.r
		f.mu.Unlock()
		out := []byte{0, 0}
		n := 1
		resized := cw != knownW || ch != knownH
		if resized {
			n = 2
		}
		out = binary.BigEndian.AppendUint16(out, uint16(n))
		if resized {
			out = binary.BigEndian.AppendUint16(out, 0)
			out = binary.BigEndian.AppendUint16(out, 0)
			out = binary.BigEndian.AppendUint16(out, uint16(cw))
			out = binary.BigEndian.AppendUint16(out, uint16(ch))
			out = binary.BigEndian.AppendUint32(out, uint32(0xFFFFFF21)) // -223 DesktopSize
		}
		out = binary.BigEndian.AppendUint16(out, 0)
		out = binary.BigEndian.AppendUint16(out, 0)
		out = binary.BigEndian.AppendUint16(out, uint16(cw))
		out = binary.BigEndian.AppendUint16(out, uint16(ch))
		out = binary.BigEndian.AppendUint32(out, 0) // Raw
		for i := 0; i < cw*ch; i++ {
			out = append(out, b, g, r, 0)
		}
		if _, err := c.Write(out); err != nil {
			return
		}
		sent, knownW, knownH = gen, cw, ch
	}
}
