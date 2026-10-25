package controlplane

import (
	"context"
	"errors"
	"time"
)

const (
	apiKeyUseInterval   = time.Minute
	apiKeyUseRetry      = 5 * time.Second
	maxAPIKeyUseEntries = 1024
)

type apiKeyUse struct {
	pending time.Time
	next    time.Time
	seen    time.Time
}

// apiKeyUsage is guarded by Auth.mu. One entry per key replaces per-request
// goroutines. The worker is lazy so session-only services own no extra worker.
type apiKeyUsage struct {
	entries      map[int64]apiKeyUse
	generation   uint64
	wake         chan struct{}
	done         chan struct{}
	cancel       context.CancelFunc
	closed       bool
	closeCtx     context.Context
	closeErr     error
	dropped      uint64
	lastOverflow time.Time
}

func (a *Auth) recordAPIKeyUse(id int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u := &a.usage
	if id <= 0 || a.store == nil || u.closed {
		return
	}
	now := a.now()
	if u.entries == nil {
		u.entries = make(map[int64]apiKeyUse)
	}
	entry, exists := u.entries[id]
	if !exists && len(u.entries) >= maxAPIKeyUseEntries {
		for key, old := range u.entries {
			if old.pending.IsZero() && !now.Before(old.next) {
				delete(u.entries, key)
			}
		}
		if len(u.entries) >= maxAPIKeyUseEntries {
			u.dropped++
			if now.Sub(u.lastOverflow) >= time.Minute {
				a.logger.Printf("[auth] API key usage queue full (%d keys); metadata observations dropped: %d", maxAPIKeyUseEntries, u.dropped)
				u.lastOverflow = now
			}
			return
		}
	}
	entry.pending = maxTimestamp(entry.pending, maxTimestamp(entry.seen, now))
	entry.seen = maxTimestamp(entry.seen, now)
	u.entries[id] = entry
	if u.done == nil {
		ctx, cancel := context.WithCancel(context.Background())
		u.cancel, u.wake, u.done = cancel, make(chan struct{}, 1), make(chan struct{})
		go a.runAPIKeyUsage(ctx)
	}
	if !now.Before(entry.next) {
		select {
		case u.wake <- struct{}{}:
		default:
		}
	}
}

func (a *Auth) runAPIKeyUsage(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() {
		a.mu.Lock()
		final := a.usage.closeCtx
		a.mu.Unlock()
		var err error
		if final != nil {
			err = a.flushAPIKeyUsage(final, true)
		}
		a.mu.Lock()
		a.usage.closeErr = err
		close(a.usage.done)
		a.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.usage.wake:
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			return
		}
		_ = a.flushAPIKeyUsage(ctx, false)
	}
}

// flushAPIKeyUsage has a single caller at a time: the owned worker, including
// its final flush. Failures retain the newest pending observation for retry.
func (a *Auth) flushAPIKeyUsage(parent context.Context, final bool) error {
	a.mu.Lock()
	now, st, generation := a.now(), a.store, a.usage.generation
	batch := make(map[int64]time.Time)
	for id, entry := range a.usage.entries {
		if !entry.pending.IsZero() && (final || !now.Before(entry.next)) {
			batch[id] = entry.pending
		}
		if entry.pending.IsZero() && now.Sub(entry.seen) > 10*time.Minute {
			delete(a.usage.entries, id)
		}
	}
	a.mu.Unlock()
	if st == nil {
		return nil
	}
	var result error
	for id, observed := range batch {
		if err := parent.Err(); err != nil {
			return err
		}
		call, cancel := context.WithTimeout(parent, storeCallTimeout)
		err := st.TouchAPIKey(call, id, observed)
		cancel()
		a.mu.Lock()
		if entry, exists := a.usage.entries[id]; exists && generation == a.usage.generation {
			entry.next = a.now().Add(apiKeyUseRetry)
			if err == nil {
				entry.next = a.now().Add(apiKeyUseInterval)
				if !entry.pending.After(observed) {
					entry.pending = time.Time{}
				}
			}
			a.usage.entries[id] = entry
		}
		a.mu.Unlock()
		if err != nil {
			a.logger.Printf("[auth] record API key usage (key %d), pending retry: %v", id, err)
			result = errors.Join(result, err)
		}
	}
	return result
}

// Close stops accepting usage observations, cancels an active periodic write,
// and saves the newest pending timestamps within the supplied exit budget.
func (a *Auth) Close(ctx context.Context) error {
	a.mu.Lock()
	if !a.usage.closed {
		a.usage.closed, a.usage.closeCtx = true, ctx
		if a.usage.cancel != nil {
			a.usage.cancel()
		}
	}
	done := a.usage.done
	a.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.usage.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func maxTimestamp(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
