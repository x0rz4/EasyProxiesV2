package monitor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"easy_proxies/internal/probe"
	"easy_proxies/internal/store"
)

func (s *Server) beginMeasuredResult(ctx context.Context, snap *Snapshot, kind probe.Kind, origin probe.Origin) (probe.Result, error) {
	if s == nil || s.store == nil || snap == nil {
		return probe.Result{}, fmt.Errorf("检测存储或节点不可用")
	}
	return probe.New(s.store).Begin(ctx, snap.NodeID, snap.URI, origin, kind)
}

func (s *Server) applyMeasuredResult(event probe.Result) error {
	if event.Kind == probe.Health {
		return fmt.Errorf("手动检测不能提交健康状态")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	event.Observation.CheckedAt = time.Now().UTC()
	applier := probe.New(s.store)
	out, err := applier.Apply(ctx, event)
	if err != nil {
		return err
	}
	if !out.Applied {
		return fmt.Errorf("检测结果未应用: %s", out.Reason)
	}
	s.enqueueRetag(event.NodeID)
	return applier.Acknowledge(ctx, out)
}

func measurementError(resultErr, persistErr error) error { return errors.Join(resultErr, persistErr) }

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
