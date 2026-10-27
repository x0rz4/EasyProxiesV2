package controlplane

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"easy_proxies/internal/store"
)

type delayedSessionStore struct {
	store.Store
	started, release chan struct{}
	once             sync.Once
}

func (s *delayedSessionStore) GetSession(ctx context.Context, token string) (*store.Session, error) {
	row, err := s.Store.GetSession(ctx, token)
	s.once.Do(func() { close(s.started); <-s.release })
	return row, err
}

func TestRevokeSessionCannotBeResurrectedByInFlightStoreRead(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	writer := NewAuth(AuthOptions{Store: db})
	session, err := writer.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	delayed := &delayedSessionStore{Store: db, started: make(chan struct{}), release: make(chan struct{})}
	auth := NewAuth(AuthOptions{Store: delayed})
	readDone := make(chan bool, 1)
	go func() { readDone <- auth.Validate(ctx, session.Token) }()
	<-delayed.started
	revokeStarted := make(chan struct{})
	revokeDone := make(chan error, 1)
	go func() { close(revokeStarted); revokeDone <- auth.RevokeSession(ctx, session.Token) }()
	<-revokeStarted
	close(delayed.release)
	<-readDone // the request already in flight is allowed to complete
	if err := <-revokeDone; err != nil {
		t.Fatal(err)
	}
	if auth.Validate(ctx, session.Token) {
		t.Fatal("revoked session was cached again")
	}
	restarted := NewAuth(AuthOptions{Store: db})
	if restarted.Validate(ctx, session.Token) {
		t.Fatal("revoked session survived restart")
	}
}

type failingSessionDeleteStore struct {
	store.Store
	failure error
	bounded bool
}

func (s *failingSessionDeleteStore) DeleteSession(ctx context.Context, token string) error {
	_, s.bounded = ctx.Deadline()
	if s.failure != nil {
		return s.failure
	}
	return s.Store.DeleteSession(ctx, token)
}

func TestRevokeSessionReportsDurableFailureAndCanRetry(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	failure := errors.New("synthetic delete failure")
	st := &failingSessionDeleteStore{Store: db, failure: failure}
	auth := NewAuth(AuthOptions{Store: st})
	session, err := auth.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.RevokeSession(context.Background(), session.Token); !errors.Is(err, failure) {
		t.Fatalf("lost deletion error: %v", err)
	}
	if !st.bounded {
		t.Fatal("durable deletion had no deadline")
	}
	if !auth.Validate(context.Background(), session.Token) {
		t.Fatal("failed revocation reported local success")
	}
	st.failure = nil
	if err := auth.RevokeSession(context.Background(), session.Token); err != nil {
		t.Fatal(err)
	}
	if auth.Validate(context.Background(), session.Token) {
		t.Fatal("retry did not revoke")
	}
}

func TestSessionStoreWaitHonorsCancellation(t *testing.T) {
	auth := NewAuth(AuthOptions{})
	auth.sessionStoreGate <- struct{}{}
	defer func() { <-auth.sessionStoreGate }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := auth.RevokeSession(ctx, "fixture"); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait ignored cancellation: %v", err)
	}
	if auth.Validate(ctx, "fixture") {
		t.Fatal("cancelled cache miss authenticated")
	}
}
