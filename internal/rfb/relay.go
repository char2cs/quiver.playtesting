package rfb

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	encContinuousUpdates = -313
	msgEndContinuous     = 150

	maxEncodings   = 1024
	maxCutText     = 1 << 20
	maxFenceLen    = 64
	maxScreens     = 16
	maxServerName  = 4096
	relayBufSize   = 64 * 1024
	defaultPump    = 10 * time.Millisecond
	defaultAdvWait = 5 * time.Second
)

// ErrProtocol marks a client that sent something outside the supported RFB subset.
var ErrProtocol = errors.New("rfb: protocol violation")

type RelayOptions struct {
	// PumpInterval is the gap between the incremental requests sent to the VM. Zero means 10ms.
	PumpInterval time.Duration
	// AdvertiseWait is how long to wait for the first SetEncodings before relaying in plain mode. Zero means 5s.
	AdvertiseWait time.Duration
	// PumpWriter receives the pump's requests, so callers can keep them out of idle accounting. Nil means the vm conn.
	PumpWriter io.Writer
}

func protoErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, a...))
}

// Relay takes over after both handshakes (the next bytes are ClientInit). It
// implements the ContinuousUpdates extension towards the browser while driving
// the VM with its own incremental requests, and returns when either side ends
// or ctx is done. Both conns are closed on return.
func Relay(ctx context.Context, client, vm net.Conn, opts RelayOptions) error {
	if opts.PumpInterval <= 0 {
		opts.PumpInterval = defaultPump
	}
	if opts.AdvertiseWait <= 0 {
		opts.AdvertiseWait = defaultAdvWait
	}
	pw := opts.PumpWriter
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	fail := func(err error) {
		once.Do(func() {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
				first = err
			}
			cancel()
		})
	}
	run := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if v := recover(); v != nil {
					fail(fmt.Errorf("rfb relay panic: %v", v))
				}
			}()
			fail(f())
		}()
	}
	run(func() error {
		<-ctx.Done()
		client.Close()
		vm.Close()
		return nil
	})

	vmMu := &sync.Mutex{}
	vmw := &lockedWriter{mu: vmMu, w: vm}
	if pw == nil {
		pw = vm
	}
	pump := &lockedWriter{mu: vmMu, w: pw}
	r := &relay{client: client, vm: vmw, pump: pump, opts: opts, opened: make(chan struct{})}
	br := bufio.NewReader(client)

	if err := r.init(br, vm); err != nil {
		fail(err)
		wg.Wait()
		return finalErr(first, parent)
	}
	run(func() error { return r.toBrowser(ctx, vm) })
	run(func() error { return r.toVM(ctx, br, run) })
	wg.Wait()
	return finalErr(first, parent)
}

func finalErr(first error, parent context.Context) error {
	if first == nil {
		return parent.Err()
	}
	return first
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

type relay struct {
	client net.Conn
	vm     *lockedWriter
	pump   *lockedWriter
	opts   RelayOptions

	mu     sync.Mutex
	isOpen bool
	opened chan struct{}
	region atomic.Uint64
}

func (r *relay) init(br *bufio.Reader, vm io.Reader) error {
	var ci [1]byte
	if _, err := io.ReadFull(br, ci[:]); err != nil {
		return err
	}
	if _, err := r.vm.Write(ci[:]); err != nil {
		return err
	}
	var si [24]byte
	if _, err := io.ReadFull(vm, si[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(si[20:])
	if n > maxServerName {
		return protoErr("server name length %d", n)
	}
	out := make([]byte, 24+n)
	copy(out, si[:])
	if _, err := io.ReadFull(vm, out[24:]); err != nil {
		return err
	}
	_, err := r.client.Write(out)
	return err
}

// open releases the VM to browser copy. The 150 byte is written first and under
// the lock, so it can never land inside a server message.
func (r *relay) open(inject bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isOpen {
		return false, nil
	}
	if inject {
		if _, err := r.client.Write([]byte{msgEndContinuous}); err != nil {
			return false, err
		}
	}
	r.isOpen = true
	close(r.opened)
	return inject, nil
}

func (r *relay) toBrowser(ctx context.Context, vm io.Reader) error {
	t := time.NewTimer(r.opts.AdvertiseWait)
	defer t.Stop()
	select {
	case <-r.opened:
	case <-t.C:
		if _, err := r.open(false); err != nil {
			return err
		}
	case <-ctx.Done():
		return nil
	}
	_, err := io.CopyBuffer(struct{ io.Writer }{r.client}, struct{ io.Reader }{vm}, make([]byte, relayBufSize))
	return err
}

func (r *relay) toVM(ctx context.Context, br *bufio.Reader, run func(func() error)) error {
	p := newParser(br)
	seenEnc, continuous, pumping := false, false, false
	for {
		m, err := p.next()
		if err != nil {
			return err
		}
		switch m.typ {
		case 2:
			if !seenEnc {
				seenEnc = true
				if continuous, err = r.open(m.offered); err != nil {
					return err
				}
			}
		case 3:
			if m.flag && continuous {
				continue
			}
		case 150:
			if m.flag && continuous {
				r.region.Store(m.region)
				if !pumping {
					pumping = true
					run(func() error { return r.runPump(ctx) })
				}
			}
			continue
		}
		if m.out == nil {
			continue
		}
		if _, err := r.vm.Write(m.out); err != nil {
			return err
		}
	}
}

func (r *relay) runPump(ctx context.Context) error {
	t := time.NewTicker(r.opts.PumpInterval)
	defer t.Stop()
	var msg [10]byte
	msg[0], msg[1] = 3, 1
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		binary.BigEndian.PutUint64(msg[2:], r.region.Load())
		if _, err := r.pump.Write(msg[:]); err != nil {
			return err
		}
	}
}

type clientMsg struct {
	typ     byte
	out     []byte // bytes to forward to the VM, valid until the next call; nil when dropped
	flag    bool   // incremental (type 3) or enable (type 150)
	offered bool   // SetEncodings listed ContinuousUpdates
	region  uint64 // x, y, w, h as four big endian u16
}

type parser struct {
	r   *bufio.Reader
	buf [4 + 4*maxEncodings]byte
}

func newParser(r *bufio.Reader) *parser { return &parser{r: r} }

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// fill reads up to total bytes of the current message, of which have are already in buf.
func (p *parser) fill(have, total int) ([]byte, error) {
	if _, err := io.ReadFull(p.r, p.buf[have:total]); err != nil {
		return nil, unexpected(err)
	}
	return p.buf[:total], nil
}

func (p *parser) next() (clientMsg, error) {
	t, err := p.r.ReadByte()
	if err != nil {
		return clientMsg{}, err
	}
	p.buf[0] = t
	m := clientMsg{typ: t}
	switch t {
	case 0:
		m.out, err = p.fill(1, 20)
	case 2:
		err = p.setEncodings(&m)
	case 3, 150:
		var b []byte
		if b, err = p.fill(1, 10); err == nil {
			m.flag = b[1] != 0
			m.region = binary.BigEndian.Uint64(b[2:])
			if t == 3 {
				m.out = b
			}
		}
	case 4:
		m.out, err = p.fill(1, 8)
	case 5:
		m.out, err = p.fill(1, 6)
	case 6:
		err = p.cutText()
	case 248:
		var b []byte
		if b, err = p.fill(1, 9); err == nil {
			n := int(b[8])
			if n > maxFenceLen {
				return m, protoErr("fence payload %d", n)
			}
			m.out, err = p.fill(9, 9+n)
		}
	case 250:
		m.out, err = p.fill(1, 4)
	case 251:
		var b []byte
		if b, err = p.fill(1, 8); err == nil {
			n := int(b[6])
			if n > maxScreens {
				return m, protoErr("desktop size screens %d", n)
			}
			m.out, err = p.fill(8, 8+16*n)
		}
	case 255:
		var b []byte
		if b, err = p.fill(1, 2); err == nil {
			if b[1] != 0 {
				return m, protoErr("qemu subtype %d", b[1])
			}
			m.out, err = p.fill(2, 12)
		}
	default:
		return m, protoErr("unknown client message %d", t)
	}
	return m, err
}

func (p *parser) setEncodings(m *clientMsg) error {
	b, err := p.fill(1, 4)
	if err != nil {
		return err
	}
	n := int(binary.BigEndian.Uint16(b[2:]))
	if n > maxEncodings {
		return protoErr("%d encodings", n)
	}
	if b, err = p.fill(4, 4+4*n); err != nil {
		return err
	}
	w := 4
	for i := 0; i < n; i++ {
		e := b[4+4*i : 8+4*i]
		if int32(binary.BigEndian.Uint32(e)) == encContinuousUpdates {
			m.offered = true
			continue
		}
		copy(b[w:], e)
		w += 4
	}
	binary.BigEndian.PutUint16(b[2:], uint16((w-4)/4))
	m.out = b[:w]
	return nil
}

func (p *parser) cutText() error {
	b, err := p.fill(1, 8)
	if err != nil {
		return err
	}
	n := int64(int32(binary.BigEndian.Uint32(b[4:])))
	if n < 0 {
		n = -n
	}
	if n > maxCutText {
		return protoErr("cut text %d bytes", n)
	}
	if _, err := io.CopyN(io.Discard, p.r, n); err != nil {
		return unexpected(err)
	}
	return nil
}
