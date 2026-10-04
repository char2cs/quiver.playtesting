// Package live tracks running sessions and fans out start/end events.
package live

import (
	"context"
	"errors"
	"sort"
	"sync"

	"quiver-playtesting/internal/core"
)

const subBuffer = 64

type entry struct {
	sess   core.Session
	cancel context.CancelCauseFunc
}

type Registry struct {
	mu       sync.Mutex
	sessions map[string]*entry
	subs     map[int]chan core.Event
	nextSub  int
}

func New() *Registry {
	return &Registry{sessions: map[string]*entry{}, subs: map[int]chan core.Event{}}
}

// Start registers a session. It returns core.ErrBusy if the link or the VM already has a live session.
func (r *Registry) Start(s core.Session, cancel context.CancelCauseFunc) error {
	if cancel == nil {
		cancel = func(error) {}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.sessions[s.ID]; dup {
		return core.ErrBusy
	}
	for _, e := range r.sessions {
		if e.sess.LinkID == s.LinkID || e.sess.VMID == s.VMID {
			return core.ErrBusy
		}
	}
	r.sessions[s.ID] = &entry{sess: s, cancel: cancel}
	r.emit(core.Event{Type: "start", Session: s})
	return nil
}

func (r *Registry) End(id string, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.sessions[id]
	if !ok {
		return
	}
	delete(r.sessions, id)
	r.emit(core.Event{Type: "end", Session: e.sess, Reason: reason})
}

func (r *Registry) SetRecording(id, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.sessions[id]; ok {
		e.sess.Recording = path
	}
}

func (r *Registry) Kill(id, reason string) error {
	r.mu.Lock()
	e, ok := r.sessions[id]
	r.mu.Unlock()
	if !ok {
		return core.ErrNotFound
	}
	e.cancel(errors.New(reason))
	return nil
}

func (r *Registry) KillByLink(linkID int64, reason string) {
	r.killWhere(reason, func(s core.Session) bool { return s.LinkID == linkID })
}

func (r *Registry) KillByVM(vmID int64, reason string) {
	r.killWhere(reason, func(s core.Session) bool { return s.VMID == vmID })
}

func (r *Registry) KillAll(reason string) {
	r.killWhere(reason, func(core.Session) bool { return true })
}

func (r *Registry) killWhere(reason string, match func(core.Session) bool) {
	r.mu.Lock()
	var cancels []context.CancelCauseFunc
	for _, e := range r.sessions {
		if match(e.sess) {
			cancels = append(cancels, e.cancel)
		}
	}
	r.mu.Unlock()
	for _, c := range cancels {
		c(errors.New(reason))
	}
}

func (r *Registry) List() []core.Session {
	r.mu.Lock()
	out := make([]core.Session, 0, len(r.sessions))
	for _, e := range r.sessions {
		out = append(out, e.sess)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

func (r *Registry) Get(id string) (core.Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.sessions[id]
	if !ok {
		return core.Session{}, false
	}
	return e.sess, true
}

// Subscribe returns a buffered event channel. Events are dropped for slow consumers.
func (r *Registry) Subscribe() (<-chan core.Event, func()) {
	ch := make(chan core.Event, subBuffer)
	r.mu.Lock()
	id := r.nextSub
	r.nextSub++
	r.subs[id] = ch
	r.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.subs, id)
			close(ch)
			r.mu.Unlock()
		})
	}
}

// emit must be called with r.mu held.
func (r *Registry) emit(ev core.Event) {
	for _, ch := range r.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
