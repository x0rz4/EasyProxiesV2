package monitor

import (
	"testing"
	"time"
)

func TestSubscribeDebugLogs(t *testing.T) {
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Stop()

	events, unsubscribe := mgr.SubscribeDebugLogs()
	handle := mgr.Register(NodeInfo{Tag: "node-1", Name: "Node 1"})
	handle.RecordSuccess("example.com:443")

	select {
	case event := <-events:
		if event.NodeTag != "node-1" || event.NodeName != "Node 1" {
			t.Fatalf("unexpected node: %+v", event)
		}
		if !event.Event.Success || event.Event.Destination != "example.com:443" {
			t.Fatalf("unexpected timeline event: %+v", event.Event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for debug log event")
	}

	unsubscribe()
	handle.RecordSuccess("ignored.example:443")
	select {
	case event := <-events:
		t.Fatalf("received event after unsubscribe: %+v", event)
	case <-time.After(20 * time.Millisecond):
	}
}
