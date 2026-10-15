// Package probe applies measurement events. Network execution remains in the
// existing schedulers and nodedetect; only health-origin events carry health.
package probe

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/nodeloc"
	"easy_proxies/internal/store"
)

type Kind string

const (
	Latency Kind = "latency"
	Speed   Kind = "speed"
	Landing Kind = "landing"
	Quality Kind = "quality"
	Unlock  Kind = "unlock"
	Health  Kind = "health"
)

type Origin string

const (
	HealthProbe      Origin = "health_probe"
	ManualDiagnostic Origin = "manual_diagnostic"
	StartupExit      Origin = "startup_exit"
	UnlockCheck      Origin = "unlock_check"
)

type HealthResult struct {
	Tag     string        `json:"tag"`
	Success bool          `json:"success"`
	Latency time.Duration `json:"latency"`
	Error   string        `json:"error,omitempty"`
}
type Result struct {
	NodeID      int64                      `json:"node_id"`
	Kind        Kind                       `json:"kind"`
	Origin      Origin                     `json:"origin"`
	Observation nodeloc.Observation        `json:"observation"`
	Detection   *store.NodeDetectionResult `json:"detection,omitempty"`
	Quality     *store.NodeIPQualityResult `json:"quality,omitempty"`
	Unlock      *store.UnlockResult        `json:"unlock,omitempty"`
	Health      *HealthResult              `json:"health,omitempty"`
}
type Outcome struct {
	ReceiptID string         `json:"receipt_id"`
	Event     Result         `json:"event"`
	Applied   bool           `json:"applied"`
	Reason    string         `json:"reason"`
	Location  nodeloc.Change `json:"location"`
}
type Applier struct{ db store.Store }

func New(db store.Store) *Applier { return &Applier{db: db} }

// Begin captures identity before dialing, so replacing A with B and back with A
// still invalidates every result from the original A revision.
func (a *Applier) Begin(ctx context.Context, nodeID int64, uri string, origin Origin, kind Kind) (Result, error) {
	if a == nil || a.db == nil {
		return Result{}, fmt.Errorf("probe store unavailable")
	}
	observation, err := store.BeginLandingProbe(ctx, a.db, nodeID, uri, "")
	return Result{NodeID: nodeID, Kind: kind, Origin: origin, Observation: observation}, err
}

// ApplyLanding adapts existing exit-IP measurements into the same event path.
// Only the landing portion is copied; cached latency/speed fields are not new
// evidence and must not be rewritten by startup reclassification.
func ApplyLanding(ctx context.Context, db store.Store, observation nodeloc.Observation, result *store.NodeDetectionResult, origin Origin) (nodeloc.Change, error) {
	if result == nil {
		return nodeloc.Change{}, fmt.Errorf("missing landing result")
	}
	observation.CheckedAt = result.ExitIPCheckedAt
	landing := &store.NodeDetectionResult{NodeID: result.NodeID, TaskID: result.TaskID, LatencyStatus: "untested", SpeedStatus: "untested",
		ExitIP: result.ExitIP, ExitIPFamily: result.ExitIPFamily, ExitCountry: result.ExitCountry, ExitCountryCode: result.ExitCountryCode,
		ExitIPStatus: result.ExitIPStatus, ExitIPError: result.ExitIPError, ExitIPCheckedAt: result.ExitIPCheckedAt}
	out, err := New(db).Apply(ctx, Result{NodeID: result.NodeID, Kind: Landing, Origin: origin, Observation: observation, Detection: landing})
	return out.Location, err
}

func (r Result) Stream() string {
	if r.Kind == Quality && r.Quality != nil {
		return string(Quality) + ":" + strings.ToLower(strings.TrimSpace(r.Quality.Provider))
	}
	return string(r.Kind)
}

func (r Result) validate() error {
	o := r.Observation
	if r.NodeID <= 0 || o.Identity == "" || o.Revision <= 0 || o.ProbeID == "" || o.StartedAt.IsZero() || o.CheckedAt.Before(o.StartedAt) {
		return fmt.Errorf("invalid probe identity or time")
	}
	if r.Origin != HealthProbe && r.Origin != ManualDiagnostic && r.Origin != StartupExit && r.Origin != UnlockCheck {
		return fmt.Errorf("invalid probe origin")
	}
	if r.Origin == HealthProbe && r.Kind != Health || r.Origin == StartupExit && r.Kind != Landing || r.Origin == UnlockCheck && r.Kind != Unlock {
		return fmt.Errorf("probe origin cannot publish this kind")
	}
	payloads := 0
	if r.Detection != nil {
		payloads++
	}
	if r.Quality != nil {
		payloads++
	}
	if r.Unlock != nil {
		payloads++
	}
	if r.Health != nil {
		payloads++
	}
	if payloads != 1 {
		return fmt.Errorf("probe event must contain exactly one payload")
	}
	switch r.Kind {
	case Health:
		if r.Origin != HealthProbe || r.Health == nil || r.Health.Tag == "" {
			return fmt.Errorf("health events require health probe origin")
		}
	case Latency, Speed, Landing:
		if r.Detection == nil || r.Detection.NodeID != r.NodeID {
			return fmt.Errorf("invalid detection payload")
		}
		if r.Kind != Landing && r.Origin != ManualDiagnostic {
			return fmt.Errorf("manual diagnostics require manual origin")
		}
		if r.Kind == Landing && (o.Source != nodeloc.SourceGeoIPExit && o.Source != nodeloc.SourceExitProbe) {
			return fmt.Errorf("landing requires exit evidence")
		}
		d := r.Detection
		untouched := func(status string) bool { return status == "" || status == "untested" }
		if r.Kind != Latency && !untouched(d.LatencyStatus) || r.Kind != Speed && !untouched(d.SpeedStatus) || r.Kind != Landing && !untouched(d.ExitIPStatus) {
			return fmt.Errorf("detection event writes another stream")
		}
	case Quality:
		if r.Origin != ManualDiagnostic || r.Quality == nil || r.Quality.NodeID != r.NodeID || strings.TrimSpace(r.Quality.Provider) == "" {
			return fmt.Errorf("invalid quality payload")
		}
	case Unlock:
		if (r.Origin != ManualDiagnostic && r.Origin != UnlockCheck) || r.Unlock == nil || r.Unlock.NodeID != r.NodeID {
			return fmt.Errorf("invalid unlock payload")
		}
	default:
		return fmt.Errorf("invalid probe kind %q", r.Kind)
	}
	return nil
}

// Apply commits facts and their receipt atomically. A projection consumer may
// fail after this returns; the pending receipt survives for reconciliation.
func (a *Applier) Apply(ctx context.Context, event Result) (Outcome, error) {
	out := Outcome{Event: event}
	if a == nil || a.db == nil {
		return out, fmt.Errorf("probe store unavailable")
	}
	if err := event.validate(); err != nil {
		return out, err
	}
	rawEvent, err := json.MarshalCanonical(event)
	if err != nil {
		return out, err
	}
	out.ReceiptID = fmt.Sprintf("%x", sha256.Sum256(rawEvent))
	err = a.db.WithTx(ctx, func(tx store.Store) error {
		current, err := tx.GetNodeLocation(ctx, event.NodeID)
		if err != nil {
			return err
		}
		if current.Token != event.Observation.Token {
			out.Reason = "obsolete_identity"
			return nil
		}
		previous, err := tx.GetProbeReceipt(ctx, event.NodeID, event.Stream())
		if err != nil {
			return err
		}
		if previous != nil && previous.Revision == current.Revision && event.Kind != Landing {
			var prior Outcome
			if err := json.Unmarshal([]byte(previous.ResultJSON), &prior); err != nil {
				return err
			}
			if prior.Event.Observation.ProbeID == event.Observation.ProbeID {
				out.Reason = "duplicate"
				return nil
			}
			if !event.Observation.StartedAt.After(previous.StartedAt) {
				out.Reason = "older_probe"
				return nil
			}
		}
		switch event.Kind {
		case Landing:
			out.Location, err = store.ApplyLandingResult(ctx, tx, event.Observation, event.Detection)
			if err != nil {
				return err
			}
			if !out.Location.Applied {
				out.Reason = out.Location.Reason
				return nil
			}
		case Latency, Speed:
			err = tx.UpsertNodeDetectionResult(ctx, event.Detection)
		case Quality:
			err = tx.UpsertNodeIPQualityResult(ctx, event.Quality)
		case Unlock:
			err = tx.UpsertUnlockResult(ctx, event.Unlock)
		case Health: // Health runtime effects are executed only by the health consumer.
		}
		if err != nil {
			return err
		}
		out.Applied = true
		out.Reason = "applied"
		raw, err := json.Marshal(out)
		if err != nil {
			return err
		}
		return tx.PutProbeReceipt(ctx, store.ProbeReceipt{NodeID: event.NodeID, Stream: event.Stream(), EventID: out.ReceiptID, Revision: current.Revision, StartedAt: event.Observation.StartedAt, ResultJSON: string(raw), Pending: true})
	})
	if err != nil {
		return Outcome{Event: event}, fmt.Errorf("apply %s result for node %d: %w", event.Kind, event.NodeID, err)
	}
	return out, nil
}

func (a *Applier) Acknowledge(ctx context.Context, out Outcome) error {
	return a.db.AcknowledgeProbeReceipt(ctx, out.Event.NodeID, out.Event.Stream(), out.ReceiptID)
}

// Reconcile invokes an idempotent projection consumer for committed results.
// Identity changes are rechecked; failed projections remain pending. Consumers
// must project latest persisted state rather than overwrite it with old fields.
func (a *Applier) Reconcile(ctx context.Context, consume func(context.Context, Outcome) error) error {
	receipts, err := a.db.ListPendingProbeReceipts(ctx, 1000)
	if err != nil {
		return err
	}
	var failures []error
	for _, receipt := range receipts {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		latest, err := a.db.GetProbeReceipt(ctx, receipt.NodeID, receipt.Stream)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if latest == nil || latest.EventID != receipt.EventID {
			continue
		}
		var out Outcome
		if err := json.Unmarshal([]byte(receipt.ResultJSON), &out); err != nil {
			failures = append(failures, err)
			continue
		}
		current, err := a.db.GetNodeLocation(ctx, receipt.NodeID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if current.Token == out.Event.Observation.Token {
			if err := consume(ctx, out); err != nil {
				failures = append(failures, err)
				continue
			}
		}
		if err := a.Acknowledge(ctx, out); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
