package store

import (
	"context"
	"fmt"
	"time"

	"easy_proxies/internal/nodecodec"
	"easy_proxies/internal/nodeloc"
)

// BeginLandingProbe captures the revision before any network I/O. The URI
// belongs to the actual outbound, not a possibly newer persisted node.
func BeginLandingProbe(ctx context.Context, db Store, nodeID int64, uri, probeID string) (nodeloc.Observation, error) {
	state, err := db.GetNodeLocation(ctx, nodeID)
	if err != nil {
		return nodeloc.Observation{}, err
	}
	identity := nodecodec.FallbackHash(uri)
	if parsed, err := nodecodec.ParseURI(uri); err == nil {
		identity = parsed.Hash
	}
	if state.Identity != identity {
		return nodeloc.Observation{}, fmt.Errorf("node %d outbound identity changed", nodeID)
	}
	now := time.Now().UTC()
	if probeID == "" {
		// Wall-clock UnixNano alone can repeat within one timer tick; the
		// monotonic suffix keeps same-tick probes distinct.
		probeID = NewProbeID(fmt.Sprintf("landing:%d", nodeID))
	}
	return nodeloc.Observation{Token: state.Token, ProbeID: probeID, StartedAt: now, Source: nodeloc.SourceGeoIPExit}, nil
}

// ApplyLandingResult stores the measurement and location in the same
// transaction. Replayed or obsolete evidence cannot poison the cached result
// and later be promoted by startup reconciliation.
func ApplyLandingResult(ctx context.Context, db Store, observation nodeloc.Observation, result *NodeDetectionResult) (nodeloc.Change, error) {
	observation.Succeeded = result.ExitIPStatus == "success"
	observation.IP, observation.ISO, observation.Country = result.ExitIP, result.ExitCountryCode, result.ExitCountry
	observation.CheckedAt = result.ExitIPCheckedAt
	var change nodeloc.Change
	err := db.WithTx(ctx, func(tx Store) error {
		var err error
		change, err = tx.ApplyNodeLocation(ctx, result.NodeID, observation)
		if err != nil || !change.Applied {
			return err
		}
		return tx.UpsertNodeDetectionResult(ctx, result)
	})
	if err != nil {
		return nodeloc.Change{}, err
	}
	return change, nil
}
