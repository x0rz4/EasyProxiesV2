package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"easy_proxies/internal/store"
)

type taskPersistenceStore struct {
	store.Store
	write func(context.Context, *store.NodeDetectionTask) error
}

func (s taskPersistenceStore) UpsertNodeDetectionTask(ctx context.Context, task *store.NodeDetectionTask) error {
	return s.write(ctx, task)
}

func (s taskPersistenceStore) PruneNodeDetectionTasks(context.Context, int) error { return nil }

func TestNodeCheckPersistenceCoalescesProgressAndPreservesTerminal(t *testing.T) {
	writes := 0
	var status string
	db := taskPersistenceStore{write: func(_ context.Context, task *store.NodeDetectionTask) error {
		writes++
		status = task.Status
		return nil
	}}
	m := newNodeCheckManager(&Server{store: db})
	defer m.shutdown()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	task := &nodeCheckTask{ctx: ctx, snapshot: nodeCheckTaskSnapshot{ID: "test", Status: nodeCheckTaskRunning}}
	for range 100 {
		if err := m.persist(task); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 1 {
		t.Fatalf("progress writes=%d", writes)
	}
	cancel()
	task.snapshot.Status = nodeCheckTaskCancelled
	if err := m.persist(task); err != nil {
		t.Fatal(err)
	}
	if writes != 2 || status != nodeCheckTaskCancelled {
		t.Fatalf("writes=%d final=%s", writes, status)
	}
}

func TestNodeCheckPersistenceTimeoutIsVisible(t *testing.T) {
	db := taskPersistenceStore{write: func(ctx context.Context, _ *store.NodeDetectionTask) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	m := newNodeCheckManager(&Server{store: db})
	defer m.shutdown()
	m.persistTimeout = time.Millisecond
	task := &nodeCheckTask{ctx: t.Context(), snapshot: nodeCheckTaskSnapshot{ID: "blocked", Status: nodeCheckTaskRunning}}
	if err := m.persist(task); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("persist error=%v", err)
	}
	if task.copySnapshot().PersistenceError == "" {
		t.Fatal("persistence failure was hidden")
	}
}

func TestNodeCheckCancellationInterruptsPendingWrite(t *testing.T) {
	entered := make(chan struct{})
	db := taskPersistenceStore{write: func(ctx context.Context, _ *store.NodeDetectionTask) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	m := newNodeCheckManager(&Server{store: db})
	defer m.shutdown()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	task := &nodeCheckTask{ctx: ctx, snapshot: nodeCheckTaskSnapshot{ID: "cancel", Status: nodeCheckTaskRunning}}
	done := make(chan error, 1)
	go func() { done <- m.persist(task) }()
	<-entered
	// A concurrent progress report is coalesced instead of waiting on the DB.
	if err := m.persist(task); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write=%v", err)
	}
}
