# Session Recording Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record every playtest session to an mp4 in a per-link folder on the gateway host, from connect to disconnect, without touching the live stream.

**Architecture:** For each session the gateway opens a second, shared VNC connection to the VM, decodes Raw and CopyRect updates into an in-memory framebuffer and pipes frames at a fixed rate to an `ffmpeg` child that writes fragmented H.264 mp4. Failures only end the recording. The page header shows "This session is recorded" while recording is available.

**Tech Stack:** Go 1.26 (module `quiver-playtesting`), `ffmpeg` at runtime, existing `internal/rfb` (Dial, handshake), SQLite store, Bubble Tea TUI.

**Spec:** `docs/superpowers/specs/2026-10-04-session-recording-design.md`

## Global Constraints

- Module path is `quiver-playtesting`; imports look like `quiver-playtesting/internal/core`.
- Run `gofmt -w` on the touched files before every commit.
- No em dashes or en dashes anywhere in code comments, docs or copy.
- Comments only for non-obvious WHY, never WHAT (repo style).
- Folders mode 0700, files mode 0600. Names built only from numeric link ID, slug alphabet `[a-z0-9-]` (max 40 chars), UTC timestamp and the session ID.
- The capture connection uses `rfb.Dial` (keeps SSRF guards) and only connects to the VM registered for the session.
- ffmpeg runs with an explicit argument list, no shell, environment limited to `PATH`.
- A recording failure never ends or slows the session. Missing ffmpeg disables recording at startup with a warning, it never stops the gateway from starting.
- Defaults: `--recordings` true, `--recordings-dir` `<data>/recordings`, `--ffmpeg` `ffmpeg`, `--record-fps` 10, `--recordings-max` 2, `--recordings-min-free` 2GiB, `--recordings-retention` 720h. Env names `QP_RECORDINGS`, `QP_RECORDINGS_DIR`, `QP_FFMPEG`, `QP_RECORD_FPS`, `QP_RECORDINGS_MAX`, `QP_RECORDINGS_MIN_FREE`, `QP_RECORDINGS_RETENTION`.
- Deviation from the spec text, decided here: `core.Session.Recording` is a string (path of the first mp4 file, empty when not recording) instead of a bool, and the `session_log.recording` column is `TEXT NOT NULL DEFAULT ''` instead of nullable. Task 1 updates the spec to match.

## Review Focus

- VM resolution changes mid-session: must start `_part2.mp4`, not corrupt or stall the first file (Task 6).
- Odd VM dimensions (for example 1365x767): yuv420p needs even sizes, the video must still be written (Task 6).
- VM or VNC server dies mid-session: recording ends, session logic and gateway keep running, file stays playable (Tasks 6, 8).
- Hostile or buggy VNC server (unknown message type, rectangle outside the screen, giant desktop size): capture errors out, bounded memory, no panic (Tasks 3, 5).
- Disk fills or recordings cap reached: new sessions run unrecorded and the header notice does not lie about the file being written (Tasks 6, 8).

---

### Task 1: Session.Recording, registry setter, session_log column

**Files:**
- Modify: `internal/core/core.go` (Session struct)
- Modify: `internal/live/live.go`
- Modify: `internal/store/store.go` (schema, Open, LogSession)
- Modify: `docs/superpowers/specs/2026-10-04-session-recording-design.md` (Visibility section)
- Test: `internal/live/live_test.go`, `internal/store/store_test.go`

**Interfaces:**
- Produces: `core.Session.Recording string` (`json:"recording,omitempty"`), `(*live.Registry).SetRecording(id, path string)`, session_log column `recording`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/live/live_test.go`:

```go
func TestSetRecording(t *testing.T) {
	r := New()
	if err := r.Start(sess("a", 1, 10), nil); err != nil {
		t.Fatal(err)
	}
	r.SetRecording("a", "/rec/x.mp4")
	r.SetRecording("missing", "/rec/y.mp4")
	l := r.List()
	if len(l) != 1 || l[0].Recording != "/rec/x.mp4" {
		t.Fatalf("list %+v", l)
	}
	if g, _ := r.Get("a"); g.Recording != "/rec/x.mp4" {
		t.Fatalf("get %+v", g)
	}
}
```

Append to `internal/store/store_test.go` (package `store`; the file's existing helper `open(t) (*Store, string)` opens a temp database):

```go
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
```

Add any missing imports (`database/sql`, `path/filepath`, `time`, `context`, `quiver-playtesting/internal/core`) to the test file.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/live ./internal/store -run 'SetRecording|Recording|Migrates' -v`
Expected: FAIL (compile errors: `Recording` field and `SetRecording` undefined).

- [ ] **Step 3: Implement**

`internal/core/core.go`, add to `Session` after `StartedAt`:

```go
	Recording string    `json:"recording,omitempty"` // path of the first mp4 file, empty when the session is not recorded
```

`internal/live/live.go`, add after `End`:

```go
func (r *Registry) SetRecording(id, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.sessions[id]; ok {
		e.sess.Recording = path
	}
}
```

`internal/store/store.go`: in `schema` add `recording  TEXT NOT NULL DEFAULT ''` as the last column of `session_log` (after `reason TEXT NOT NULL,` so the line before it ends with a comma). Add after the `db.Exec(schema)` block in `Open`:

```go
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
```

and the function:

```go
// migrate adds columns that databases created by older releases lack.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(session_log)`)
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "recording" {
			has = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE session_log ADD COLUMN recording TEXT NOT NULL DEFAULT ''`)
	return err
}
```

Replace `LogSession`:

```go
func (s *Store) LogSession(ctx context.Context, sess core.Session, ended time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_log (session_id, link_id, label, vm_id, vm_name, client_ip, started_at, ended_at, reason, recording)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.LinkID, sess.Label, sess.VMID, sess.VMName, sess.ClientIP, ts(sess.StartedAt), ts(ended), reason, sess.Recording)
	return err
}
```

In the spec file, replace the sentence about `core.Session` getting `Recording bool` and the nullable column with: "`core.Session` gets `Recording string`, the path of the first mp4 file (empty when not recording). `session_log` gets a `recording TEXT NOT NULL DEFAULT ''` column (added by a migration on startup) holding the same path."

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/core ./internal/live ./internal/store ./internal/admin ./internal/service -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal docs
git commit -m "feat(store): track the recording path on sessions and in the session log"
```

---

### Task 2: Folder and file naming

**Files:**
- Create: `internal/recorder/path.go`
- Test: `internal/recorder/path_test.go`

**Interfaces:**
- Produces: `Slug(label string) string`, `LinkDir(root string, linkID int64, label string) string`, `FileName(started time.Time, sessionID string, segment int) string`.

- [ ] **Step 1: Write the failing test**

`internal/recorder/path_test.go`:

```go
package recorder

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Mateo's Game #1":    "mateo-s-game-1",
		"../../etc/passwd":   "etc-passwd",
		"  ":                 "",
		"日本語":                "",
		"a/b\\c\x00d":        "a-b-c-d",
		strings.Repeat("a", 90): strings.Repeat("a", 40),
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLinkDirStaysUnderRoot(t *testing.T) {
	root := "/data/recordings"
	for _, label := range []string{"x", "../../etc", "/abs/path", "", ".."} {
		d := LinkDir(root, 7, label)
		if filepath.Dir(d) != root || !strings.HasPrefix(filepath.Base(d), "link-7") {
			t.Errorf("label %q gave %q", label, d)
		}
	}
	if got := LinkDir(root, 7, "Alpha Team"); got != "/data/recordings/link-7-alpha-team" {
		t.Fatalf("got %q", got)
	}
}

func TestFileName(t *testing.T) {
	at := time.Date(2026, 10, 4, 15, 30, 12, 0, time.UTC)
	if got := FileName(at, "ab12", 1); got != "20261004T153012Z_ab12.mp4" {
		t.Fatalf("got %q", got)
	}
	if got := FileName(at, "ab12", 3); got != "20261004T153012Z_ab12_part3.mp4" {
		t.Fatalf("got %q", got)
	}
	if got := FileName(at, "../x/y", 1); strings.ContainsAny(got, "/\\") {
		t.Fatalf("unsafe id leaked: %q", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/recorder -run 'Slug|LinkDir|FileName' -v`
Expected: FAIL (package has no Go files / undefined functions).

- [ ] **Step 3: Implement**

`internal/recorder/path.go`:

```go
// Package recorder records each playtest session to an mp4 by watching the VM over its own VNC connection.
package recorder

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	slugRe   = regexp.MustCompile(`[^a-z0-9]+`)
	unsafeID = regexp.MustCompile(`[^A-Za-z0-9]+`)
)

// Slug reduces a label to [a-z0-9-], at most 40 characters, so it can never form a path.
func Slug(label string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(label), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

// LinkDir is the per-link folder. The numeric ID keeps it unique and stable when labels repeat or change.
func LinkDir(root string, linkID int64, label string) string {
	name := "link-" + strconv.FormatInt(linkID, 10)
	if s := Slug(label); s != "" {
		name += "-" + s
	}
	return filepath.Join(root, name)
}

// FileName is the mp4 for one segment of a session. Segment 1 has no suffix.
func FileName(started time.Time, sessionID string, segment int) string {
	base := started.UTC().Format("20060102T150405Z") + "_" + unsafeID.ReplaceAllString(sessionID, "")
	if segment > 1 {
		base += "_part" + strconv.Itoa(segment)
	}
	return base + ".mp4"
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `gofmt -l internal/recorder; go test ./internal/recorder -count=1`
Expected: no gofmt output, PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/recorder
git commit -m "feat(recorder): per-link folder and file naming"
```

---

### Task 3: Framebuffer

**Files:**
- Create: `internal/recorder/framebuffer.go`
- Test: `internal/recorder/framebuffer_test.go`

**Interfaces:**
- Produces: `NewFramebuffer(w, h int) (*Framebuffer, error)`, `(*Framebuffer).Resize(w, h int) error`, `Size() (int, int)`, `Snapshot(dst []byte) ([]byte, int, int)` (BGRX, 4 bytes per pixel, copies under the lock, reuses `dst` capacity), `PutRaw(x, y, w, h int, src []byte) error`, `Copy(sx, sy, w, h, dx, dy int) error`. Limits: width and height 1..8192 and at most 16,000,000 pixels. Error `errBounds` for rectangles outside the screen.

- [ ] **Step 1: Write the failing tests**

`internal/recorder/framebuffer_test.go`:

```go
package recorder

import (
	"bytes"
	"testing"
)

func px(b, g, r byte) []byte { return []byte{b, g, r, 0} }

func TestNewFramebufferLimits(t *testing.T) {
	for _, c := range [][2]int{{0, 10}, {10, 0}, {-1, 5}, {8193, 10}, {5000, 5000}} {
		if _, err := NewFramebuffer(c[0], c[1]); err == nil {
			t.Errorf("%v accepted", c)
		}
	}
	if _, err := NewFramebuffer(3840, 2160); err != nil {
		t.Fatal(err)
	}
}

func TestPutRawAndSnapshot(t *testing.T) {
	fb, _ := NewFramebuffer(4, 3)
	src := bytes.Repeat(px(1, 2, 3), 2*2)
	if err := fb.PutRaw(1, 1, 2, 2, src); err != nil {
		t.Fatal(err)
	}
	snap, w, h := fb.Snapshot(nil)
	if w != 4 || h != 3 || len(snap) != 4*3*4 {
		t.Fatalf("snapshot %d %d %d", w, h, len(snap))
	}
	if !bytes.Equal(snap[(1*4+1)*4:(1*4+1)*4+4], px(1, 2, 3)) || !bytes.Equal(snap[0:4], px(0, 0, 0)) {
		t.Fatal("pixels wrong")
	}
	snap[0] = 99
	if again, _, _ := fb.Snapshot(nil); again[0] != 0 {
		t.Fatal("snapshot aliases the framebuffer")
	}
}

func TestPutRawBounds(t *testing.T) {
	fb, _ := NewFramebuffer(4, 3)
	for _, c := range [][4]int{{3, 0, 2, 1}, {0, 2, 1, 2}, {-1, 0, 1, 1}, {0, 0, 5, 1}} {
		if err := fb.PutRaw(c[0], c[1], c[2], c[3], make([]byte, c[2]*c[3]*4)); err == nil {
			t.Errorf("%v accepted", c)
		}
	}
	if err := fb.PutRaw(0, 0, 2, 2, make([]byte, 3)); err == nil {
		t.Error("short data accepted")
	}
}

func TestCopyOverlap(t *testing.T) {
	fb, _ := NewFramebuffer(4, 1)
	fb.PutRaw(0, 0, 4, 1, append(append(append(px(1, 0, 0), px(2, 0, 0)...), px(3, 0, 0)...), px(4, 0, 0)...))
	if err := fb.Copy(0, 0, 3, 1, 1, 0); err != nil {
		t.Fatal(err)
	}
	snap, _, _ := fb.Snapshot(nil)
	got := []byte{snap[0], snap[4], snap[8], snap[12]}
	if !bytes.Equal(got, []byte{1, 1, 2, 3}) {
		t.Fatalf("overlapping copy gave %v", got)
	}
	if err := fb.Copy(2, 0, 3, 1, 0, 0); err == nil {
		t.Fatal("source out of bounds accepted")
	}
}

func TestResizeClears(t *testing.T) {
	fb, _ := NewFramebuffer(2, 2)
	fb.PutRaw(0, 0, 1, 1, px(9, 9, 9))
	if err := fb.Resize(3, 1); err != nil {
		t.Fatal(err)
	}
	snap, w, h := fb.Snapshot(nil)
	if w != 3 || h != 1 || len(snap) != 12 || snap[0] != 0 {
		t.Fatalf("%d %d %v", w, h, snap)
	}
	if err := fb.Resize(0, 0); err == nil {
		t.Fatal("zero size accepted")
	}
	if w, h := fb.Size(); w != 3 || h != 1 {
		t.Fatal("failed resize changed the size")
	}
}

func FuzzFramebufferOps(f *testing.F) {
	f.Add(1, 1, 2, 2, 0, 0)
	f.Fuzz(func(t *testing.T, x, y, w, h, dx, dy int) {
		fb, _ := NewFramebuffer(16, 16)
		if w >= 0 && h >= 0 && w <= 64 && h <= 64 {
			fb.PutRaw(x, y, w, h, make([]byte, w*h*4))
		}
		fb.Copy(x, y, w, h, dx, dy)
	})
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/recorder -run 'Framebuffer|PutRaw|Copy|Resize' -v`
Expected: FAIL (undefined `NewFramebuffer`).

- [ ] **Step 3: Implement**

`internal/recorder/framebuffer.go`:

```go
package recorder

import (
	"errors"
	"sync"
)

const (
	maxDim    = 8192
	maxPixels = 16_000_000
)

var (
	errBounds = errors.New("recorder: rectangle outside the screen")
	errSize   = errors.New("recorder: unsupported screen size")
)

// Framebuffer is the VM screen as BGRX pixels (4 bytes each), written by the capture
// goroutine and read by the encoder.
type Framebuffer struct {
	mu   sync.Mutex
	w, h int
	pix  []byte
}

func checkSize(w, h int) error {
	if w < 1 || h < 1 || w > maxDim || h > maxDim || w*h > maxPixels {
		return errSize
	}
	return nil
}

func NewFramebuffer(w, h int) (*Framebuffer, error) {
	if err := checkSize(w, h); err != nil {
		return nil, err
	}
	return &Framebuffer{w: w, h: h, pix: make([]byte, w*h*4)}, nil
}

// Resize replaces the screen with a black one of the new size.
func (f *Framebuffer) Resize(w, h int) error {
	if err := checkSize(w, h); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.w, f.h, f.pix = w, h, make([]byte, w*h*4)
	return nil
}

func (f *Framebuffer) Size() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.w, f.h
}

// Snapshot copies the screen into dst (reusing its capacity) and returns it with the size it was taken at.
func (f *Framebuffer) Snapshot(dst []byte) ([]byte, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dst = append(dst[:0], f.pix...)
	return dst, f.w, f.h
}

func (f *Framebuffer) inside(x, y, w, h int) bool {
	return x >= 0 && y >= 0 && w >= 0 && h >= 0 && x <= f.w && y <= f.h && w <= f.w-x && h <= f.h-y
}

// PutRaw draws w*h BGRX pixels at (x, y).
func (f *Framebuffer) PutRaw(x, y, w, h int, src []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.inside(x, y, w, h) {
		return errBounds
	}
	if len(src) != w*h*4 {
		return errors.New("recorder: raw rectangle has the wrong length")
	}
	for row := 0; row < h; row++ {
		copy(f.pix[((y+row)*f.w+x)*4:], src[row*w*4:(row+1)*w*4])
	}
	return nil
}

// Copy moves a rectangle within the screen. The source is read fully first, so overlapping moves are safe.
func (f *Framebuffer) Copy(sx, sy, w, h, dx, dy int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.inside(sx, sy, w, h) || !f.inside(dx, dy, w, h) {
		return errBounds
	}
	tmp := make([]byte, w*h*4)
	for row := 0; row < h; row++ {
		copy(tmp[row*w*4:], f.pix[((sy+row)*f.w+sx)*4:((sy+row)*f.w+sx+w)*4])
	}
	for row := 0; row < h; row++ {
		copy(f.pix[((dy+row)*f.w+dx)*4:], tmp[row*w*4:(row+1)*w*4])
	}
	return nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/recorder -count=1 && go test ./internal/recorder -run xxx -fuzz FuzzFramebufferOps -fuzztime 10s`
Expected: PASS, fuzz finds nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/recorder
git commit -m "feat(recorder): bounded framebuffer with raw and copy-rect updates"
```

---

### Task 4: Fake VNC server that serves a screen

**Files:**
- Modify: `internal/rfb/rfbtest/rfbtest.go` (split `serve` so the handshake is reusable)
- Create: `internal/rfb/rfbtest/fb.go`
- Test: `internal/rfb/rfbtest/fb_test.go`

**Interfaces:**
- Produces: `rfbtest.NewFB(t testing.TB, password string, w, h int) *FB` with `Addr() string`, `Fill(r, g, b byte)` (paints the whole screen and marks it dirty), `Resize(w, h int)`, `Shared() bool` (the ClientInit flag of the last client), `Encodings() []int32` (from the last SetEncodings), `Clients() int` (currently connected). On every FramebufferUpdateRequest the server sends one update (a full Raw rectangle, preceded by a DesktopSize rectangle when the size changed), immediately for a non-incremental request or when the screen changed since the last update, otherwise it waits until it changes. Pixels are BGRX little-endian, matching the format the recorder requests.

- [ ] **Step 1: Write the failing test**

`internal/rfb/rfbtest/fb_test.go`:

```go
package rfbtest

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func dialFB(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	ver := make([]byte, 12)
	io.ReadFull(c, ver)
	c.Write(ver)
	sec := make([]byte, 2)
	io.ReadFull(c, sec)
	c.Write([]byte{1})
	res := make([]byte, 4)
	io.ReadFull(c, res)
	c.Write([]byte{1}) // shared
	si := make([]byte, 24)
	if _, err := io.ReadFull(c, si); err != nil {
		t.Fatal(err)
	}
	name := make([]byte, binary.BigEndian.Uint32(si[20:]))
	io.ReadFull(c, name)
	return c
}

func readUpdate(t *testing.T, c net.Conn) (rects int, firstEnc int32) {
	t.Helper()
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 0 {
		t.Fatalf("update header %v %v", hdr, err)
	}
	n := int(binary.BigEndian.Uint16(hdr[2:]))
	for i := 0; i < n; i++ {
		r := make([]byte, 12)
		io.ReadFull(c, r)
		w, h := int(binary.BigEndian.Uint16(r[4:])), int(binary.BigEndian.Uint16(r[6:]))
		enc := int32(binary.BigEndian.Uint32(r[8:]))
		if i == 0 {
			firstEnc = enc
		}
		if enc == 0 {
			io.ReadFull(c, make([]byte, w*h*4))
		}
	}
	return n, firstEnc
}

func TestFBServesUpdates(t *testing.T) {
	fb := NewFB(t, "", 8, 4)
	c := dialFB(t, fb.Addr())
	c.Write([]byte{2, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 1}) // SetEncodings [Raw, CopyRect]
	c.Write([]byte{3, 0, 0, 0, 0, 0, 0, 8, 0, 4})        // full request
	if n, enc := readUpdate(t, c); n != 1 || enc != 0 {
		t.Fatalf("first update %d %d", n, enc)
	}
	if !fb.Shared() {
		t.Fatal("shared flag not recorded")
	}
	if e := fb.Encodings(); len(e) != 2 || e[0] != 0 || e[1] != 1 {
		t.Fatalf("encodings %v", e)
	}

	c.Write([]byte{3, 1, 0, 0, 0, 0, 0, 8, 0, 4}) // incremental, nothing changed: must wait
	c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("incremental request answered without a change")
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	fb.Fill(1, 2, 3)
	readUpdate(t, c)

	c.Write([]byte{3, 1, 0, 0, 0, 0, 0, 8, 0, 4})
	fb.Resize(16, 6)
	if n, enc := readUpdate(t, c); n != 2 || enc != -223 {
		t.Fatalf("resize update %d %d", n, enc)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/rfb/rfbtest -run FB -v`
Expected: FAIL (undefined `NewFB`).

- [ ] **Step 3: Implement**

In `internal/rfb/rfbtest/rfbtest.go` replace the body of `serve` with a call to a new `handshake` and keep behaviour identical:

```go
func serve(c net.Conn, password string) {
	defer c.Close()
	if _, ok := handshake(c, password, 640, 480); !ok {
		return
	}
	io.Copy(c, c)
}

// handshake runs version, security and init, ending after ServerInit. It returns the ClientInit shared flag.
func handshake(c net.Conn, password string, w, h int) (shared, ok bool) {
	if _, err := c.Write([]byte("RFB 003.008\n")); err != nil {
		return false, false
	}
	var ver [12]byte
	if _, err := io.ReadFull(c, ver[:]); err != nil || string(ver[:]) != "RFB 003.008\n" {
		return false, false
	}
	var sel [1]byte
	if password == "" {
		c.Write([]byte{1, 1})
		if _, err := io.ReadFull(c, sel[:]); err != nil || sel[0] != 1 {
			return false, false
		}
	} else {
		c.Write([]byte{1, 2})
		if _, err := io.ReadFull(c, sel[:]); err != nil || sel[0] != 2 {
			return false, false
		}
		var ch [16]byte
		rand.Read(ch[:])
		c.Write(ch[:])
		var resp [16]byte
		if _, err := io.ReadFull(c, resp[:]); err != nil {
			return false, false
		}
		if !bytes.Equal(resp[:], encrypt(password, ch)) {
			msg := "bad password"
			out := binary.BigEndian.AppendUint32(nil, 1)
			out = binary.BigEndian.AppendUint32(out, uint32(len(msg)))
			c.Write(append(out, msg...))
			return false, false
		}
	}
	c.Write([]byte{0, 0, 0, 0})
	var ci [1]byte
	if _, err := io.ReadFull(c, ci[:]); err != nil {
		return false, false
	}
	name := "fake"
	si := binary.BigEndian.AppendUint16(nil, uint16(w))
	si = binary.BigEndian.AppendUint16(si, uint16(h))
	si = append(si, make([]byte, 16)...)
	si = binary.BigEndian.AppendUint32(si, uint32(len(name)))
	c.Write(append(si, name...))
	return ci[0] != 0, true
}
```

`internal/rfb/rfbtest/fb.go`:

```go
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
}

func NewFB(t testing.TB, password string, w, h int) *FB {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &FB{t: t, ln: ln, password: password, w: w, h: h, gen: 1}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(c)
		}
	}()
	return f
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
				reqs <- rq[0] != 0
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
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/rfb/... -count=1`
Expected: PASS, including every pre-existing rfb and gateway test (the refactor of `serve` must not change behaviour).

- [ ] **Step 5: Commit**

```bash
git add internal/rfb
git commit -m "test(rfb): fake VNC server that serves and resizes a screen"
```

---

### Task 5: VNC capture client

**Files:**
- Create: `internal/recorder/capture.go`
- Test: `internal/recorder/capture_test.go`

**Interfaces:**
- Consumes: `Framebuffer` (Task 3), `rfbtest.FB` (Task 4), `rfb.Dial` (existing: returns an authenticated conn positioned before ClientInit).
- Produces: `Run(ctx context.Context, conn net.Conn, ready func(*Framebuffer)) error`. Sends ClientInit (shared), reads ServerInit, creates the framebuffer, calls `ready(fb)` once, then applies updates until `ctx` ends (returns nil) or the connection fails or the server misbehaves (returns an error). Requests exactly one update at a time. Closes `conn` when `ctx` ends. Error `ErrProtocol` for unknown message types, unsupported encodings, rectangles outside the screen.

- [ ] **Step 1: Write the failing tests**

`internal/recorder/capture_test.go`:

```go
package recorder

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"quiver-playtesting/internal/rfb"
	"quiver-playtesting/internal/rfb/rfbtest"
)

func startRun(t *testing.T, addr string) (fbc chan *Framebuffer, errc chan error, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	conn, err := rfb.Dial(ctx, addr, "")
	if err != nil {
		t.Fatal(err)
	}
	fbc = make(chan *Framebuffer, 1)
	errc = make(chan error, 1)
	go func() { errc <- Run(ctx, conn, func(f *Framebuffer) { fbc <- f }) }()
	return fbc, errc, cancel
}

func waitPixel(t *testing.T, fb *Framebuffer, want [3]byte) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, _, _ := fb.Snapshot(nil)
		if snap[0] == want[0] && snap[1] == want[1] && snap[2] == want[2] {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap, _, _ := fb.Snapshot(nil)
	t.Fatalf("pixel %v, want %v", snap[:3], want)
}

func TestRunAppliesUpdatesAndResizes(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 8, 4)
	srv.Fill(30, 20, 10) // r, g, b
	fbc, errc, cancel := startRun(t, srv.Addr())
	fb := <-fbc
	if w, h := fb.Size(); w != 8 || h != 4 {
		t.Fatalf("size %d %d", w, h)
	}
	waitPixel(t, fb, [3]byte{10, 20, 30}) // BGR order in memory

	srv.Fill(5, 6, 7)
	waitPixel(t, fb, [3]byte{7, 6, 5})

	srv.Resize(16, 6)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if w, h := fb.Size(); w == 16 && h == 6 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w, h := fb.Size(); w != 16 || h != 6 {
		t.Fatalf("size after resize %d %d", w, h)
	}

	if !srv.Shared() {
		t.Fatal("capture must connect as a shared client")
	}
	if e := srv.Encodings(); len(e) != 3 || e[0] != 0 || e[1] != 1 || e[2] != -223 {
		t.Fatalf("encodings %v", e)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("clean stop returned %v", err)
	}
}

func TestRunServerDisconnect(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 8, 4)
	fbc, errc, _ := startRun(t, srv.Addr())
	<-fbc
	srv.Close()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("disconnect must be an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not notice the disconnect")
	}
}

// scripted plays a server over a pipe: it accepts the client's setup, then writes script.
func scripted(t *testing.T, w, h int, script func(c net.Conn)) error {
	t.Helper()
	cli, srv := net.Pipe()
	go func() {
		defer srv.Close()
		var ci [1]byte
		io.ReadFull(srv, ci[:])
		si := binary.BigEndian.AppendUint16(nil, uint16(w))
		si = binary.BigEndian.AppendUint16(si, uint16(h))
		si = append(si, make([]byte, 16)...)
		si = binary.BigEndian.AppendUint32(si, 0)
		srv.Write(si)
		io.CopyN(io.Discard, srv, 20+16+10) // SetPixelFormat, SetEncodings(3), first request
		script(srv)
	}()
	errc := make(chan error, 1)
	go func() { errc <- Run(context.Background(), cli, func(*Framebuffer) {}) }()
	select {
	case err := <-errc:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Run hung on hostile input")
		return nil
	}
}

func update(rects ...[]byte) []byte {
	out := []byte{0, 0}
	out = binary.BigEndian.AppendUint16(out, uint16(len(rects)))
	for _, r := range rects {
		out = append(out, r...)
	}
	return out
}

func rect(x, y, w, h int, enc int32, data []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(x))
	b = binary.BigEndian.AppendUint16(b, uint16(y))
	b = binary.BigEndian.AppendUint16(b, uint16(w))
	b = binary.BigEndian.AppendUint16(b, uint16(h))
	b = binary.BigEndian.AppendUint32(b, uint32(enc))
	return append(b, data...)
}

func TestRunRejectsHostileServers(t *testing.T) {
	cases := map[string]func(c net.Conn){
		"unknown message":      func(c net.Conn) { c.Write([]byte{99}) },
		"colour map":           func(c net.Conn) { c.Write([]byte{1, 0, 0, 0, 0, 0}) },
		"unsupported encoding": func(c net.Conn) { c.Write(update(rect(0, 0, 1, 1, 7, nil))) },
		"rect outside screen":  func(c net.Conn) { c.Write(update(rect(7, 3, 4, 4, 0, make([]byte, 64)))) },
		"giant desktop size":   func(c net.Conn) { c.Write(update(rect(0, 0, 65535, 65535, -223, nil))) },
		"giant cut text":       func(c net.Conn) { c.Write([]byte{3, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}) },
		"copyrect outside":     func(c net.Conn) { c.Write(update(rect(0, 0, 4, 4, 1, []byte{0, 99, 0, 99}))) },
	}
	for name, script := range cases {
		err := scripted(t, 8, 4, script)
		if !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: got %v, want ErrProtocol", name, err)
		}
	}
}

func TestRunAcceptsBellAndCutText(t *testing.T) {
	err := scripted(t, 8, 4, func(c net.Conn) {
		c.Write([]byte{2})
		c.Write([]byte{3, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c'})
		c.Write([]byte{99}) // ends the run so the test can observe it
	})
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("got %v", err)
	}
}
```

`rfbtest.FB` needs a `Close()` that closes the listener and every open client connection; add it in this task to `fb.go` (track conns in a slice under `f.mu`, close them in `Close`).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/recorder -run Run -v`
Expected: FAIL (undefined `Run`, `ErrProtocol`; `srv.Close` undefined).

- [ ] **Step 3: Implement**

`internal/recorder/capture.go`:

```go
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
			if err := fb.Resize(w, h); err != nil {
				return raw, protoErr("%v", err)
			}
		default:
			return raw, protoErr("encoding %d", int32(binary.BigEndian.Uint32(r[8:])))
		}
	}
	return raw, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/recorder ./internal/rfb/... -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal
git commit -m "feat(recorder): shared VNC capture client that mirrors the screen"
```

---

### Task 6: ffmpeg encoder and the recording loop

**Files:**
- Create: `internal/recorder/encoder.go`, `internal/recorder/priority_unix.go`, `internal/recorder/priority_other.go`
- Create: `internal/recorder/recorder.go`, `internal/recorder/diskfree_unix.go`, `internal/recorder/diskfree_other.go`
- Test: `internal/recorder/encoder_test.go`, `internal/recorder/recorder_test.go`

**Interfaces:**
- Consumes: `Run`, `Framebuffer.Snapshot/Size`, `FileName`, `LinkDir`.
- Produces:
  - `startEncoder(ffmpeg, out string, w, h, fps int) (*encoder, error)`, `(*encoder).WriteFrame(p []byte) error`, `(*encoder).Finish(timeout time.Duration) error`.
  - `recorder.Config{Dir, FFmpeg string; FPS, Max int; MinFree uint64}`, `recorder.New(cfg Config) *Recorder`, `(*Recorder).Enabled() bool`, `(*Recorder).Start(ctx context.Context, s core.Session, vm core.VM) (path string, stop func())`. `path` is empty when this session is not recorded. `stop` is always non-nil, is idempotent, and returns only after the file is finalised (bounded to about 15 seconds).

- [ ] **Step 1: Write the failing tests**

`internal/recorder/encoder_test.go`:

```go
package recorder

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func needFFmpeg(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	a, err1 := exec.LookPath("ffmpeg")
	b, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg and ffprobe are not installed")
	}
	return a, b
}

type probe struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func ffprobe(t *testing.T, bin, file string) (w, h int, dur float64) {
	t.Helper()
	out, err := exec.Command(bin, "-v", "error", "-show_entries", "stream=width,height:format=duration", "-of", "json", file).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", file, err)
	}
	var p probe
	if err := json.Unmarshal(out, &p); err != nil || len(p.Streams) == 0 {
		t.Fatalf("probe output %s %v", out, err)
	}
	dur, _ = strconv.ParseFloat(p.Format.Duration, 64)
	return p.Streams[0].Width, p.Streams[0].Height, dur
}

func TestEncoderWritesPlayableMP4(t *testing.T) {
	ff, fp := needFFmpeg(t)
	out := filepath.Join(t.TempDir(), "a.mp4")
	enc, err := startEncoder(ff, out, 100, 60, 10)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 100*60*4)
	for i := 0; i < 30; i++ {
		frame[0] = byte(i * 8)
		if err := enc.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := enc.Finish(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	w, h, dur := ffprobe(t, fp, out)
	if w != 100 || h != 60 || dur < 2.0 || dur > 4.5 {
		t.Fatalf("got %dx%d %.2fs", w, h, dur)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestEncoderOddSizeIsRoundedDown(t *testing.T) {
	ff, fp := needFFmpeg(t)
	out := filepath.Join(t.TempDir(), "odd.mp4")
	enc, err := startEncoder(ff, out, 101, 61, 10)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 101*61*4)
	for i := 0; i < 10; i++ {
		enc.WriteFrame(frame)
		time.Sleep(100 * time.Millisecond)
	}
	if err := enc.Finish(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if w, h, _ := ffprobe(t, fp, out); w != 100 || h != 60 {
		t.Fatalf("got %dx%d", w, h)
	}
}

func TestEncoderFinishKillsAHungChild(t *testing.T) {
	sh := filepath.Join(t.TempDir(), "hang.sh")
	os.WriteFile(sh, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755)
	enc, err := startEncoder(sh, filepath.Join(t.TempDir(), "x.mp4"), 4, 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	enc.Finish(300 * time.Millisecond)
	if time.Since(start) > 3*time.Second {
		t.Fatal("Finish did not kill the hung encoder")
	}
}

func TestStartEncoderMissingBinary(t *testing.T) {
	if _, err := startEncoder("/nonexistent/ffmpeg", filepath.Join(t.TempDir(), "x.mp4"), 4, 4, 10); err == nil {
		t.Fatal("missing binary accepted")
	}
}
```

`internal/recorder/recorder_test.go`:

```go
package recorder

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"quiver-playtesting/internal/core"
	"quiver-playtesting/internal/rfb/rfbtest"
)

// fakeEncoder is a shell script that swallows frames, so loop logic can be tested without ffmpeg.
func fakeEncoder(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script encoder")
	}
	p := filepath.Join(t.TempDir(), "ffmpeg")
	os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755)
	return p
}

func vmFor(t *testing.T, srv *rfbtest.FB) core.VM {
	t.Helper()
	host, port := splitAddr(t, srv.Addr())
	return core.VM{ID: 1, Name: "vm", Host: host, Port: port}
}

func newRec(t *testing.T, ffmpeg string, max int) (*Recorder, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "recordings")
	r := New(Config{Dir: dir, FFmpeg: ffmpeg, FPS: 10, Max: max, MinFree: 1})
	if !r.Enabled() {
		t.Fatal("recorder should be enabled")
	}
	return r, dir
}

func session() core.Session {
	return core.Session{ID: "ab12cd34", LinkID: 5, Label: "Alpha Team", VMID: 1, StartedAt: time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)}
}

func TestDisabledWithoutFFmpeg(t *testing.T) {
	r := New(Config{Dir: t.TempDir(), FFmpeg: "/nonexistent/ffmpeg", FPS: 10, Max: 1})
	if r.Enabled() {
		t.Fatal("must be disabled")
	}
	path, stop := r.Start(t.Context(), session(), core.VM{})
	if path != "" || stop == nil {
		t.Fatalf("%q %v", path, stop)
	}
	stop()
}

func TestStartCreatesFolderAndFile(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 64, 48)
	r, dir := newRec(t, fakeEncoder(t, "cat >/dev/null"), 2)
	path, stop := r.Start(t.Context(), session(), vmFor(t, srv))
	if path == "" {
		t.Fatal("not recording")
	}
	want := filepath.Join(dir, "link-5-alpha-team", "20261004T150000Z_ab12cd34.mp4")
	if path != want {
		t.Fatalf("path %q want %q", path, want)
	}
	time.Sleep(300 * time.Millisecond)
	stop()
	stop() // idempotent
	if st, err := os.Stat(filepath.Dir(path)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("folder %v %v", st, err)
	}
	if srv.Clients() != 0 {
		t.Fatal("capture connection left open")
	}
}

func TestConcurrentCap(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 64, 48)
	r, _ := newRec(t, fakeEncoder(t, "cat >/dev/null"), 1)
	p1, stop1 := r.Start(t.Context(), session(), vmFor(t, srv))
	s2 := session()
	s2.ID, s2.LinkID = "ee55", 6
	p2, stop2 := r.Start(t.Context(), s2, vmFor(t, srv))
	if p1 == "" || p2 != "" {
		t.Fatalf("p1=%q p2=%q", p1, p2)
	}
	stop2()
	stop1()
	p3, stop3 := r.Start(t.Context(), s2, vmFor(t, srv))
	if p3 == "" {
		t.Fatal("slot not released after stop")
	}
	stop3()
}

func TestLowDiskSkipsRecording(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 64, 48)
	r, _ := newRec(t, fakeEncoder(t, "cat >/dev/null"), 1)
	r.free = func(string) (uint64, error) { return 0, nil }
	if p, stop := r.Start(t.Context(), session(), vmFor(t, srv)); p != "" {
		t.Fatalf("recorded on a full disk: %q", p)
	} else {
		stop()
	}
}

func TestVMUnreachableSkipsRecording(t *testing.T) {
	r, _ := newRec(t, fakeEncoder(t, "cat >/dev/null"), 1)
	if p, stop := r.Start(t.Context(), session(), core.VM{Host: "127.0.0.1", Port: 1}); p != "" {
		t.Fatalf("recorded without a VM: %q", p)
	} else {
		stop()
	}
}

func TestEncoderCrashDoesNotHang(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 64, 48)
	r, _ := newRec(t, fakeEncoder(t, "exit 1"), 1)
	path, stop := r.Start(t.Context(), session(), vmFor(t, srv))
	if path == "" {
		t.Fatal("start should succeed, the crash comes later")
	}
	done := make(chan struct{})
	go func() { time.Sleep(500 * time.Millisecond); stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop hung after the encoder crashed")
	}
	// the slot is free again
	if p, s := r.Start(t.Context(), session(), vmFor(t, srv)); p == "" {
		t.Fatal("slot leaked after a crash")
	} else {
		s()
	}
}

func TestVMDiesMidRecording(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 64, 48)
	r, _ := newRec(t, fakeEncoder(t, "cat >/dev/null"), 1)
	_, stop := r.Start(t.Context(), session(), vmFor(t, srv))
	time.Sleep(300 * time.Millisecond)
	srv.Close()
	done := make(chan struct{})
	go func() { time.Sleep(500 * time.Millisecond); stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop hung after the VM died")
	}
}

func TestResolutionChangeStartsANewFile(t *testing.T) {
	ff, fp := needFFmpeg(t)
	srv := rfbtest.NewFB(t, "", 64, 48)
	srv.Fill(10, 20, 30)
	r, _ := newRec(t, ff, 1)
	path, stop := r.Start(t.Context(), session(), vmFor(t, srv))
	if path == "" {
		t.Fatal("not recording")
	}
	time.Sleep(1200 * time.Millisecond)
	srv.Resize(96, 64)
	time.Sleep(1500 * time.Millisecond)
	stop()
	part2 := strings.TrimSuffix(path, ".mp4") + "_part2.mp4"
	if w, h, _ := ffprobe(t, fp, path); w != 64 || h != 48 {
		t.Fatalf("first file %dx%d", w, h)
	}
	if w, h, _ := ffprobe(t, fp, part2); w != 96 || h != 64 {
		t.Fatalf("second file %dx%d", w, h)
	}
}

func TestEndToEndDuration(t *testing.T) {
	ff, fp := needFFmpeg(t)
	srv := rfbtest.NewFB(t, "", 128, 96)
	srv.Fill(200, 100, 50)
	r, _ := newRec(t, ff, 1)
	path, stop := r.Start(t.Context(), session(), vmFor(t, srv))
	time.Sleep(3 * time.Second)
	stop()
	_, _, dur := ffprobe(t, fp, path)
	if dur < 2.0 || dur > 4.5 {
		t.Fatalf("duration %.2fs for a 3s session", dur)
	}
}
```

Add a helper at the bottom of `recorder_test.go`:

```go
func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return host, n
}
```

(with `net` and `strconv` added to the imports.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/recorder -count=1 2>&1 | head -20`
Expected: FAIL (undefined `startEncoder`, `New`, `Config`).

- [ ] **Step 3: Implement the encoder**

`internal/recorder/encoder.go`:

```go
package recorder

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// capBuf keeps the first bytes of ffmpeg's stderr for the log and drops the rest.
type capBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (c *capBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := 2048 - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capBuf) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

type encoder struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	stderr *capBuf
	done   chan error
}

// startEncoder runs ffmpeg reading raw BGRA frames of w*h from stdin and writing fragmented H.264 mp4 to out.
// Fragmented output stays playable if the process dies mid-session.
func startEncoder(ffmpeg, out string, w, h, fps int) (*encoder, error) {
	// Created 0600 up front: ffmpeg truncates an existing file and keeps its mode.
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "rawvideo", "-pixel_format", "bgra", "-video_size", fmt.Sprintf("%dx%d", w, h),
		"-use_wallclock_as_timestamps", "1", "-i", "pipe:0",
		"-vf", fmt.Sprintf("fps=%d,scale=trunc(iw/2)*2:trunc(ih/2)*2", fps),
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "30", "-pix_fmt", "yuv420p",
		"-g", strconv.Itoa(fps * 2),
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-y", out,
	}
	cmd := exec.Command(ffmpeg, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	e := &encoder{cmd: cmd, stderr: &capBuf{}, done: make(chan error, 1)}
	cmd.Stderr = e.stderr
	if e.in, err = cmd.StdinPipe(); err != nil {
		os.Remove(out)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		os.Remove(out)
		return nil, err
	}
	lowerPriority(cmd.Process.Pid)
	go func() { e.done <- cmd.Wait() }()
	return e, nil
}

func (e *encoder) WriteFrame(p []byte) error {
	_, err := e.in.Write(p)
	return err
}

// Finish closes the input so ffmpeg writes the end of the file, and kills it if it takes longer than timeout.
func (e *encoder) Finish(timeout time.Duration) error {
	e.in.Close()
	select {
	case err := <-e.done:
		if err != nil {
			return fmt.Errorf("ffmpeg: %w: %s", err, e.stderr.String())
		}
		return nil
	case <-time.After(timeout):
		e.cmd.Process.Kill()
		<-e.done
		return fmt.Errorf("ffmpeg did not finish within %s", timeout)
	}
}
```

`internal/recorder/priority_unix.go`:

```go
//go:build unix

package recorder

import "syscall"

// lowerPriority makes the encoder yield to the gateway, which shares a small host.
func lowerPriority(pid int) { _ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19) }
```

`internal/recorder/priority_other.go`:

```go
//go:build !unix

package recorder

func lowerPriority(int) {}
```

`internal/recorder/diskfree_unix.go`:

```go
//go:build unix

package recorder

import "syscall"

func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
```

`internal/recorder/diskfree_other.go`:

```go
//go:build !unix

package recorder

import "math"

func freeBytes(string) (uint64, error) { return math.MaxUint64, nil }
```

- [ ] **Step 4: Implement the recorder**

`internal/recorder/recorder.go`:

```go
package recorder

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"quiver-playtesting/internal/core"
	"quiver-playtesting/internal/rfb"
)

const (
	dialWait   = 3 * time.Second
	finishWait = 10 * time.Second
	diskEvery  = 30 * time.Second
)

var errResized = errors.New("screen size changed")

type Config struct {
	Dir     string
	FFmpeg  string
	FPS     int
	Max     int
	MinFree uint64
}

type Recorder struct {
	cfg     Config
	enabled bool
	sem     chan struct{}
	dial    func(ctx context.Context, addr, password string) (net.Conn, error)
	free    func(dir string) (uint64, error)
}

// New resolves ffmpeg and prepares the recordings directory. When either fails the recorder is
// disabled and says why in the log, so the gateway still starts.
func New(cfg Config) *Recorder {
	if cfg.FPS <= 0 {
		cfg.FPS = 10
	}
	if cfg.Max <= 0 {
		cfg.Max = 2
	}
	r := &Recorder{cfg: cfg, sem: make(chan struct{}, cfg.Max), dial: rfb.Dial, free: freeBytes}
	bin, err := exec.LookPath(cfg.FFmpeg)
	if err != nil {
		slog.Warn("recording disabled: ffmpeg not found", "ffmpeg", cfg.FFmpeg)
		return r
	}
	r.cfg.FFmpeg = bin
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		slog.Warn("recording disabled: cannot create the recordings directory", "dir", cfg.Dir, "err", err)
		return r
	}
	_ = os.Chmod(cfg.Dir, 0o700)
	r.enabled = true
	return r
}

func (r *Recorder) Enabled() bool { return r != nil && r.enabled }

func (r *Recorder) lowDisk() bool {
	free, err := r.free(r.cfg.Dir)
	return err == nil && free < r.cfg.MinFree
}

// Start begins recording the session. It returns the path of the first file, or "" when this session
// is not being recorded, and a stop function that is always safe to call and returns once the file is finished.
func (r *Recorder) Start(ctx context.Context, s core.Session, vm core.VM) (string, func()) {
	noop := func() {}
	if !r.Enabled() {
		return "", noop
	}
	select {
	case r.sem <- struct{}{}:
	default:
		slog.Warn("recording skipped: concurrent recording limit reached", "session", s.ID)
		return "", noop
	}
	release := func() { <-r.sem }
	if r.lowDisk() {
		slog.Warn("recording skipped: low disk space", "session", s.ID)
		release()
		return "", noop
	}
	dir := LinkDir(r.cfg.Dir, s.LinkID, s.Label)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("recording skipped: cannot create the folder", "session", s.ID, "err", err)
		release()
		return "", noop
	}
	_ = os.Chmod(dir, 0o700)
	dctx, cancel := context.WithTimeout(ctx, dialWait)
	conn, err := r.dial(dctx, net.JoinHostPort(vm.Host, strconv.Itoa(vm.Port)), vm.Password)
	cancel()
	if err != nil {
		slog.Warn("recording skipped: cannot reach the VM", "session", s.ID, "err", err)
		release()
		return "", noop
	}

	rctx, rcancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer release()
		defer func() {
			if v := recover(); v != nil {
				slog.Error("recorder panic", "session", s.ID, "panic", v)
			}
		}()
		if err := r.record(rctx, conn, dir, s); err != nil && rctx.Err() == nil {
			slog.Warn("recording ended early", "session", s.ID, "err", err)
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(rcancel)
		<-done
	}
	return filepath.Join(dir, FileName(s.StartedAt, s.ID, 1)), stop
}

func (r *Recorder) record(ctx context.Context, conn net.Conn, dir string, s core.Session) error {
	ectx, ecancel := context.WithCancel(ctx)
	defer ecancel()
	ready := make(chan *Framebuffer, 1)
	capErr := make(chan error, 1)
	go func() {
		err := Run(ectx, conn, func(f *Framebuffer) { ready <- f })
		capErr <- err
		ecancel()
	}()
	var fb *Framebuffer
	select {
	case fb = <-ready:
	case err := <-capErr:
		return err
	}
	loopErr := r.encodeLoop(ectx, fb, dir, s)
	ecancel()
	if err := <-capErr; err != nil && loopErr == nil {
		loopErr = err
	}
	return loopErr
}

// encodeLoop writes one file per screen size, sampling the framebuffer at a fixed rate.
func (r *Recorder) encodeLoop(ctx context.Context, fb *Framebuffer, dir string, s core.Session) error {
	tick := time.NewTicker(time.Second / time.Duration(r.cfg.FPS))
	defer tick.Stop()
	disk := time.NewTicker(diskEvery)
	defer disk.Stop()
	var buf []byte
	for seg := 1; ; seg++ {
		w, h := fb.Size()
		enc, err := startEncoder(r.cfg.FFmpeg, filepath.Join(dir, FileName(s.StartedAt, s.ID, seg)), w, h, r.cfg.FPS)
		if err != nil {
			return err
		}
		err = func() error {
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-disk.C:
					if r.lowDisk() {
						return errors.New("low disk space")
					}
				case <-tick.C:
					var cw, ch int
					buf, cw, ch = fb.Snapshot(buf)
					if cw != w || ch != h {
						return errResized
					}
					if err := enc.WriteFrame(buf); err != nil {
						return err
					}
				}
			}
		}()
		if ferr := enc.Finish(finishWait); ferr != nil && err == nil {
			err = ferr
		}
		if !errors.Is(err, errResized) {
			return err
		}
	}
}
```

- [ ] **Step 5: Run to verify they pass**

Run: `go vet ./internal/recorder && go test ./internal/recorder -count=1 -race -v 2>&1 | tail -40`
Expected: PASS. The tests that need ffmpeg run here because ffmpeg is installed on this machine (`/opt/homebrew/bin/ffmpeg`); they skip elsewhere.

- [ ] **Step 6: Commit**

```bash
git add internal/recorder
git commit -m "feat(recorder): ffmpeg encoder and per-session recording loop"
```

---

### Task 7: Retention pruning

**Files:**
- Create: `internal/recorder/prune.go`
- Test: `internal/recorder/prune_test.go`

**Interfaces:**
- Produces: `Prune(dir string, keep time.Duration, now time.Time) (removed int, err error)`. Deletes regular `*.mp4` files whose modification time is older than `now-keep` inside folders named `link-*`, then removes those folders when empty. Never follows symlinks and never touches anything outside `link-*` folders.

- [ ] **Step 1: Write the failing test**

`internal/recorder/prune_test.go`:

```go
package recorder

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func touch(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	os.Chtimes(path, mt, mt)
}

func TestPrune(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "link-1-a", "old.mp4"), 40*24*time.Hour)
	touch(t, filepath.Join(root, "link-1-a", "new.mp4"), time.Hour)
	touch(t, filepath.Join(root, "link-2-b", "old.mp4"), 40*24*time.Hour)
	touch(t, filepath.Join(root, "link-2-b", "notes.txt"), 40*24*time.Hour)
	touch(t, filepath.Join(root, "other", "old.mp4"), 40*24*time.Hour)
	outside := filepath.Join(t.TempDir(), "secret.mp4")
	touch(t, outside, 90*24*time.Hour)
	os.Symlink(outside, filepath.Join(root, "link-1-a", "evil.mp4"))

	n, err := Prune(root, 30*24*time.Hour, time.Now())
	if err != nil || n != 2 {
		t.Fatalf("removed %d err %v", n, err)
	}
	for path, want := range map[string]bool{
		filepath.Join(root, "link-1-a", "old.mp4"):    false,
		filepath.Join(root, "link-1-a", "new.mp4"):    true,
		filepath.Join(root, "link-2-b", "old.mp4"):    false,
		filepath.Join(root, "link-2-b", "notes.txt"):  true,
		filepath.Join(root, "other", "old.mp4"):       true,
		filepath.Join(root, "link-1-a", "evil.mp4"):   true,
		outside:                                       true,
	} {
		_, err := os.Lstat(path)
		if (err == nil) != want {
			t.Errorf("%s exists=%v want %v", path, err == nil, want)
		}
	}
}

func TestPruneEmptiesFolders(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "link-9-z", "old.mp4"), 40*24*time.Hour)
	if _, err := Prune(root, 24*time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "link-9-z")); !os.IsNotExist(err) {
		t.Fatal("empty folder kept")
	}
}

func TestPruneMissingDir(t *testing.T) {
	if n, err := Prune(filepath.Join(t.TempDir(), "nope"), time.Hour, time.Now()); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/recorder -run Prune -v`
Expected: FAIL (undefined `Prune`).

- [ ] **Step 3: Implement**

`internal/recorder/prune.go`:

```go
package recorder

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Prune deletes recordings older than keep and folders left empty. It only looks inside link-* folders
// and only removes regular .mp4 files, so a symlink or a stray file can never lead it elsewhere.
func Prune(dir string, keep time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-keep)
	removed := 0
	for _, d := range entries {
		if !d.IsDir() || !strings.HasPrefix(d.Name(), "link-") {
			continue
		}
		sub := filepath.Join(dir, d.Name())
		files, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.Type().IsRegular() || !strings.HasSuffix(f.Name(), ".mp4") {
				continue
			}
			info, err := f.Info()
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			if os.Remove(filepath.Join(sub, f.Name())) == nil {
				removed++
			}
		}
		_ = os.Remove(sub) // fails harmlessly when files remain
	}
	return removed, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `gofmt -l internal/recorder; go test ./internal/recorder -count=1 -race`
Expected: no gofmt output, PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/recorder
git commit -m "feat(recorder): delete recordings past the retention window"
```

---

### Task 8: Gateway hook and header notice

**Files:**
- Modify: `internal/gateway/gateway.go` (Config, `serveIndex`, `serveWS`)
- Modify: `web/static/index.html`, `web/static/assets/style.css`
- Test: `internal/gateway/recording_test.go`

**Interfaces:**
- Consumes: `live.Registry.SetRecording` (Task 1).
- Produces: `gateway.Recorder` interface and `Config.Recorder`:

```go
type Recorder interface {
	Enabled() bool
	Start(ctx context.Context, s core.Session, vm core.VM) (path string, stop func())
}
```

`*recorder.Recorder` satisfies it (the gateway does not import the recorder package).

- [ ] **Step 1: Write the failing tests**

`internal/gateway/recording_test.go`:

```go
package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"quiver-playtesting/internal/core"
	"quiver-playtesting/internal/rfb/rfbtest"
)

type fakeRec struct {
	enabled bool
	path    string
	mu      sync.Mutex
	starts  int
	stops   int
	vmSeen  core.VM
}

func (f *fakeRec) Enabled() bool { return f.enabled }

func (f *fakeRec) Start(_ context.Context, _ core.Session, vm core.VM) (string, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.vmSeen = vm
	return f.path, func() { f.mu.Lock(); f.stops++; f.mu.Unlock() }
}

func (f *fakeRec) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

func getIndex(t *testing.T, e *env) string {
	t.Helper()
	resp, err := http.Get(e.srv.URL + "/s/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestRecordingNoticeFollowsEnabled(t *testing.T) {
	on := setup(t, Config{Recorder: &fakeRec{enabled: true}})
	if body := getIndex(t, on); !strings.Contains(body, "This session is recorded") || strings.Contains(body, "<!--rec-notice-->") {
		t.Fatalf("notice missing or placeholder left:\n%s", body)
	}
	for name, cfg := range map[string]Config{"nil recorder": {}, "disabled": {Recorder: &fakeRec{}}} {
		e := setup(t, cfg)
		if body := getIndex(t, e); strings.Contains(body, "recorded") || strings.Contains(body, "<!--rec-notice-->") {
			t.Fatalf("%s: notice shown or placeholder left", name)
		}
	}
}

func TestSessionStartsAndStopsRecording(t *testing.T) {
	rec := &fakeRec{enabled: true, path: "/rec/a.mp4"}
	e := setup(t, Config{Recorder: rec})
	e.b.add("tok1", 1, rfbtest.Server(t, "pw"), "pw")
	ws, _, err := e.dial(t, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	s := waitSessions(t, e.reg, 1)[0]
	if s.Recording != "/rec/a.mp4" {
		t.Fatalf("registry session %+v", s)
	}
	if st, _ := rec.counts(); st != 1 {
		t.Fatalf("starts %d", st)
	}
	if rec.vmSeen.Password != "pw" || rec.vmSeen.ID != 1 {
		t.Fatalf("recorder got %+v", rec.vmSeen)
	}
	nc.Close()
	waitLog(t, e.b)
	if st, sp := rec.counts(); st != 1 || sp != 1 {
		t.Fatalf("starts %d stops %d", st, sp)
	}
}

func TestSessionWorksWhenNotRecorded(t *testing.T) {
	rec := &fakeRec{enabled: true} // Start returns "" like a full disk or the cap
	e := setup(t, Config{Recorder: rec})
	e.b.add("tok1", 1, rfbtest.Server(t, "pw"), "pw")
	ws, _, err := e.dial(t, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	if s := waitSessions(t, e.reg, 1)[0]; s.Recording != "" {
		t.Fatalf("session marked as recording: %+v", s)
	}
	nc.Close()
	waitLog(t, e.b)
}

func TestKillStillStopsRecording(t *testing.T) {
	rec := &fakeRec{enabled: true, path: "/rec/a.mp4"}
	e := setup(t, Config{Recorder: rec})
	e.b.add("tok1", 1, rfbtest.Server(t, "pw"), "pw")
	ws, _, _ := e.dial(t, "tok1")
	handshake(t, ws)
	s := waitSessions(t, e.reg, 1)[0]
	e.reg.Kill(s.ID, "killed")
	waitLog(t, e.b)
	if _, sp := rec.counts(); sp != 1 {
		t.Fatalf("stops %d", sp)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gateway -run 'Recording|Recorded' -v`
Expected: FAIL (unknown field `Recorder` in `Config`).

- [ ] **Step 3: Implement the gateway change**

In `internal/gateway/gateway.go` add to the `Config` struct:

```go
	// Recorder, when set and Enabled, records every session and turns on the header notice.
	Recorder Recorder
```

and the interface next to `Backend`:

```go
type Recorder interface {
	Enabled() bool
	Start(ctx context.Context, s core.Session, vm core.VM) (path string, stop func())
}
```

In `serveWS`, right after the `rfb.Accept(nc)` error check and before `limit, limitReason := ...`, add:

```go
	if h.cfg.Recorder != nil && h.cfg.Recorder.Enabled() {
		if path, stop := h.cfg.Recorder.Start(ctx, sess, vm); path != "" {
			defer stop()
			sess.Recording = path
			h.reg.SetRecording(sess.ID, path)
		} else {
			stop()
		}
	}
```

The deferred `stop` runs before the earlier session-log defer (LIFO), so the file is finished before the session is logged, and the log defer reads the updated `sess.Recording` because it captures the variable.

Replace `serveIndex` body so the placeholder is filled per request (the file is small and embedded):

```go
func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	page, err := fs.ReadFile(h.static, "index.html")
	if err != nil {
		notFound(w)
		return
	}
	notice := ""
	if h.cfg.Recorder != nil && h.cfg.Recorder.Enabled() {
		notice = `<div class="rec"><span class="rec-dot"></span>This session is recorded</div>`
	}
	page = bytes.Replace(page, []byte("<!--rec-notice-->"), []byte(notice), 1)
	hd := w.Header()
	hd.Set("Content-Security-Policy", staticCSP)
	hd.Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(page))
}
```

Add `"bytes"` to the imports (and drop `"io"` only if nothing else uses it; `serveAsset` still uses `io.ReadSeeker`).

`web/static/index.html`: replace `<div class="label">Playtesting</div>` with:

```html
<div class="right"><!--rec-notice--><div class="label">Playtesting</div></div>
```

`web/static/assets/style.css`: add after the `.label::before` rule (line 10):

```css
.right { display: flex; align-items: center; gap: 24px; }
.rec { display: flex; align-items: center; gap: 8px; font-family: "IBM Plex Mono", ui-monospace, Menlo, monospace; font-weight: 500; font-size: 11px; letter-spacing: 0.08em; text-transform: uppercase; color: var(--paper); opacity: 0.75; }
.rec-dot { width: 8px; height: 8px; border-radius: 50%; background: #E5484D; }
```

and extend the existing small-screen rule (`@media (max-width: 560px)`) with `.right { gap: 12px; } .rec { font-size: 10px; }`.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/gateway ./web/... -count=1 -race`
Expected: PASS, including the existing `TestStaticRoutes` and every other gateway test.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway web
git commit -m "feat(gateway): record sessions and show the recording notice in the header"
```

---

### Task 9: Flags, retention loop, TUI marker

**Files:**
- Modify: `cmd/quiver-playtest/main.go`
- Modify: `internal/tui/tui.go`
- Test: `internal/tui/tui_test.go`, `cmd/quiver-playtest/main_test.go`

**Interfaces:**
- Consumes: `recorder.New`, `recorder.Config`, `recorder.Prune` (Tasks 6, 7), `gateway.Config.Recorder` (Task 8).

- [ ] **Step 1: Write the failing tests**

Append to `internal/tui/tui_test.go`:

```go
func TestSessionsTableMarksRecording(t *testing.T) {
	m := newTestModel(&fakeSvc{})
	m.sessions = []core.Session{
		{ID: "a", Label: "one", VMName: "vm", ClientIP: "1.1.1.1", StartedAt: time.Now(), Recording: "/rec/a.mp4"},
		{ID: "b", Label: "two", VMName: "vm", ClientIP: "2.2.2.2", StartedAt: time.Now()},
	}
	m.syncRows()
	rows := m.tables[tabSessions].Rows()
	if len(rows) != 2 || rows[0][4] != "REC" || rows[1][4] != "" {
		t.Fatalf("rows %v", rows)
	}
}
```


`cmd/quiver-playtest/main_test.go` (append):

```go
func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"2GiB": 2 << 30, "512MiB": 512 << 20, "1048576": 1 << 20, "1GB": 1_000_000_000} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "-1GiB", "1TiB2"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) accepted", bad)
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tui ./cmd/... -count=1`
Expected: FAIL (`parseSize` undefined, REC column missing).

- [ ] **Step 3: Implement**

`internal/tui/tui.go`: add a column to the sessions table (line 88) and a value to each row in `syncRows`:

```go
	m.tables[tabSessions] = newTable([]table.Column{{Title: "Label", Width: 22}, {Title: "VM", Width: 18}, {Title: "Client IP", Width: 18}, {Title: "Running", Width: 10}, {Title: "Rec", Width: 5}})
```

```go
	for _, s := range m.sessions {
		rec := ""
		if s.Recording != "" {
			rec = "REC"
		}
		rows = append(rows, table.Row{s.Label, s.VMName, s.ClientIP, age(now.Sub(s.StartedAt)), rec})
	}
```

`cmd/quiver-playtest/main.go`: add the import `"quiver-playtesting/internal/recorder"`, `"strconv"`, `"strings"` (if absent) and these flags in `serve` after `continuous`:

```go
	recordOn := fs.Bool("recordings", envBool("QP_RECORDINGS", true), "record every session to an mp4 (needs ffmpeg)")
	recordDir := fs.String("recordings-dir", envOr("QP_RECORDINGS_DIR", ""), "where recordings go, default <data>/recordings")
	ffmpegBin := fs.String("ffmpeg", envOr("QP_FFMPEG", "ffmpeg"), "ffmpeg binary")
	recordFPS := fs.Int("record-fps", envInt("QP_RECORD_FPS", 10), "recording frame rate")
	recordMax := fs.Int("recordings-max", envInt("QP_RECORDINGS_MAX", 2), "max concurrent recordings")
	recordFree := fs.String("recordings-min-free", envOr("QP_RECORDINGS_MIN_FREE", "2GiB"), "stop recording below this much free disk")
	recordKeep := fs.Duration("recordings-retention", envDuration("QP_RECORDINGS_RETENTION", 720*time.Hour), "delete recordings older than this, 0 keeps everything")
```

`envInt` and `envDuration` do not exist yet. Add them next to `envBool` with the same shape (`strconv.Atoi` / `time.ParseDuration` on `os.Getenv(key)`, falling back to the default on empty or invalid input).

After `svc := service.New(st, reg)`:

```go
	var rec *recorder.Recorder
	if *recordOn {
		minFree, err := parseSize(*recordFree)
		if err != nil {
			return fmt.Errorf("--recordings-min-free: %w", err)
		}
		dir := *recordDir
		if dir == "" {
			dir = filepath.Join(*data, "recordings")
		}
		rec = recorder.New(recorder.Config{Dir: dir, FFmpeg: *ffmpegBin, FPS: *recordFPS, Max: *recordMax, MinFree: minFree})
		if rec.Enabled() && *recordKeep > 0 {
			go recordingPruneLoop(ctx, dir, *recordKeep)
		}
	}
```

`ctx` is created after the gateway config today (`signal.NotifyContext` below it): move the `ctx, stop := signal.NotifyContext(...)` and `defer stop()` lines above this block. In the `gateway.Config{...}` literal add:

```go
		Recorder: recorderOrNil(rec),
```

with this helper, which avoids a typed-nil interface when recording is off:

```go
func recorderOrNil(r *recorder.Recorder) gateway.Recorder {
	if r == nil {
		return nil
	}
	return r
}
```

Add the prune loop beside `pruneLoop` and the size parser:

```go
func recordingPruneLoop(ctx context.Context, dir string, keep time.Duration) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := recorder.Prune(dir, keep, time.Now()); err != nil {
			slog.Warn("recordings prune failed", "err", err)
		} else if n > 0 {
			slog.Info("recordings pruned", "files", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// parseSize reads a byte count such as 2GiB, 512MiB, 1GB or a plain number.
func parseSize(s string) (uint64, error) {
	units := []struct {
		suffix string
		mult   uint64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1000}}
	mult := uint64(1)
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `gofmt -l . ; go vet ./... && go test ./... -count=1 -race`
Expected: no gofmt output, all packages PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd internal
git commit -m "feat: recording flags, retention loop and REC marker in the TUI"
```

---

### Task 10: Docs, end-to-end check against a real VM, pull request

**Files:**
- Modify: `README.md`, `ARROW.md`, `docs/contracts.md`

- [ ] **Step 1: Document**

`README.md`: add a section "Session recordings" after "Security model and flags" with: what is recorded (every session, connect to disconnect, one mp4 per connection in `<data>/recordings/link-<id>-<label>/`), the `ffmpeg` requirement (`apt install ffmpeg` on Debian), the seven flags and env vars with defaults, the limits (2 concurrent, 2 GiB free floor, 30 days retention), the header notice, and that recordings contain whatever the playtester does so the directory is operator-only. `ARROW.md`: add one line under requirements stating ffmpeg must be installed on the host for recordings and that without it the gateway runs unrecorded. `docs/contracts.md`: add the `recording` field to the Session JSON and the `session_log.recording` column. No dash punctuation in any of the text.

- [ ] **Step 2: Whole-repo verification**

Run: `gofmt -l . ; go vet ./... && go test ./... -count=1 -race && go build ./...`
Expected: clean output, all PASS.

- [ ] **Step 3: End-to-end against the real Linux VM**

```bash
D=$(mktemp -d)
go run ./cmd/quiver-playtest serve --listen 127.0.0.1:8480 --public-host 127.0.0.1:8490 --data $D --recordings-dir $D/rec &
```

Add the Linux VM (`10.10.128.200:5900`, VNC password from the existing e2e helper `/tmp/e2etmp`, which creates a VM and link and prints the token), open `http://127.0.0.1:8480/s/<token>` in headless Chrome for about 20 seconds (the `/tmp/fps2.mjs` script already does this), then close it. Check:

```bash
ls -la $D/rec/link-*/
ffprobe -v error -show_entries stream=width,height:format=duration -of default=nw=1 $D/rec/link-*/*.mp4
```

Expected: one folder `link-<id>-<label>`, one mp4 mode 0600 in a 0700 folder, the VM's resolution, duration close to the session length. Open one frame (`ffmpeg -i file.mp4 -frames:v 1 frame.png`) and look at it to confirm it shows the desktop. Repeat once against the Windows VM (`10.10.128.201`, TightVNC) to confirm the second server also works. Stop the gateway.

- [ ] **Step 4: Commit and open the PR**

```bash
git add README.md ARROW.md docs
git commit -m "docs: session recordings"
git push -u origin feat/session-recording
gh pr create --repo char2cs/quiver.playtesting --title "feat: record every session to an mp4" --body "<summary of the feature, the ffmpeg requirement, defaults, and the e2e result from Step 3>"
```

End the PR description with the attribution line the harness requires. Do not merge and do not deploy: both need the operator's approval (the `main` ruleset requires a review, and `.101` needs ffmpeg installed first).
