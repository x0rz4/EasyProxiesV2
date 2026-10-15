package monitor

import (
	"context"
	"fmt"
	"time"

	"easy_proxies/internal/probe"
	"easy_proxies/internal/store"
)

// applyDiagnosticResult is the synchronous commit boundary for manual stages.
// No call here can publish health events. Failed persistence is visible to the
// task, and projection receipts remain pending until their work is acknowledged.
func (m *nodeCheckManager) applyDiagnosticResult(task *nodeCheckTask, node Snapshot, kind probe.Kind, detection *store.NodeDetectionResult, quality *store.NodeIPQualityResult) bool {
	observation, ok := task.observations[node.NodeID]
	if !ok {
		task.publish(nodeCheckEvent{Type: "task", NodeID: node.NodeID, Status: "warning", Error: "缺少检测前身份快照，结果未保存"})
		return false
	}
	observation.CheckedAt = time.Now().UTC()
	observation.ProbeID += ":" + string(kind)
	event := probe.Result{NodeID: node.NodeID, Kind: kind, Origin: probe.ManualDiagnostic, Observation: observation, Detection: detection, Quality: quality}
	applier := probe.New(m.server.store)
	out, err := applier.Apply(context.Background(), event)
	if err == nil && !out.Applied {
		err = fmt.Errorf("结果未应用: %s", out.Reason)
	}
	if err != nil {
		task.publish(nodeCheckEvent{Type: "task", Phase: string(kind), NodeID: node.NodeID, Status: "warning", Error: "保存检测结果失败: " + err.Error()})
		return false
	}
	m.server.enqueueRetag(node.NodeID)
	if err := applier.Acknowledge(context.Background(), out); err != nil {
		task.publish(nodeCheckEvent{Type: "task", Phase: string(kind), NodeID: node.NodeID, Status: "warning", Error: "确认结果同步失败: " + err.Error()})
	}
	return true
}

func (s *Server) reconcileProbeProjection(ctx context.Context, out probe.Outcome) error {
	if out.Event.Kind == probe.Health {
		return fmt.Errorf("health projection consumer not bound")
	}
	if out.Event.Kind == probe.Landing {
		state, err := s.store.GetNodeLocation(ctx, out.Event.NodeID)
		if err != nil {
			return err
		}
		if state.Token != out.Event.Observation.Token {
			return nil
		}
		if s.mgr == nil {
			return fmt.Errorf("monitor not initialized")
		}
		s.mgr.UpdateNodeLocation(out.Event.NodeID, state.Current.ISO, state.Current.Country)
		membership, ok := s.nodeMgr.(GroupMembershipManager)
		if !ok {
			return fmt.Errorf("membership manager not initialized")
		}
		if err := membership.ApplyGroupMembershipChanges(ctx, []int64{out.Event.NodeID}); err != nil {
			return err
		}
	}
	s.enqueueRetag(out.Event.NodeID)
	return nil
}

func (s *Server) runProbeProjections(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
		}
		if s.store == nil {
			continue
		}
		attempt, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := probe.New(s.store).Reconcile(attempt, s.reconcileProbeProjection)
		cancel()
		if err != nil && s.logger != nil {
			s.logger.Printf("[probe] pending projection retry: %v", err)
		}
	}
}
