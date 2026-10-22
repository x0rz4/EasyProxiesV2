package subscription

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/store"
)

type notModifiedFailureStore struct {
	store.Store
	err error
}

type failingAuditRuntime struct{ err error }

func (r failingAuditRuntime) CurrentPortMap() map[string]uint16                         { return nil }
func (r failingAuditRuntime) ReloadWithPortMap(*config.Config, map[string]uint16) error { return r.err }

func TestRefreshReportsRuntimeReconcileFailure(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("http://node.example:80#node"))
	}))
	defer server.Close()
	reloadErr := errors.New("injected reload failure")
	mgr := New(&config.Config{}, nil, WithStore(db), WithHTTPClient(server.Client()))
	defer mgr.Stop()
	sub, err := mgr.Create(t.Context(), store.Subscription{Name: "audit", URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	mgr.boxMgr = failingAuditRuntime{err: reloadErr}
	if err := mgr.RefreshOne(t.Context(), sub.ID); !errors.Is(err, reloadErr) {
		t.Fatalf("refresh error = %v", err)
	}
	if mgr.Status().LastError == "" {
		t.Fatal("status reported success before failed runtime reconciliation")
	}
}

func (s notModifiedFailureStore) RecordSubscriptionNotModified(context.Context, int64, time.Time) error {
	return s.err
}

func TestRefreshNotModifiedPersistence(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "write_failure"
		}
		t.Run(name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "audit.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotModified)
			}))
			defer server.Close()
			var persistence store.Store = db
			writeErr := errors.New("injected 304 write failure")
			if fail {
				persistence = notModifiedFailureStore{Store: db, err: writeErr}
			}
			mgr := New(&config.Config{}, nil, WithStore(persistence), WithHTTPClient(server.Client()))
			defer mgr.Stop()
			sub, err := mgr.Create(t.Context(), store.Subscription{Name: "audit", URL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			err = mgr.RefreshOne(t.Context(), sub.ID)
			if fail {
				if !errors.Is(err, writeErr) {
					t.Fatalf("refresh error = %v, want write failure", err)
				}
				if mgr.Status().LastError == "" {
					t.Fatal("refresh status concealed persistence failure")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				current, err := mgr.Get(t.Context(), sub.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.LastAttempt.IsZero() || !current.LastSuccess.IsZero() || current.NodeCount != 0 || mgr.Status().LastError != "" {
					t.Fatalf("304 changed snapshot or failed to record attempt: %+v", current)
				}
			}
		})
	}
}

func TestRefreshRejectsResultAfterSourceChange(t *testing.T) {
	for _, scenario := range []string{"url", "round_trip", "format", "user_agent", "304", "failure", "delete", "cancel", "rename"} {
		t.Run(scenario, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "source.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			entered, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				if scenario == "304" {
					w.WriteHeader(http.StatusNotModified)
					return
				}
				if scenario == "failure" {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				_, _ = w.Write([]byte("http://old.example:80#old"))
			}))
			defer server.Close()
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			mgr := New(&config.Config{}, nil, WithStore(db), WithHTTPClient(server.Client()))
			defer mgr.Stop()
			sub, err := mgr.Create(t.Context(), store.Subscription{Name: "before", URL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() { done <- mgr.RefreshOne(ctx, sub.ID) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("fetch did not start")
			}
			input := store.Subscription{Name: "after", URL: server.URL}
			switch scenario {
			case "delete":
				if err := mgr.Delete(t.Context(), sub.ID); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			default:
				switch scenario {
				case "format":
					input.Format = "clash"
				case "user_agent":
					input.UserAgent = "new-client"
				case "rename":
				default:
					input.URL += "/new"
				}
				if _, err := mgr.Update(t.Context(), sub.ID, input); err != nil {
					t.Fatal(err)
				}
				if scenario == "round_trip" {
					input.URL = server.URL
					if _, err := mgr.Update(t.Context(), sub.ID, input); err != nil {
						t.Fatal(err)
					}
				}
			}
			close(release)
			err = <-done
			if scenario == "rename" {
				if err != nil {
					t.Fatal(err)
				}
				current, err := mgr.Get(t.Context(), sub.ID)
				if err != nil || current.Name != "after" || current.NodeCount != 1 {
					t.Fatalf("rename or snapshot lost: %+v %v", current, err)
				}
				return
			}
			if err == nil {
				t.Fatal("old source result reported success")
			}
			if scenario == "delete" {
				nodes, err := db.ListNodes(t.Context(), store.NodeFilter{})
				if err != nil || len(nodes) != 0 {
					t.Fatalf("deleted subscription left nodes: %v %v", nodes, err)
				}
				return
			}
			if count := subscriptionNodeCount(t, mgr, sub.ID); count != 0 {
				t.Fatalf("old source repopulated %d nodes", count)
			}
			current, err := mgr.Get(t.Context(), sub.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !current.LastAttempt.IsZero() || !current.LastSuccess.IsZero() || current.LastError != "" {
				t.Fatalf("discarded result wrote refresh metadata: %+v", current)
			}
		})
	}
}
