package rfb_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"quiver-playtesting/internal/rfb"
)

const wait = 5 * time.Second

func tcpPair(t testing.TB) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := <-ch
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

type recorder struct {
	mu   sync.Mutex
	buf  []byte
	note chan struct{}
}

func newRecorder(c net.Conn) *recorder {
	r := &recorder{note: make(chan struct{}, 1)}
	go func() {
		tmp := make([]byte, 4096)
		for {
			n, err := c.Read(tmp)
			r.mu.Lock()
			r.buf = append(r.buf, tmp[:n]...)
			r.mu.Unlock()
			select {
			case r.note <- struct{}{}:
			default:
			}
			if err != nil {
				return
			}
		}
	}()
	return r
}

func (r *recorder) bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.buf)
}

func (r *recorder) waitLen(t testing.TB, n int) []byte {
	t.Helper()
	deadline := time.After(wait)
	for {
		if b := r.bytes(); len(b) >= n {
			return b
		}
		select {
		case <-r.note:
		case <-deadline:
			t.Fatalf("timeout: have %d bytes, want %d", len(r.bytes()), n)
		}
	}
}

type rig struct {
	t       testing.TB
	browser net.Conn
	vm      net.Conn
	vmRecv  *recorder
	cancel  context.CancelFunc
	fin     chan struct{}
	err     error
}

func (r *rig) result() error {
	select {
	case <-r.fin:
		return r.err
	case <-time.After(wait):
		r.t.Fatal("relay did not stop")
		return nil
	}
}

func serverInit() []byte {
	si := make([]byte, 20)
	binary.BigEndian.PutUint16(si, 640)
	binary.BigEndian.PutUint16(si[2:], 480)
	si = binary.BigEndian.AppendUint32(si, 2)
	return append(si, "vm"...)
}

func newRig(t testing.TB, opts rfb.RelayOptions) *rig {
	t.Helper()
	browser, relayClient := tcpPair(t)
	relayVM, vm := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := &rig{t: t, browser: browser, vm: vm, cancel: cancel, fin: make(chan struct{})}
	go func() { r.err = rfb.Relay(ctx, relayClient, relayVM, opts); close(r.fin) }()
	t.Cleanup(func() {
		cancel()
		r.result()
	})
	browser.SetDeadline(time.Now().Add(30 * time.Second))
	vm.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := browser.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	var ci [1]byte
	if _, err := io.ReadFull(vm, ci[:]); err != nil || ci[0] != 1 {
		t.Fatalf("clientinit %v %v", ci, err)
	}
	if _, err := vm.Write(serverInit()); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(serverInit()))
	if _, err := io.ReadFull(browser, got); err != nil || !bytes.Equal(got, serverInit()) {
		t.Fatalf("serverinit %v %v", got, err)
	}
	r.vmRecv = newRecorder(vm)
	return r
}

func (r *rig) send(b ...[]byte) {
	r.t.Helper()
	if _, err := r.browser.Write(bytes.Join(b, nil)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) read(n int) []byte {
	r.t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(r.browser, b); err != nil {
		r.t.Fatal(err)
	}
	return b
}

func setEnc(encs ...int32) []byte {
	b := []byte{2, 0, 0, byte(len(encs))}
	for _, e := range encs {
		b = binary.BigEndian.AppendUint32(b, uint32(e))
	}
	return b
}

func fbur(incr byte, x, y, w, h uint16) []byte {
	b := []byte{3, incr}
	for _, v := range []uint16{x, y, w, h} {
		b = binary.BigEndian.AppendUint16(b, v)
	}
	return b
}

func enable(on byte, x, y, w, h uint16) []byte {
	b := fbur(on, x, y, w, h)
	b[0] = 150
	return b
}

var (
	keyDown = []byte{4, 1, 0, 0, 0, 0, 0, 'a'}
	keyUp   = []byte{4, 0, 0, 0, 0, 0, 0, 'a'}
	pointer = []byte{5, 1, 0, 10, 0, 20}
)

func TestRelaySwallowsIncrementalInContinuousMode(t *testing.T) {
	r := newRig(t, rfb.RelayOptions{})
	r.send(setEnc(0, -313, 1))
	if got := r.read(1); got[0] != 150 {
		t.Fatalf("want 150, got %v", got)
	}
	r.send(fbur(1, 0, 0, 10, 10), fbur(0, 0, 0, 640, 480), keyDown, fbur(1, 1, 1, 1, 1), pointer, keyUp)
	want := bytes.Join([][]byte{setEnc(0, 1), fbur(0, 0, 0, 640, 480), keyDown, pointer, keyUp}, nil)
	if got := r.vmRecv.waitLen(t, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("vm got %v\nwant %v", got, want)
	}
}

func TestRelayPumpDrivesVM(t *testing.T) {
	const interval = 5 * time.Millisecond
	r := newRig(t, rfb.RelayOptions{PumpInterval: interval})
	r.send(setEnc(-313))
	r.read(1)
	start := time.Now()
	r.send(enable(1, 1, 2, 3, 4))
	hdr := len(setEnc())
	msg := fbur(1, 1, 2, 3, 4)
	got := r.vmRecv.waitLen(t, hdr+10*8)
	elapsed := time.Since(start)
	n := (len(got) - hdr) / 10
	if max := int(elapsed/interval) + 2; n > max {
		t.Fatalf("%d requests in %v, more than the interval allows (%d)", n, elapsed, max)
	}
	for i := 0; i < n; i++ {
		if !bytes.Equal(got[hdr+10*i:hdr+10*i+10], msg) {
			t.Fatalf("request %d = %v", i, got[hdr+10*i:hdr+10*i+10])
		}
	}

	r.send(enable(1, 9, 9, 9, 9))
	next := fbur(1, 9, 9, 9, 9)
	deadline := time.Now().Add(wait)
	for {
		b := r.vmRecv.bytes()
		if end := hdr + (len(b)-hdr)/10*10; end >= hdr+10 && bytes.Equal(b[end-10:end], next) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("pump never switched to the new region")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRelayEnableZeroKeepsRunning(t *testing.T) {
	r := newRig(t, rfb.RelayOptions{PumpInterval: 2 * time.Millisecond})
	r.send(setEnc(-313))
	r.read(1)
	r.send(enable(1, 0, 0, 5, 5), enable(0, 0, 0, 5, 5), keyDown)
	b := r.vmRecv.waitLen(t, len(setEnc())+30)
	if bytes.Contains(b, keyUp) {
		t.Fatal("unexpected key")
	}
}

func TestRelayStripsContinuousUpdatesEncoding(t *testing.T) {
	cases := []struct{ in, out []int32 }{
		{[]int32{0, -313, 7, -239, 1}, []int32{0, 7, -239, 1}},
		{[]int32{-313}, nil},
		{[]int32{-313, -313, 5}, []int32{5}},
		{[]int32{5, 4, 3}, []int32{5, 4, 3}},
		{nil, nil},
	}
	for _, c := range cases {
		r := newRig(t, rfb.RelayOptions{})
		r.send(setEnc(c.in...), keyDown)
		want := append(setEnc(c.out...), keyDown...)
		if got := r.vmRecv.waitLen(t, len(want)); !bytes.Equal(got, want) {
			t.Errorf("in %v: vm got %v want %v", c.in, got, want)
		}
	}
}

func TestRelayInjectsOnceBeforeFirstVMByte(t *testing.T) {
	r := newRig(t, rfb.RelayOptions{})
	r.vm.Write([]byte("XYZ"))
	r.send(setEnc(-313), setEnc(-313))
	if got := r.read(4); string(got) != "\x96XYZ" {
		t.Fatalf("got %q", got)
	}
	r.vm.Write([]byte("QQ"))
	if got := r.read(2); string(got) != "QQ" {
		t.Fatalf("second SetEncodings must not inject again, got %q", got)
	}
}

func TestRelayPlainModeWithoutOffer(t *testing.T) {
	r := newRig(t, rfb.RelayOptions{PumpInterval: time.Millisecond})
	r.vm.Write([]byte("XYZ"))
	r.send(setEnc(0, 1), fbur(1, 0, 0, 5, 5), enable(1, 0, 0, 5, 5), keyDown)
	if got := r.read(3); string(got) != "XYZ" {
		t.Fatalf("got %q", got)
	}
	want := bytes.Join([][]byte{setEnc(0, 1), fbur(1, 0, 0, 5, 5), keyDown}, nil)
	if got := r.vmRecv.waitLen(t, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("vm got %v want %v", got, want)
	}
}

func TestRelayAdvertiseTimeoutFallsBackToPlain(t *testing.T) {
	r := newRig(t, rfb.RelayOptions{AdvertiseWait: 20 * time.Millisecond})
	r.vm.Write([]byte("ABC"))
	if got := r.read(3); string(got) != "ABC" {
		t.Fatalf("got %q", got)
	}
	r.send(setEnc(-313), fbur(1, 0, 0, 1, 1))
	want := append(setEnc(), fbur(1, 0, 0, 1, 1)...)
	r.vmRecv.waitLen(t, len(want))
	r.vm.Write([]byte("DEF"))
	if got := r.read(3); string(got) != "DEF" {
		t.Fatalf("late offer must not inject, got %q", got)
	}
	if got := r.vmRecv.bytes(); !bytes.Equal(got, want) {
		t.Fatalf("incremental request must pass in plain mode: %v", got)
	}
}

func TestRelayDropsClipboard(t *testing.T) {
	cut := func(n int32, payload string) []byte {
		b := []byte{6, 0, 0, 0}
		b = binary.BigEndian.AppendUint32(b, uint32(n))
		return append(b, payload...)
	}
	r := newRig(t, rfb.RelayOptions{})
	r.send(cut(5, "hello"), cut(0, ""), cut(-3, "abc"), keyDown)
	if got := r.vmRecv.waitLen(t, len(keyDown)); !bytes.Equal(got, keyDown) {
		t.Fatalf("vm got %v", got)
	}
}

func TestRelayWhitelistedMessagesDoNotDesync(t *testing.T) {
	fence := append([]byte{248, 0, 0, 0, 0, 0, 0, 3, 64}, bytes.Repeat([]byte{'f'}, 64)...)
	desk := append([]byte{251, 0, 3, 0, 2, 0, 2, 0}, make([]byte, 32)...)
	qemu := append([]byte{255, 0}, make([]byte, 10)...)
	msgs := [][]byte{
		append([]byte{0, 0, 0, 0}, make([]byte, 16)...),
		keyDown, pointer, fence, {248, 0, 0, 0, 0, 0, 0, 0, 0},
		{250, 0, 1, 2}, desk, {251, 0, 0, 0, 0, 0, 0, 0}, qemu,
	}
	r := newRig(t, rfb.RelayOptions{})
	want := bytes.Join(msgs, nil)
	r.send(want, keyUp)
	want = append(want, keyUp...)
	if got := r.vmRecv.waitLen(t, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("desync: got %d bytes", len(got))
	}
}

func TestRelayMalformedClosesWithError(t *testing.T) {
	big := func(n int) []byte { return append([]byte{2, 0}, byte(n>>8), byte(n)) }
	cases := map[string][]byte{
		"unknown type":      {99},
		"type 1":            {1, 0, 0, 0},
		"oversized encs":    big(1025),
		"oversized cut":     {6, 0, 0, 0, 0, 0x20, 0, 1},
		"oversized ext cut": {6, 0, 0, 0, 0xff, 0xdf, 0xff, 0xff},
		"bad qemu subtype":  {255, 1, 0, 0},
		"fence too long":    {248, 0, 0, 0, 0, 0, 0, 0, 65},
		"too many screens":  {251, 0, 0, 1, 0, 1, 17, 0},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, rfb.RelayOptions{})
			r.send(in)
			if err := r.result(); !errors.Is(err, rfb.ErrProtocol) {
				t.Fatalf("err = %v", err)
			}
			if _, err := r.browser.Read(make([]byte, 1)); err == nil {
				t.Fatal("browser conn still open")
			}
		})
	}
}

func TestRelayTruncatedMessage(t *testing.T) {
	for _, in := range [][]byte{{4, 1, 0}, setEnc(1, 2)[:6], {6, 0, 0, 0, 0, 0, 0, 5, 'h'}} {
		r := newRig(t, rfb.RelayOptions{})
		r.send(in)
		r.browser.(*net.TCPConn).CloseWrite()
		if r.result() == nil {
			t.Fatalf("%v: want error", in)
		}
	}
}

func TestRelayCleanEOFReturnsNil(t *testing.T) {
	r := newRig(t, rfb.RelayOptions{})
	r.send(keyDown)
	r.browser.(*net.TCPConn).CloseWrite()
	if err := r.result(); err != nil {
		t.Fatal(err)
	}
}

func TestRelayCancelClosesEverything(t *testing.T) {
	r := newRig(t, rfb.RelayOptions{PumpInterval: time.Millisecond})
	r.send(setEnc(-313))
	r.read(1)
	r.send(enable(1, 0, 0, 1, 1))
	r.vmRecv.waitLen(t, len(setEnc())+10)
	r.cancel()
	if err := r.result(); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.browser.Read(make([]byte, 1)); err == nil {
		t.Fatal("browser conn open")
	}
	for {
		if _, err := r.vm.Read(make([]byte, 64)); err != nil {
			break
		}
	}
}

func TestRelayVMCloseEndsSession(t *testing.T) {
	r := newRig(t, rfb.RelayOptions{})
	r.send(setEnc())
	r.vm.Close()
	r.result()
}

func TestRelayThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput guard")
	}
	const total = 400 << 20
	browser, relayClient := tcpPair(t)
	relayVM, vm := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rfb.Relay(ctx, relayClient, relayVM, rfb.RelayOptions{}) }()
	browser.Write([]byte{1})
	io.ReadFull(vm, make([]byte, 1))
	vm.Write(serverInit())
	io.ReadFull(browser, make([]byte, len(serverInit())))
	browser.Write(setEnc())
	go func() {
		chunk := make([]byte, 256<<10)
		for sent := 0; sent < total; sent += len(chunk) {
			if _, err := vm.Write(chunk); err != nil {
				return
			}
		}
	}()
	start := time.Now()
	n, err := io.CopyN(io.Discard, browser, total)
	if err != nil || n != total {
		t.Fatalf("copied %d: %v", n, err)
	}
	mbps := float64(total) / (1 << 20) / time.Since(start).Seconds()
	t.Logf("%.0f MB/s", mbps)
	if mbps < throughputFloor {
		t.Fatalf("throughput %.0f MB/s below %.0f", mbps, throughputFloor)
	}
	cancel()
	<-done
}
