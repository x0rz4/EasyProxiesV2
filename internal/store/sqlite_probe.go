package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ProbeReceipt keeps only the newest result per node and measurement stream.
// Pending projection is acknowledged by event ID so an old consumer cannot
// acknowledge a replacement result written while it was running.
type ProbeReceipt struct {
	NodeID     int64     `json:"node_id"`
	Stream     string    `json:"stream"`
	EventID    string    `json:"event_id"`
	Revision   int64     `json:"revision"`
	StartedAt  time.Time `json:"started_at"`
	ResultJSON string    `json:"result_json"`
	Pending    bool      `json:"pending"`
}

func scanProbeReceipt(row interface{ Scan(...any) error }) (*ProbeReceipt, error) {
	var r ProbeReceipt
	var started string
	err := row.Scan(&r.NodeID, &r.Stream, &r.EventID, &r.Revision, &started, &r.ResultJSON, &r.Pending)
	if err != nil {
		return nil, err
	}
	r.StartedAt, err = time.Parse(time.RFC3339Nano, started)
	return &r, err
}

func (s *sqliteStore) GetProbeReceipt(ctx context.Context, nodeID int64, stream string) (*ProbeReceipt, error) {
	r, err := scanProbeReceipt(s.readConn().QueryRowContext(ctx, `SELECT node_id,stream,event_id,revision,started_at,result_json,pending FROM node_probe_receipts WHERE node_id=? AND stream=?`, nodeID, stream))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}
func (s *sqliteStore) PutProbeReceipt(ctx context.Context, r ProbeReceipt) error {
	_, err := s.writeConn().ExecContext(ctx, `INSERT INTO node_probe_receipts(node_id,stream,event_id,revision,started_at,result_json,pending) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(node_id,stream) DO UPDATE SET event_id=excluded.event_id,revision=excluded.revision,started_at=excluded.started_at,result_json=excluded.result_json,pending=excluded.pending`, r.NodeID, r.Stream, r.EventID, r.Revision, r.StartedAt.UTC().Format(time.RFC3339Nano), r.ResultJSON, r.Pending)
	return err
}
func (s *sqliteStore) ListPendingProbeReceipts(ctx context.Context, limit int) ([]ProbeReceipt, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.readConn().QueryContext(ctx, `SELECT node_id,stream,event_id,revision,started_at,result_json,pending FROM node_probe_receipts WHERE pending=1 ORDER BY started_at,node_id,stream LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ProbeReceipt{}
	for rows.Next() {
		r, err := scanProbeReceipt(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *r)
	}
	return result, rows.Err()
}
func (s *sqliteStore) AcknowledgeProbeReceipt(ctx context.Context, nodeID int64, stream, eventID string) error {
	_, err := s.writeConn().ExecContext(ctx, `UPDATE node_probe_receipts SET pending=0 WHERE node_id=? AND stream=? AND event_id=?`, nodeID, stream, eventID)
	return err
}
