// Package service implements core.Service and the gateway backend on top of the store and live registry.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"quiver-playtesting/internal/core"
	"quiver-playtesting/internal/live"
	"quiver-playtesting/internal/rfb"
)

const (
	MinTTL       = time.Minute
	MaxTTL       = 30 * 24 * time.Hour
	maxTokenLen  = 128
	maxNameLen   = 64
	maxLabelLen  = 128
	maxVNCPasswd = 8
	testTimeout  = 15 * time.Second
	opTimeout    = 5 * time.Second
)

type Service struct {
	st   core.Store
	reg  *live.Registry
	now  func() time.Time
	dial func(ctx context.Context, addr, password string) (net.Conn, error)
}

var _ core.Service = (*Service)(nil)

func New(st core.Store, reg *live.Registry) *Service {
	return &Service{st: st, reg: reg, now: time.Now, dial: rfb.Dial}
}

func hashToken(token string) []byte {
	if len(token) > maxTokenLen {
		token = token[:maxTokenLen]
	}
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func (s *Service) Resolve(ctx context.Context, token string) (core.Link, core.VM, error) {
	l, err := s.st.LinkByTokenHash(ctx, hashToken(token))
	if err != nil {
		return core.Link{}, core.VM{}, err
	}
	if !l.Active(s.now()) {
		return core.Link{}, core.VM{}, core.ErrNotFound
	}
	vm, err := s.st.GetVM(ctx, l.VMID)
	if err != nil {
		return core.Link{}, core.VM{}, err
	}
	return l, vm, nil
}

func (s *Service) LogSession(ctx context.Context, sess core.Session, ended time.Time, reason string) error {
	return s.st.LogSession(ctx, sess, ended, reason)
}

func (s *Service) ListVMs(ctx context.Context) ([]core.VM, error) { return s.st.ListVMs(ctx) }

func printable(str string) bool {
	if !utf8.ValidString(str) {
		return false
	}
	for _, r := range str {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func validHost(h string) bool {
	if net.ParseIP(h) != nil {
		return true
	}
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func (s *Service) AddVM(ctx context.Context, name, host string, port int, password string) (core.VM, error) {
	n := utf8.RuneCountInString(name)
	if n < 1 || n > maxNameLen || !printable(name) || strings.TrimSpace(name) != name {
		return core.VM{}, errors.New("invalid name: 1 to 64 printable characters, no surrounding spaces")
	}
	if !validHost(host) {
		return core.VM{}, errors.New("invalid host: expected a DNS name or IP address")
	}
	if port < 1 || port > 65535 {
		return core.VM{}, errors.New("invalid port: expected 1 to 65535")
	}
	if len(password) > maxVNCPasswd {
		return core.VM{}, errors.New("invalid password: VNC authentication supports at most 8 bytes")
	}
	return s.st.AddVM(ctx, name, host, port, password)
}

func (s *Service) RemoveVM(ctx context.Context, id int64) error {
	if err := s.st.RemoveVM(ctx, id); err != nil {
		return err
	}
	s.reg.KillByVM(id, "vm_removed")
	return nil
}

func (s *Service) TestVM(ctx context.Context, id int64) error {
	vm, err := s.st.GetVM(ctx, id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	conn, err := s.dial(ctx, net.JoinHostPort(vm.Host, strconv.Itoa(vm.Port)), vm.Password)
	if err != nil {
		return fmt.Errorf("vm test failed: %w", err)
	}
	return conn.Close()
}

func (s *Service) ListLinks(ctx context.Context) ([]core.Link, error) { return s.st.ListLinks(ctx) }

func (s *Service) CreateLink(ctx context.Context, vmID int64, label string, ttl time.Duration) (core.Link, string, error) {
	if ttl < MinTTL || ttl > MaxTTL {
		return core.Link{}, "", errors.New("invalid ttl: expected 1 minute to 30 days")
	}
	if utf8.RuneCountInString(label) > maxLabelLen || !printable(label) {
		return core.Link{}, "", errors.New("invalid label: up to 128 printable characters")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return core.Link{}, "", errors.New("token generation failed")
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	l, err := s.st.CreateLink(ctx, vmID, label, hashToken(token), s.now().Add(ttl))
	if err != nil {
		return core.Link{}, "", err
	}
	return l, token, nil
}

func (s *Service) RevokeLink(ctx context.Context, id int64) error {
	if err := s.st.RevokeLink(ctx, id); err != nil {
		return err
	}
	s.reg.KillByLink(id, "revoked")
	return nil
}

func (s *Service) ListSessions() []core.Session { return s.reg.List() }

func (s *Service) Kill(sessionID string, revokeLink bool) error {
	sess, ok := s.reg.Get(sessionID)
	if !ok {
		return core.ErrNotFound
	}
	if !revokeLink {
		return s.reg.Kill(sessionID, "killed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := s.st.RevokeLink(ctx, sess.LinkID); err != nil {
		return err
	}
	s.reg.KillByLink(sess.LinkID, "revoked")
	return nil
}

func (s *Service) Flag(sessionID string) error {
	sess, ok := s.reg.Get(sessionID)
	if !ok {
		return core.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	return s.st.FlagLink(ctx, sess.LinkID)
}

func (s *Service) Subscribe() (<-chan core.Event, func()) { return s.reg.Subscribe() }
