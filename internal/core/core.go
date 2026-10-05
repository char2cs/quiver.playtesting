// Package core holds the shared types and interfaces every other package builds on.
package core

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrBusy     = errors.New("already in use")
)

type VM struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Password  string    `json:"-"` // never serialized to the admin protocol
	CreatedAt time.Time `json:"created_at"`
}

type Link struct {
	ID        int64     `json:"id"`
	VMID      int64     `json:"vm_id"`
	VMName    string    `json:"vm_name"`
	Label     string    `json:"label"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	Revoked   bool      `json:"revoked"`
	Flagged   bool      `json:"flagged"`
}

// Active reports whether the link may start a session at time now.
func (l Link) Active(now time.Time) bool { return !l.Revoked && now.Before(l.ExpiresAt) }

type Session struct {
	ID        string    `json:"id"`
	LinkID    int64     `json:"link_id"`
	Label     string    `json:"label"`
	VMID      int64     `json:"vm_id"`
	VMName    string    `json:"vm_name"`
	ClientIP  string    `json:"client_ip"`
	StartedAt time.Time `json:"started_at"`
	Recording string    `json:"recording,omitempty"` // path of the first mp4 file, empty when the session is not recorded
}

type Event struct {
	Type    string  `json:"type"` // "start" | "end"
	Session Session `json:"session"`
	Reason  string  `json:"reason,omitempty"` // for "end": closed, killed, revoked, vm_error, idle, ...
}

// Store is persistent state. Implemented by internal/store (SQLite).
type Store interface {
	AddVM(ctx context.Context, name, host string, port int, password string) (VM, error)
	ListVMs(ctx context.Context) ([]VM, error)
	GetVM(ctx context.Context, id int64) (VM, error)
	// RemoveVM also revokes the VM's links.
	RemoveVM(ctx context.Context, id int64) error

	// CreateLink stores only the SHA-256 of tokenHash (32 bytes).
	CreateLink(ctx context.Context, vmID int64, label string, tokenHash []byte, expires time.Time) (Link, error)
	ListLinks(ctx context.Context) ([]Link, error)
	GetLink(ctx context.Context, id int64) (Link, error)
	// LinkByTokenHash returns ErrNotFound for unknown hashes. The caller checks Active().
	LinkByTokenHash(ctx context.Context, tokenHash []byte) (Link, error)
	RevokeLink(ctx context.Context, id int64) error
	FlagLink(ctx context.Context, id int64) error

	LogSession(ctx context.Context, s Session, ended time.Time, reason string) error
}

// Service is what the admin socket drives. Implemented by internal/service.
type Service interface {
	ListVMs(ctx context.Context) ([]VM, error)
	AddVM(ctx context.Context, name, host string, port int, password string) (VM, error)
	RemoveVM(ctx context.Context, id int64) error // also kills live sessions on that VM
	TestVM(ctx context.Context, id int64) error   // TCP + RFB handshake + VNC auth

	ListLinks(ctx context.Context) ([]Link, error)
	// CreateLink returns the link and the plaintext token (shown once, never stored).
	CreateLink(ctx context.Context, vmID int64, label string, ttl time.Duration) (Link, string, error)
	RevokeLink(ctx context.Context, id int64) error // also kills its live session

	ListSessions() []Session
	Kill(sessionID string, revokeLink bool) error
	Flag(sessionID string) error // flags the session's link
	// Subscribe streams start/end events; the returned func unsubscribes.
	Subscribe() (<-chan Event, func())
}
