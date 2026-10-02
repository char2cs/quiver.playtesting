package gateway

import (
	"sync"
	"time"
)

const (
	maxFails      = 10
	failWindow    = time.Minute
	banDuration   = 5 * time.Minute
	maxLimiterIPs = 10000
)

type ipState struct {
	fails       int
	windowStart time.Time
	bannedUntil time.Time
}

// limiter bans keys after too many failures in a window. Memory is bounded by maxLimiterIPs.
type limiter struct {
	mu        sync.Mutex
	m         map[string]*ipState
	now       func() time.Time
	lastSweep time.Time
}

func newLimiter() *limiter {
	return &limiter{m: map[string]*ipState{}, now: time.Now}
}

func (l *limiter) banned(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.m[key]
	return s != nil && l.now().Before(s.bannedUntil)
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	s := l.m[key]
	if s == nil {
		if len(l.m) >= maxLimiterIPs {
			l.evict(now)
		}
		s = &ipState{windowStart: now}
		l.m[key] = s
	}
	if now.Sub(s.windowStart) > failWindow {
		s.fails, s.windowStart = 0, now
	}
	s.fails++
	if s.fails >= maxFails {
		s.bannedUntil = now.Add(banDuration)
		s.fails, s.windowStart = 0, now
	}
}

func (l *limiter) evict(now time.Time) {
	if now.Sub(l.lastSweep) > time.Second {
		l.lastSweep = now
		for k, s := range l.m {
			if now.After(s.bannedUntil) && now.Sub(s.windowStart) > failWindow {
				delete(l.m, k)
			}
		}
		if len(l.m) < maxLimiterIPs {
			return
		}
	}
	for k, s := range l.m {
		if !now.Before(s.bannedUntil) {
			delete(l.m, k)
			return
		}
	}
	for k := range l.m {
		delete(l.m, k)
		return
	}
}

func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}
