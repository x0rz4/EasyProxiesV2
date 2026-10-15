package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/nodeloc"
)

func (s *sqliteStore) GetNodeLocation(ctx context.Context, nodeID int64) (nodeloc.State, error) {
	var state nodeloc.State
	var raw, identity, region, country string
	var revision int64
	err := s.readConn().QueryRowContext(ctx, `SELECT l.state_json,l.revision,n.identity_hash,n.region,n.country
FROM node_locations l JOIN nodes n ON n.id=l.node_id WHERE n.id=?`, nodeID).Scan(&raw, &revision, &identity, &region, &country)
	if err != nil {
		return state, fmt.Errorf("read node %d location: %w", nodeID, err)
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return state, fmt.Errorf("decode node %d location: %w", nodeID, err)
	}
	state.Token = nodeloc.Token{Identity: identity, Revision: revision}
	if state.Current.Source == "" {
		// Preserve legacy text until verified; v1 group rules compare that text.
		iso := strings.ToLower(strings.TrimSpace(region))
		state.Current = nodeloc.Location{ISO: iso, Country: strings.TrimSpace(country), Known: nodeloc.ValidISO(nodeloc.Normalize(iso)), Source: nodeloc.SourceLegacy}
	}
	return state, nil
}

func (s *sqliteStore) ApplyNodeLocation(ctx context.Context, nodeID int64, observation nodeloc.Observation) (nodeloc.Change, error) {
	var change nodeloc.Change
	err := s.runInTx(ctx, func(tx *sqliteStore) error {
		state, err := tx.GetNodeLocation(ctx, nodeID)
		if err != nil {
			return err
		}
		next, result := nodeloc.Decide(state, observation)
		change = result
		if !result.Applied {
			return nil
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode node location: %w", err)
		}
		if _, err := tx.writeConn().ExecContext(ctx, `UPDATE node_locations SET state_json=? WHERE node_id=?`, string(raw), nodeID); err != nil {
			return fmt.Errorf("save node location evidence: %w", err)
		}
		if result.Changed {
			_, err = tx.writeConn().ExecContext(ctx, `UPDATE nodes SET region=?,country=?,updated_at=? WHERE id=?`,
				result.Location.ISO, result.Location.Country, formatTime(time.Now().UTC()), nodeID)
		}
		return err
	})
	if err != nil {
		return nodeloc.Change{}, fmt.Errorf("apply node %d location: %w", nodeID, err)
	}
	return change, nil
}
