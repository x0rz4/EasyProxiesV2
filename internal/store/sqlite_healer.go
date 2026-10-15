package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"easy_proxies/internal/group"
	json "easy_proxies/internal/jsonx"
)

type GroupHealEvent struct {
	ID        int64            `json:"id"`
	GroupID   int64            `json:"group_id"`
	Status    string           `json:"status"`
	Action    group.HealAction `json:"action"`
	CreatedAt time.Time        `json:"created_at"`
}

// AppendGroupHealEvent suppresses repeated identical observations across process
// restarts. Each group's audit is bounded to 500 entries and 30 days.
func (s *sqliteStore) AppendGroupHealEvent(ctx context.Context, event GroupHealEvent, repeatAfter time.Duration) (bool, error) {
	if event.GroupID <= 0 || event.CreatedAt.IsZero() || repeatAfter < 0 {
		return false, fmt.Errorf("invalid healing event")
	}
	switch event.Status {
	case "dry_run", "planned", "succeeded", "failed", "deferred", "alert":
	default:
		return false, fmt.Errorf("invalid healing event status %q", event.Status)
	}
	detail, err := json.MarshalCanonical(event.Action)
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(append([]byte(event.Status+":"), detail...))
	key := hex.EncodeToString(digest[:])
	inserted := false
	err = s.runInTx(ctx, func(tx *sqliteStore) error {
		var previous string
		err := tx.writeConn().QueryRowContext(ctx, `SELECT created_at FROM group_heal_events WHERE group_id=? AND event_key=? ORDER BY id DESC LIMIT 1`, event.GroupID, key).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && event.CreatedAt.Sub(parseTime(previous)) < repeatAfter {
			return nil
		}
		if _, err := tx.writeConn().ExecContext(ctx, `INSERT INTO group_heal_events(group_id,event_key,action,status,detail_json,created_at) VALUES(?,?,?,?,?,?)`,
			event.GroupID, key, event.Action.Kind, event.Status, string(detail), formatTime(event.CreatedAt)); err != nil {
			return err
		}
		_, err = tx.writeConn().ExecContext(ctx, `DELETE FROM group_heal_events WHERE group_id=? AND
 (created_at<? OR id NOT IN (SELECT id FROM group_heal_events WHERE group_id=? ORDER BY id DESC LIMIT 500))`,
			event.GroupID, formatTime(event.CreatedAt.Add(-30*24*time.Hour)), event.GroupID)
		if err == nil {
			inserted = true
		}
		return err
	})
	return inserted && err == nil, err
}

func (s *sqliteStore) ListGroupHealEvents(ctx context.Context, groupID int64, limit int) ([]GroupHealEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.readConn().QueryContext(ctx, `SELECT id,group_id,status,detail_json,created_at FROM group_heal_events WHERE group_id=? ORDER BY id DESC LIMIT ?`, groupID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []GroupHealEvent{}
	for rows.Next() {
		var event GroupHealEvent
		var detail, created string
		if err := rows.Scan(&event.ID, &event.GroupID, &event.Status, &detail, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(detail), &event.Action); err != nil {
			return nil, err
		}
		event.CreatedAt = parseTime(created)
		result = append(result, event)
	}
	return result, rows.Err()
}
