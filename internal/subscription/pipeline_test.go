package subscription

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/store"
)

func TestRefreshPipelineBackpressureAndEarlyCommit(t *testing.T) {
	for _, slow := range []int{0, 2} {
		t.Run(fmt.Sprint(slow), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			db, err := store.Open(filepath.Join(t.TempDir(), "pipeline.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			started := make(chan int, 20)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
				started <- i
				if i == slow {
					<-r.Context().Done()
					return
				}
				fmt.Fprintf(w, "http://node-%d.example:80#node-%d", i, i)
			}))
			defer server.Close()
			committed := make(chan struct{}, 20)
			mgr := New(&config.Config{}, nil, WithStore(db), WithHTTPClient(server.Client()), WithRefreshObserver(func() { committed <- struct{}{} }))
			defer mgr.Stop()
			for i := range 12 {
				if err := db.CreateSubscription(ctx, &store.Subscription{Name: fmt.Sprint(i), URL: fmt.Sprintf("%s/%d", server.URL, i), Enabled: true, SortOrder: i, RefreshTimeoutSeconds: 60}); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- mgr.refreshDue(ctx, false) }()
			for range fetchConcurrency {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("initial window did not start")
				}
			}
			if slow == 0 {
				select {
				case i := <-started:
					t.Fatalf("download %d escaped bounded window", i)
				case <-time.After(100 * time.Millisecond):
				}
			} else {
				// A later slow provider cannot prevent an earlier commit.
				select {
				case <-committed:
				case <-time.After(5 * time.Second):
					t.Fatal("commit waited for all fetches")
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("pipeline workers did not exit")
			}
			if mgr.Status().IsRefreshing {
				t.Fatal("in-flight state leaked")
			}
		})
	}
}
