package boxmgr

import (
	"context"
	"fmt"

	"easy_proxies/internal/probe"
)

// ProjectProbeOutcome is invoked by app's retry worker, including when the
// management HTTP server is disabled. Lifecycle locks are never held across
// the membership reconciliation call.
func (m *Manager) ProjectProbeOutcome(ctx context.Context, out probe.Outcome) error {
	m.mu.RLock()
	runtime, db := m.monitorMgr, m.store
	m.mu.RUnlock()
	if runtime == nil || db == nil {
		return fmt.Errorf("probe runtime not initialized")
	}
	state, err := db.GetNodeLocation(ctx, out.Event.NodeID)
	if err != nil {
		return err
	}
	if state.Token != out.Event.Observation.Token {
		return nil
	}
	switch out.Event.Kind {
	case probe.Health:
		return runtime.ProjectHealthOutcome(out)
	case probe.Landing:
		runtime.UpdateNodeLocation(out.Event.NodeID, state.Current.ISO, state.Current.Country)
		return m.ApplyGroupMembershipChanges(ctx, []int64{out.Event.NodeID})
	default:
		return nil
	}
}
