package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func scanSavedFilter(row scanner) (SavedFilter, error) {
	var filter SavedFilter
	var updated string
	err := row.Scan(&filter.ID, &filter.Name, &filter.SpecJSON, &filter.Revision, &updated)
	filter.UpdatedAt = parseTime(updated)
	return filter, err
}

func (s *sqliteStore) ListSavedFilters(ctx context.Context) ([]SavedFilter, error) {
	rows, err := s.readConn().QueryContext(ctx, `SELECT id,name,spec_json,revision,updated_at FROM saved_filters ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SavedFilter{}
	for rows.Next() {
		filter, err := scanSavedFilter(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, filter)
	}
	return result, rows.Err()
}

func (s *sqliteStore) GetSavedFilter(ctx context.Context, id int64) (*SavedFilter, error) {
	filter, err := scanSavedFilter(s.readConn().QueryRowContext(ctx, `SELECT id,name,spec_json,revision,updated_at FROM saved_filters WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &filter, nil
}

func (s *sqliteStore) SaveFilter(ctx context.Context, filter *SavedFilter) error {
	now := time.Now().UTC()
	if filter.ID == 0 {
		result, err := s.writeConn().ExecContext(ctx, `INSERT INTO saved_filters(name,spec_json,updated_at) VALUES(?,?,?)`, filter.Name, filter.SpecJSON, formatTime(now))
		if err != nil {
			return fmt.Errorf("create saved filter: %w", err)
		}
		filter.ID, err = result.LastInsertId()
		filter.Revision = 1
		filter.UpdatedAt = now
		return err
	}
	result, err := s.writeConn().ExecContext(ctx, `UPDATE saved_filters SET name=?,spec_json=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, filter.Name, filter.SpecJSON, formatTime(now), filter.ID, filter.Revision)
	if err != nil {
		return fmt.Errorf("update saved filter: %w", err)
	}
	if err := requireAffected(result, "saved filter changed or no longer exists; reload before saving"); err != nil {
		return err
	}
	filter.Revision++
	filter.UpdatedAt = now
	return nil
}

func (s *sqliteStore) DeleteSavedFilter(ctx context.Context, id int64) error {
	result, err := s.writeConn().ExecContext(ctx, `DELETE FROM saved_filters WHERE id=?`, id)
	if err != nil {
		return err
	}
	return requireAffected(result, "saved filter not found")
}
