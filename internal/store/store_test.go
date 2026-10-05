package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"quiver-playtesting/internal/core"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "q.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, p
}

func hash(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestPermissions(t *testing.T) {
	s, p := open(t)
	ctx := context.Background()
	if _, err := s.AddVM(ctx, "a", "h", 5900, "pw"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p, p + "-wal", p + "-shm"} {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", f, st.Mode().Perm())
		}
	}
}

func TestPermissionsTightenedOnExisting(t *testing.T) {
	p := filepath.Join(t.TempDir(), "q.db")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

func TestPathWithSpecialChars(t *testing.T) {
	p := filepath.Join(t.TempDir(), "we ird?#%name.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
}

func TestVMCRUD(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	v, err := s.AddVM(ctx, "win", "10.0.0.1", 5901, "secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetVM(ctx, v.ID)
	if err != nil || got.Password != "secret" || got.Host != "10.0.0.1" || got.Port != 5901 || got.Name != "win" {
		t.Fatalf("%+v %v", got, err)
	}
	list, _ := s.ListVMs(ctx)
	if len(list) != 1 || list[0].Password != "" {
		t.Fatalf("%+v", list)
	}
	if _, err := s.GetVM(ctx, 999); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.RemoveVM(ctx, 999); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.RemoveVM(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListVMs(ctx); len(list) != 0 {
		t.Fatal("not removed")
	}
}

func TestSQLInjectionIsInert(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	evil := `x'); DROP TABLE vms;--`
	v, err := s.AddVM(ctx, evil, evil, 1, evil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetVM(ctx, v.ID)
	if err != nil || got.Name != evil {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestLinkLifecycle(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	v, _ := s.AddVM(ctx, "win", "h", 5900, "")
	exp := time.Now().Add(time.Hour)
	l, err := s.CreateLink(ctx, v.ID, "alice", hash(1), exp)
	if err != nil {
		t.Fatal(err)
	}
	if l.VMID != v.ID || l.VMName != "win" || l.Label != "alice" || l.Revoked || l.Flagged {
		t.Fatalf("%+v", l)
	}
	if d := l.ExpiresAt.Sub(exp); d > time.Millisecond || d < -time.Millisecond {
		t.Fatalf("expiry drift %v", d)
	}
	by, err := s.LinkByTokenHash(ctx, hash(1))
	if err != nil || by.ID != l.ID {
		t.Fatal(by, err)
	}
	if _, err := s.LinkByTokenHash(ctx, hash(2)); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.LinkByTokenHash(ctx, []byte("short")); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.FlagLink(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeLink(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	g, _ := s.GetLink(ctx, l.ID)
	if !g.Flagged || !g.Revoked || g.Active(time.Now()) {
		t.Fatalf("%+v", g)
	}
	if err := s.RevokeLink(ctx, 999); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.FlagLink(ctx, 999); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.GetLink(ctx, 999); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if ls, _ := s.ListLinks(ctx); len(ls) != 1 {
		t.Fatal(ls)
	}
}

func TestCreateLinkValidation(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	v, _ := s.AddVM(ctx, "win", "h", 5900, "")
	exp := time.Now().Add(time.Hour)
	if _, err := s.CreateLink(ctx, v.ID, "x", []byte("short"), exp); err == nil {
		t.Fatal("short hash accepted")
	}
	if _, err := s.CreateLink(ctx, 999, "x", hash(1), exp); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.CreateLink(ctx, v.ID, "x", hash(1), exp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLink(ctx, v.ID, "y", hash(1), exp); err == nil {
		t.Fatal("duplicate hash accepted")
	}
}

func TestRemoveVMRevokesLinksAndKeepsName(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	v1, _ := s.AddVM(ctx, "one", "h", 1, "")
	v2, _ := s.AddVM(ctx, "two", "h", 2, "")
	exp := time.Now().Add(time.Hour)
	l1, _ := s.CreateLink(ctx, v1.ID, "a", hash(1), exp)
	l2, _ := s.CreateLink(ctx, v2.ID, "b", hash(2), exp)
	if err := s.RemoveVM(ctx, v1.ID); err != nil {
		t.Fatal(err)
	}
	g, err := s.GetLink(ctx, l1.ID)
	if err != nil || !g.Revoked || g.VMName != "one" || g.VMID != 0 {
		t.Fatalf("%+v %v", g, err)
	}
	g2, _ := s.GetLink(ctx, l2.ID)
	if g2.Revoked || g2.VMName != "two" {
		t.Fatalf("%+v", g2)
	}
	by, err := s.LinkByTokenHash(ctx, hash(1))
	if err != nil || !by.Revoked {
		t.Fatal(by, err)
	}
}

func TestLogSession(t *testing.T) {
	s, p := open(t)
	ctx := context.Background()
	now := time.Now()
	err := s.LogSession(ctx, core.Session{ID: "s1", LinkID: 1, VMID: 2, VMName: "vm", Label: "l", ClientIP: "1.2.3.4", StartedAt: now}, now.Add(time.Minute), "closed")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	var reason string
	if err := s.db.QueryRow(`SELECT count(*), max(reason) FROM session_log`).Scan(&n, &reason); err != nil || n != 1 || reason != "closed" {
		t.Fatal(n, reason, err)
	}
	_ = p
}

func TestReopenPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "q.db")
	s, _ := Open(p)
	v, _ := s.AddVM(context.Background(), "n", "h", 1, "pw")
	s.Close()
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	g, err := s2.GetVM(context.Background(), v.ID)
	if err != nil || g.Password != "pw" {
		t.Fatal(g, err)
	}
}

func TestForeignKeysOn(t *testing.T) {
	s, _ := open(t)
	var on int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil || on != 1 {
		t.Fatal(on, err)
	}
	_, err := s.db.Exec(`INSERT INTO links (vm_id, vm_name, label, token_hash, expires_at, created_at) VALUES (777, 'x', 'x', ?, 0, 0)`, hash(9))
	if err == nil {
		t.Fatal("fk not enforced")
	}
	var _ sql.DB
}

func TestPruneSessionLog(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	now := time.Now()
	old := core.Session{ID: "old", StartedAt: now.Add(-100 * 24 * time.Hour)}
	if err := s.LogSession(ctx, old, now.Add(-100*24*time.Hour), "closed"); err != nil {
		t.Fatal(err)
	}
	if err := s.LogSession(ctx, core.Session{ID: "new", StartedAt: now}, now, "closed"); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneSessionLog(ctx, now.Add(-90*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, err)
	}
}

func TestWALFilesAreOwnerOnly(t *testing.T) {
	s, p := open(t)
	if err := s.LogSession(context.Background(), core.Session{ID: "x", StartedAt: time.Now()}, time.Now(), "closed"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		fi, err := os.Stat(p + suffix)
		if err != nil {
			continue
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s mode %v", suffix, fi.Mode())
		}
	}
}

func TestSessionLogRecordingColumn(t *testing.T) {
	s, _ := open(t)
	sess := core.Session{ID: "abc", LinkID: 1, Label: "l", VMID: 2, VMName: "vm", ClientIP: "1.2.3.4", StartedAt: time.Now(), Recording: "/rec/a.mp4"}
	if err := s.LogSession(context.Background(), sess, time.Now(), "closed"); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := s.db.QueryRow(`SELECT recording FROM session_log WHERE session_id = 'abc'`).Scan(&got); err != nil || got != "/rec/a.mp4" {
		t.Fatalf("recording %q %v", got, err)
	}
}

func TestOpenMigratesOldSessionLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE session_log (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, link_id INTEGER NOT NULL, label TEXT NOT NULL, vm_id INTEGER NOT NULL, vm_name TEXT NOT NULL, client_ip TEXT NOT NULL, started_at INTEGER NOT NULL, ended_at INTEGER NOT NULL, reason TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.LogSession(context.Background(), core.Session{ID: "x", StartedAt: time.Now()}, time.Now(), "closed"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("second open must be a no-op: %v", err)
	}
}
