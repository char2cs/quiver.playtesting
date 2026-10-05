package live

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"quiver-playtesting/internal/core"
)

func sess(id string, link, vm int64) core.Session {
	return core.Session{ID: id, LinkID: link, VMID: vm, StartedAt: time.Now()}
}

func TestBusyRules(t *testing.T) {
	r := New()
	if err := r.Start(sess("a", 1, 10), nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(sess("b", 1, 11), nil); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("same link: %v", err)
	}
	if err := r.Start(sess("c", 2, 10), nil); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("same vm: %v", err)
	}
	if err := r.Start(sess("a", 3, 12), nil); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("dup id: %v", err)
	}
	if err := r.Start(sess("d", 2, 11), nil); err != nil {
		t.Fatal(err)
	}
	r.End("a", "closed")
	if err := r.Start(sess("e", 1, 10), nil); err != nil {
		t.Fatalf("after end: %v", err)
	}
}

func TestEndIdempotentAndEvents(t *testing.T) {
	r := New()
	ch, unsub := r.Subscribe()
	defer unsub()
	_ = r.Start(sess("a", 1, 1), nil)
	r.End("a", "closed")
	r.End("a", "closed")
	ev := <-ch
	if ev.Type != "start" || ev.Session.ID != "a" {
		t.Fatalf("got %+v", ev)
	}
	ev = <-ch
	if ev.Type != "end" || ev.Reason != "closed" {
		t.Fatalf("got %+v", ev)
	}
	select {
	case ev := <-ch:
		t.Fatalf("unexpected second end: %+v", ev)
	default:
	}
	if _, ok := r.Get("a"); ok {
		t.Fatal("still present")
	}
}

func TestKill(t *testing.T) {
	r := New()
	ctx, cancel := context.WithCancelCause(context.Background())
	_ = r.Start(sess("a", 1, 1), cancel)
	if err := r.Kill("nope", "x"); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if err := r.Kill("a", "killed"); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	if got := context.Cause(ctx); got == nil || got.Error() != "killed" {
		t.Fatalf("cause %v", got)
	}
}

func TestKillByLinkAndVM(t *testing.T) {
	r := New()
	c1, x1 := context.WithCancelCause(context.Background())
	c2, x2 := context.WithCancelCause(context.Background())
	_ = r.Start(sess("a", 1, 10), x1)
	_ = r.Start(sess("b", 2, 20), x2)
	r.KillByLink(1, "revoked")
	if c1.Err() == nil || c2.Err() != nil {
		t.Fatal("link kill wrong target")
	}
	if context.Cause(c1).Error() != "revoked" {
		t.Fatal("cause")
	}
	r.KillByVM(20, "vm_removed")
	if c2.Err() == nil || context.Cause(c2).Error() != "vm_removed" {
		t.Fatal("vm kill")
	}
	r.KillByVM(999, "x")
	r.KillByLink(999, "x")
}

func TestSlowSubscriberNeverBlocks(t *testing.T) {
	r := New()
	_, unsub := r.Subscribe()
	defer unsub()
	done := make(chan struct{})
	go func() {
		for i := 0; i < subBuffer*5; i++ {
			id := string(rune('a' + i%26))
			_ = r.Start(core.Session{ID: id, LinkID: 1, VMID: 1}, nil)
			r.End(id, "closed")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("registry blocked on slow subscriber")
	}
}

func TestUnsubscribeTwiceAndList(t *testing.T) {
	r := New()
	ch, unsub := r.Subscribe()
	unsub()
	unsub()
	if _, ok := <-ch; ok {
		t.Fatal("channel not closed")
	}
	now := time.Now()
	_ = r.Start(core.Session{ID: "late", LinkID: 1, VMID: 1, StartedAt: now.Add(time.Second)}, nil)
	_ = r.Start(core.Session{ID: "early", LinkID: 2, VMID: 2, StartedAt: now}, nil)
	l := r.List()
	if len(l) != 2 || l[0].ID != "early" {
		t.Fatalf("%+v", l)
	}
}

func TestConcurrent(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := int64(0); i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := string(rune('A' + i))
			if r.Start(core.Session{ID: id, LinkID: i, VMID: i}, nil) == nil {
				r.List()
				_ = r.Kill(id, "x")
				r.End(id, "x")
			}
		}()
	}
	wg.Wait()
	if len(r.List()) != 0 {
		t.Fatal("leak")
	}
}

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
