package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// HealthEvent is one accepted health measurement in commit order. It is the
// authoritative source for group failure/recovery transitions: the in-memory
// notification only says "new rows exist", so a slow consumer or a restart can
// still replay every transition instead of losing it with a queue overflow.
type HealthEvent struct {
	Seq              int64     `json:"seq"`
	NodeID           int64     `json:"node_id"`
	IdentityToken    string    `json:"identity_token"`
	IdentityRevision int64     `json:"identity_revision"`
	Tag              string    `json:"tag"`
	ProbeID          string    `json:"probe_id"`
	Success          bool      `json:"success"`
	LatencyMs        int64     `json:"latency_ms"`
	Error            string    `json:"error,omitempty"`
	CheckedAt        time.Time `json:"checked_at"`
	RecordedAt       time.Time `json:"recorded_at"`
}

// HealthEventJournalStats is the raw journal view before the monitor adds its
// per-consumer delivery counters.
type HealthEventJournalStats struct {
	Count            int64
	MinSeq           int64
	MaxSeq           int64
	OldestRecordedAt time.Time
	ConsumerCursors  map[string]int64
}

// HealthEventJournalLimit bounds the journal. Reaching it is a hard backpressure
// signal: new measurements are rejected with ErrHealthEventJournalFull instead
// of silently pruning facts that a wedged consumer still owes the state machine.
const HealthEventJournalLimit = 8192

// ErrHealthEventJournalFull is returned when the journal is at capacity. Callers
// must surface it; the measurement is not persisted.
var ErrHealthEventJournalFull = errors.New("health event journal full")

// healthEventTimeLayout is fixed width so lexicographic comparison in SQL is
// chronological. RFC3339Nano drops trailing zeros and would sort
// "…:00Z" after "…:00.1Z", which makes age-based pruning wrong.
const healthEventTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatHealthEventTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(healthEventTimeLayout)
}

func parseHealthEventTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(healthEventTimeLayout, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func scanHealthEvent(row interface{ Scan(...any) error }) (*HealthEvent, error) {
	var event HealthEvent
	var checkedAt, recordedAt string
	if err := row.Scan(&event.Seq, &event.NodeID, &event.IdentityToken, &event.IdentityRevision, &event.Tag,
		&event.ProbeID, &event.Success, &event.LatencyMs, &event.Error, &checkedAt, &recordedAt); err != nil {
		return nil, err
	}
	event.CheckedAt = parseHealthEventTime(checkedAt)
	event.RecordedAt = parseHealthEventTime(recordedAt)
	return &event, nil
}

// AppendHealthEvent assigns the next sequence number inside the caller's
// transaction, so a health fact and its journal row are never split.
func (s *sqliteStore) AppendHealthEvent(ctx context.Context, event HealthEvent) (int64, error) {
	conn := s.writeConn()
	var count int64
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM health_events`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count health events: %w", err)
	}
	if count >= HealthEventJournalLimit {
		return 0, ErrHealthEventJournalFull
	}
	recordedAt := event.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now().UTC()
	}
	result, err := conn.ExecContext(ctx, `INSERT INTO health_events(node_id,identity_token,identity_revision,tag,probe_id,success,latency_ms,error,checked_at,recorded_at)
 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		event.NodeID, event.IdentityToken, event.IdentityRevision, event.Tag, event.ProbeID,
		event.Success, event.LatencyMs, event.Error,
		formatHealthEventTime(event.CheckedAt), formatHealthEventTime(recordedAt))
	if err != nil {
		return 0, fmt.Errorf("append health event: %w", err)
	}
	return result.LastInsertId()
}

// ListHealthEventsAfter returns events strictly after seq in commit order.
func (s *sqliteStore) ListHealthEventsAfter(ctx context.Context, after int64, limit int) ([]HealthEvent, error) {
	if limit <= 0 || limit > 512 {
		limit = 512
	}
	rows, err := s.readConn().QueryContext(ctx, `SELECT seq,node_id,identity_token,identity_revision,tag,probe_id,success,latency_ms,error,checked_at,recorded_at
 FROM health_events WHERE seq>? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]HealthEvent, 0, limit)
	for rows.Next() {
		event, err := scanHealthEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, *event)
	}
	return events, rows.Err()
}

// MaxHealthEventSeq is the tail of the journal. A consumer registered for the
// first time starts here: replaying the whole journal at first start would
// turn a restart into a burst of stale evictions and recoveries.
func (s *sqliteStore) MaxHealthEventSeq(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	if err := s.readConn().QueryRowContext(ctx, `SELECT MAX(seq) FROM health_events`).Scan(&seq); err != nil {
		return 0, err
	}
	return seq.Int64, nil
}

func (s *sqliteStore) HealthEventJournalStats(ctx context.Context) (HealthEventJournalStats, error) {
	stats := HealthEventJournalStats{ConsumerCursors: make(map[string]int64)}
	var oldest sql.NullString
	if err := s.readConn().QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(seq),0), COALESCE(MAX(seq),0), COALESCE(MIN(recorded_at),'') FROM health_events`).
		Scan(&stats.Count, &stats.MinSeq, &stats.MaxSeq, &oldest); err != nil {
		return stats, err
	}
	stats.OldestRecordedAt = parseHealthEventTime(oldest.String)
	rows, err := s.readConn().QueryContext(ctx, `SELECT consumer,seq FROM health_event_cursors`)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	for rows.Next() {
		var consumer string
		var seq int64
		if err := rows.Scan(&consumer, &seq); err != nil {
			return stats, err
		}
		stats.ConsumerCursors[consumer] = seq
	}
	return stats, rows.Err()
}

// GetHealthEventCursor reports the persisted progress of a durable consumer.
// found=false means the consumer has never committed a cursor.
func (s *sqliteStore) GetHealthEventCursor(ctx context.Context, consumer string) (int64, bool, error) {
	var seq int64
	err := s.readConn().QueryRowContext(ctx, `SELECT seq FROM health_event_cursors WHERE consumer=?`, consumer).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return seq, true, nil
}

// PutHealthEventCursor persists delivery progress. It only moves forward so a
// retried write of an older batch cannot rewind a resumed consumer.
func (s *sqliteStore) PutHealthEventCursor(ctx context.Context, consumer string, seq int64) error {
	_, err := s.writeConn().ExecContext(ctx, `INSERT INTO health_event_cursors(consumer,seq,updated_at) VALUES(?,?,?)
 ON CONFLICT(consumer) DO UPDATE SET seq=CASE WHEN excluded.seq>seq THEN excluded.seq ELSE seq END, updated_at=excluded.updated_at`,
		consumer, seq, formatHealthEventTime(time.Now()))
	return err
}

// PruneHealthEvents deletes events older than olderThan that every *currently
// registered* consumer has already consumed. Rows beyond the slowest live
// cursor are never deleted: that would silently drop a transition the state
// machine still owes.
//
// Only activeConsumers constrain the cutoff. A cursor row left behind by a
// deleted group, a disabled group or a group whose box is stopped must not hold
// the journal open: it would pin the slowest position forever, so a single
// inactive consumer would eventually fill the journal and get every other
// group's measurements rejected. Such a consumer resumes from its cursor when
// it comes back and counts the rows pruned in between as a gap, which is the
// same contract ephemeral consumers already had.
//
// With no active consumer nothing owes the journal, so it is pruned by age only.
func (s *sqliteStore) PruneHealthEvents(ctx context.Context, olderThan time.Time, activeConsumers []string) (int64, error) {
	query := `DELETE FROM health_events WHERE recorded_at < ?`
	args := []any{formatHealthEventTime(olderThan)}
	if len(activeConsumers) > 0 {
		// An active consumer with no cursor row yet is positioned at the journal
		// tail, so falling back to MAX(seq) only releases rows everyone has
		// already passed.
		query += ` AND seq <= (SELECT COALESCE(MIN(seq),
     (SELECT COALESCE(MAX(seq),0) FROM health_events)) FROM health_event_cursors WHERE consumer IN (` +
			placeholders(len(activeConsumers)) + `))`
		for _, consumer := range activeConsumers {
			args = append(args, consumer)
		}
	}
	result, err := s.writeConn().ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("prune health events: %w", err)
	}
	removed, err := result.RowsAffected()
	return removed, err
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, 0, n*2-1)
	for i := 0; i < n; i++ {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '?')
	}
	return string(buf)
}
