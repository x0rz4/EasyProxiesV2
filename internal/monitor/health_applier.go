package monitor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"easy_proxies/internal/probe"
	"easy_proxies/internal/store"
)

// SetProbeStore is called before registering runtime nodes. A nil store keeps
// the standalone in-memory manager useful to embedders and isolated tests.
func (m *Manager) SetProbeStore(db store.Store) {
	if db == nil {
		m.resultApplier.Store(nil)
		return
	}
	m.resultApplier.Store(probe.New(db))
}

type healthTicket struct {
	applier *probe.Applier
	event   probe.Result
}

func (m *Manager) beginHealthResult(ctx context.Context, e *entry) (*healthTicket, error) {
	applier := m.resultApplier.Load()
	if applier == nil {
		return nil, nil
	}
	e.mu.RLock()
	info := e.info
	e.mu.RUnlock()
	if info.NodeID <= 0 {
		return nil, fmt.Errorf("health node has no persisted identity")
	}
	event, err := applier.Begin(ctx, info.NodeID, info.URI, probe.HealthProbe, probe.Health)
	if err != nil {
		return nil, err
	}
	event.Health = &probe.HealthResult{Tag: info.Tag}
	return &healthTicket{applier: applier, event: event}, nil
}

func (m *Manager) commitHealthResult(e *entry, ticket *healthTicket, latency time.Duration, probeErr error, checkedAt time.Time) error {
	if ticket == nil {
		m.projectHealthResult(e, latency, probeErr, checkedAt)
		return nil
	}
	event := ticket.event
	event.Observation.CheckedAt = checkedAt
	event.Health = &probe.HealthResult{Tag: ticket.event.Health.Tag, Success: probeErr == nil, Latency: latency}
	if probeErr != nil {
		event.Health.Error = probeErr.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := ticket.applier.Apply(ctx, event)
	if err != nil {
		return err
	}
	if !out.Applied {
		return fmt.Errorf("health result rejected: %s", out.Reason)
	}
	if err := m.ProjectHealthOutcome(out); err != nil {
		return err
	}
	return ticket.applier.Acknowledge(ctx, out)
}

// Receipt retries are idempotent within a process. After restart, fresh startup
// probes establish health: historical receipts cannot advance recovery again.
func (m *Manager) ProjectHealthOutcome(out probe.Outcome) error {
	event := out.Event
	if event.Kind != probe.Health || event.Origin != probe.HealthProbe || event.Health == nil {
		return fmt.Errorf("invalid health projection")
	}
	if event.Observation.StartedAt.Before(m.probeEpoch) {
		return nil
	}
	e := m.registry.byTagEntry(event.Health.Tag)
	if e == nil || m.registry.byNodeID(event.NodeID) != e {
		return nil
	} // retired runtime generation
	e.healthApplyMu.Lock()
	defer e.healthApplyMu.Unlock()
	e.mu.RLock()
	nodeID, tag := e.info.NodeID, e.info.Tag
	e.mu.RUnlock()
	if nodeID != event.NodeID || tag != event.Health.Tag || out.ReceiptID == e.healthReceiptID || !event.Observation.CheckedAt.After(e.healthReceiptAt) {
		return nil
	}
	var err error
	if !event.Health.Success {
		err = errors.New(event.Health.Error)
	}
	m.projectHealthResult(e, event.Health.Latency, err, event.Observation.CheckedAt)
	e.healthReceiptID, e.healthReceiptAt = out.ReceiptID, event.Observation.CheckedAt
	return nil
}
