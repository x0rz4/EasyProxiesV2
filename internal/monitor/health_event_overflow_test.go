package monitor

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"easy_proxies/internal/group"
)

// Characterization of the unresolved overflow defect, not an endorsement of
// lossy health delivery. Replace these assertions when delivery is redesigned.
func TestHealthEventOverflowLosesTransitionsAndSnapshotMarker(t *testing.T) {
	for _, success := range []bool{false, true} {
		name := "failure"
		if success {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				group.Reset()
				defer group.Reset()
				group.Register(901, time.Minute, 1, "node", map[string]group.GroupInitialState{
					"node": {NodeID: 1, Evicted: true, EvictedAt: time.Now().Add(-time.Hour)},
				}, group.RecoveryPolicy{Enabled: true, SuccessThreshold: 2})
				group.RecordHealthSuccess(901, "node")
				entered, release := make(chan struct{}), make(chan struct{})
				var received []HealthResultEvent
				s := newHealthEventSubscription(func(event HealthResultEvent) {
					if len(received) == 0 {
						close(entered)
						<-release
					}
					received = append(received, event)
					if event.SnapshotOnly {
						return
					}
					if event.Success {
						group.RecordHealthSuccess(901, "node")
					} else {
						group.RecordGroupHealthFailure(901, "node", errors.New("probe failed"), event.CheckedAt)
					}
				}, nil, nil, true)
				defer s.close()
				s.enqueue(HealthResultEvent{SnapshotOnly: true})
				<-entered
				for i := 1; i <= healthEventQueueLimit; i++ {
					s.enqueue(HealthResultEvent{SnapshotOnly: true, NodeID: int64(i)})
				}
				s.enqueue(HealthResultEvent{Success: success, CheckedAt: time.Now()})
				s.enqueue(HealthResultEvent{SnapshotOnly: true, NodeID: -1})
				if s.Dropped() != 2 {
					t.Fatalf("dropped = %d", s.Dropped())
				}
				close(release)
				synctest.Wait()
				if len(received) != healthEventQueueLimit+1 {
					t.Fatalf("received %d events", len(received))
				}
				for i, event := range received {
					if event.NodeID != int64(i) {
						t.Fatalf("FIFO at %d: %+v", i, event)
					}
				}
				state := group.GroupRuntimeSnapshots()[901].Members[0]
				if state.Status != "EVICTED" || state.RecoverySuccesses != 1 {
					t.Fatalf("unexpected recovery state: %+v", state)
				}
				// Delivery after the queue drains demonstrates the missing transition.
				s.enqueue(HealthResultEvent{Success: success, CheckedAt: time.Now()})
				synctest.Wait()
				state = group.GroupRuntimeSnapshots()[901].Members[0]
				if success && state.Status != "ALIVE" || !success && state.RecoverySuccesses != 0 {
					t.Fatalf("delivered transition had no expected effect: %+v", state)
				}
			})
		})
	}
}

func TestHealthEventCloseDiscardsPendingButAllowsRunningCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		calls := 0
		s := newHealthEventSubscription(func(HealthResultEvent) {
			close(entered)
			<-release
			calls++
		}, nil, nil, true)
		s.enqueue(HealthResultEvent{})
		<-entered
		s.enqueue(HealthResultEvent{})
		s.close()
		s.enqueue(HealthResultEvent{})
		close(release)
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("callbacks after close = %d", calls)
		}
	})
}
