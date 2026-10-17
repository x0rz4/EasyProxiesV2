package monitor

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

func TestPersistentHealthRequiresMeasurementWithoutTarget(t *testing.T) {
	for _, policy := range []string{StartupAvailabilityOptimistic, StartupAvailabilityStrict} {
		t.Run(policy, func(t *testing.T) {
			mgr, db, node := NewPersistentHealthFixture(t)
			mgr.SetStartupAvailabilityPolicy(policy)
			if err := mgr.MarkAvailableWithoutProbe("health@v1"); err == nil {
				t.Fatal("unmeasured persistent node accepted as healthy")
			}
			mgr.RequestStartupProbeAllOnce()
			mgr.RequestProbeTagsOnce([]string{"health@v1"})
			snapshot := mgr.SnapshotForNodeID(node.ID)
			if snapshot.InitialCheckDone || snapshot.Available || snapshot.SuccessCount != 0 || !snapshot.HealthCheckedAt.IsZero() {
				t.Fatalf("no-target path manufactured health: %+v", snapshot)
			}
			if snapshot.RoutingEligible != (policy == StartupAvailabilityOptimistic) {
				t.Fatalf("startup routing policy changed: %+v", snapshot)
			}
			if receipt, err := db.GetProbeReceipt(context.Background(), node.ID, "health"); err != nil || receipt != nil {
				t.Fatalf("no measurement must mean no health receipt: %+v %v", receipt, err)
			}
		})
	}
}

func TestInMemoryNoTargetCompatibility(t *testing.T) {
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.Register(NodeInfo{Tag: "embedded"})
	if err := mgr.MarkAvailableWithoutProbe("embedded"); err != nil {
		t.Fatal(err)
	}
	if snapshot := mgr.SnapshotForTag("embedded"); !snapshot.InitialCheckDone || !snapshot.Available {
		t.Fatalf("in-memory compatibility lost: %+v", snapshot)
	}
}

func TestHealthResultCannotCrossIdentityRoundTrip(t *testing.T) {
	mgr, db, node := NewPersistentHealthFixture(t)
	originalURI := node.URI
	mgr.HandleForNodeID(node.ID).SetProbe(func(context.Context) (time.Duration, error) {
		node.URI = "http://127.0.0.2:12345"
		if err := db.UpdateNode(context.Background(), node); err != nil {
			return 0, err
		}
		node.URI = originalURI
		return time.Millisecond, db.UpdateNode(context.Background(), node)
	})
	if _, err := mgr.Probe(context.Background(), "health@v1"); err == nil {
		t.Fatal("result from the original identity revision survived A-B-A")
	}
	if snapshot := mgr.SnapshotForNodeID(node.ID); snapshot.InitialCheckDone || snapshot.SuccessCount != 0 {
		t.Fatalf("obsolete identity affected runtime: %+v", snapshot)
	}
	if receipt, err := db.GetProbeReceipt(context.Background(), node.ID, "health"); err != nil || receipt != nil {
		t.Fatalf("obsolete identity left health evidence: %+v %v", receipt, err)
	}
}

func TestStandaloneProbeCancellationDoesNotChangeHealth(t *testing.T) {
	for _, outcome := range []string{"canceled_before_start", "canceled_error", "late_success"} {
		t.Run(outcome, func(t *testing.T) {
			mgr, db, node := NewPersistentHealthFixture(t)
			handle := mgr.HandleForNodeID(node.ID)
			handle.SetProbe(func(context.Context) (time.Duration, error) { return time.Millisecond, nil })
			if _, err := mgr.Probe(context.Background(), "health@v1"); err != nil {
				t.Fatal(err)
			}
			before := mgr.SnapshotForNodeID(node.ID)
			receiptBefore, err := db.GetProbeReceipt(context.Background(), node.ID, "health")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			handle.SetProbe(func(context.Context) (time.Duration, error) {
				calls++
				cancel()
				if outcome == "late_success" {
					return time.Millisecond, nil
				}
				return 0, context.Canceled
			})
			if outcome == "canceled_before_start" {
				cancel()
			}
			if _, err := mgr.Probe(ctx, "health@v1"); !errors.Is(err, context.Canceled) {
				t.Fatalf("caller cancellation not returned: %v", err)
			}
			if outcome == "canceled_before_start" && calls != 0 {
				t.Fatal("already-canceled probe still dialed")
			}
			after := mgr.SnapshotForNodeID(node.ID)
			if after.SuccessCount != before.SuccessCount || after.FailureCount != before.FailureCount ||
				after.Available != before.Available || after.HealthCheckedAt != before.HealthCheckedAt {
				t.Fatalf("caller cancellation changed measured health: before=%+v after=%+v", before, after)
			}
			receiptAfter, err := db.GetProbeReceipt(context.Background(), node.ID, "health")
			if err != nil || !reflect.DeepEqual(receiptBefore, receiptAfter) {
				t.Fatalf("caller cancellation changed receipt: before=%+v after=%+v err=%v", receiptBefore, receiptAfter, err)
			}
			// Cancellation must release the tag lease so subsequent health work runs.
			handle.SetProbe(func(context.Context) (time.Duration, error) { return time.Millisecond, nil })
			if _, err := mgr.Probe(context.Background(), "health@v1"); err != nil {
				t.Fatalf("lease leaked after cancellation: %v", err)
			}
		})
	}
}

func TestStandaloneProbeTimeoutCannotCommitLateSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr, err := NewManager(Config{RoutineProbeTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Stop()
		mgr.Register(NodeInfo{Tag: "slow"}).SetProbe(func(ctx context.Context) (time.Duration, error) {
			<-ctx.Done()
			return time.Millisecond, nil
		})
		if _, err := mgr.Probe(context.Background(), "slow"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("late success escaped probe timeout: %v", err)
		}
		snapshot := mgr.SnapshotForTag("slow")
		if snapshot.Available || !snapshot.InitialCheckDone || snapshot.SuccessCount != 0 || snapshot.FailureCount != 1 {
			t.Fatalf("timeout did not become a measured failure: %+v", snapshot)
		}
	})
}
