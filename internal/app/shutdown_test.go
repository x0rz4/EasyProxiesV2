package app

import (
	"context"
	"errors"
	"testing"
)

func TestShutdownBudgetPreservesResourceOrder(t *testing.T) {
	producer, cancelProducer := context.WithCancel(t.Context())
	s := &shutdownStack{cancel: cancelProducer}
	storeClosed := make(chan struct{})
	workerEntered, releaseWorker := make(chan struct{}), make(chan struct{})
	s.add(func(context.Context) error { close(storeClosed); return nil })
	s.add(func(context.Context) error {
		<-producer.Done()
		close(workerEntered)
		<-releaseWorker // Model a third-party Close without context support.
		return nil
	})
	budget, cancelBudget := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- s.close(budget) }()
	<-workerEntered
	cancelBudget()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-storeClosed:
		t.Fatal("store closed underneath running worker")
	default:
	}
	close(releaseWorker)
	<-s.done
	<-storeClosed
	if err := s.close(t.Context()); err != nil {
		t.Fatal(err)
	}
}
