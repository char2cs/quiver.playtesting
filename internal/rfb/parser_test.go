package rfb

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

func parseAll(in []byte) ([]clientMsg, error) {
	p := newParser(bufio.NewReader(bytes.NewReader(in)))
	var out []clientMsg
	for {
		m, err := p.next()
		if err != nil {
			return out, err
		}
		m.out = bytes.Clone(m.out)
		out = append(out, m)
	}
}

func TestParserBoundaries(t *testing.T) {
	enc := func(n int) []byte {
		b := []byte{2, 0, byte(n >> 8), byte(n)}
		return append(b, make([]byte, 4*n)...)
	}
	cut := func(n int32) []byte {
		return []byte{6, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	cases := []struct {
		name string
		in   []byte
		fwd  int
	}{
		{"setpixelformat", append([]byte{0}, make([]byte, 19)...), 20},
		{"setencodings 0", enc(0), 4},
		{"setencodings max", enc(1024), 4 + 4096},
		{"fbur", []byte{3, 1, 0, 0, 0, 0, 0, 0, 0, 0}, 10},
		{"key", append([]byte{4}, make([]byte, 7)...), 8},
		{"pointer", append([]byte{5}, make([]byte, 5)...), 6},
		{"cut empty", cut(0), 0},
		{"cut max", append(cut(1<<20), make([]byte, 1<<20)...), 0},
		{"cut extended", append(cut(-8), make([]byte, 8)...), 0},
		{"enable", []byte{150, 1, 0, 0, 0, 0, 0, 0, 0, 0}, 0},
		{"fence empty", []byte{248, 0, 0, 0, 0, 0, 0, 0, 0}, 9},
		{"fence max", append([]byte{248, 0, 0, 0, 0, 0, 0, 0, 64}, make([]byte, 64)...), 73},
		{"xvp", []byte{250, 0, 1, 2}, 4},
		{"desktop size 0", []byte{251, 0, 0, 1, 0, 1, 0, 0}, 8},
		{"desktop size max", append([]byte{251, 0, 0, 1, 0, 1, 16, 0}, make([]byte, 256)...), 264},
		{"qemu key", append([]byte{255, 0}, make([]byte, 10)...), 12},
	}
	for _, c := range cases {
		msgs, err := parseAll(append(bytes.Clone(c.in), 4, 0, 0, 0, 0, 0, 0, 0))
		if err != io.EOF || len(msgs) != 2 {
			t.Errorf("%s: %d msgs, err %v", c.name, len(msgs), err)
			continue
		}
		if len(msgs[0].out) != c.fwd || msgs[1].typ != 4 {
			t.Errorf("%s: forwarded %d bytes, want %d", c.name, len(msgs[0].out), c.fwd)
		}
	}
}

func TestParserRejects(t *testing.T) {
	cases := map[string][]byte{
		"unknown":        {7},
		"encodings 1025": {2, 0, 4, 1},
		"cut over cap":   {6, 0, 0, 0, 0, 0x10, 0, 1},
		"qemu subtype":   {255, 2},
		"fence 65":       {248, 0, 0, 0, 0, 0, 0, 0, 65},
		"screens 17":     {251, 0, 0, 1, 0, 1, 17, 0},
	}
	for name, in := range cases {
		if _, err := parseAll(in); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, in := range [][]byte{{4, 1}, {2, 0, 0, 2, 0, 0, 0, 1}, {6, 0, 0, 0, 0, 0, 0, 9, 1}, {255}} {
		if _, err := parseAll(in); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%v: %v", in, err)
		}
	}
}

func FuzzParser(f *testing.F) {
	f.Add([]byte{2, 0, 0, 2, 0, 0, 0, 0, 255, 255, 254, 199, 4, 1, 0, 0, 0, 0, 0, 97})
	f.Add([]byte{6, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c', 248, 0, 0, 0, 0, 0, 0, 1, 1, 'x'})
	f.Add([]byte{251, 0, 0, 1, 0, 1, 1, 0})
	f.Fuzz(func(t *testing.T, in []byte) {
		p := newParser(bufio.NewReader(bytes.NewReader(in)))
		for {
			m, err := p.next()
			if err != nil {
				return
			}
			if len(m.out) > len(p.buf) {
				t.Fatalf("out %d > buf", len(m.out))
			}
			for i := 4; m.typ == 2 && i+4 <= len(m.out); i += 4 {
				if bytes.Equal(m.out[i:i+4], []byte{0xff, 0xff, 0xfe, 0xc7}) {
					t.Fatal("-313 forwarded")
				}
			}
		}
	})
}
