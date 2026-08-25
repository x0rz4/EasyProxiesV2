package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// sqliteStore implements Store using SQLite.
type sqliteStore struct {
	db *sql.DB
	tx *sql.Tx // non-nil when operating inside WithTx
}

// Open creates a new SQLite-backed Store at the given path.
// It applies all pending migrations and sets optimal PRAGMAs.
func Open(dbPath string) (Store, error) {
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-64000)&_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dbPath, err)
	}

	// Connection pool settings
	db.SetMaxOpenConns(1) // SQLite only supports 1 writer
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(0) // connections don't expire

	// Verify connection
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	// Run migrations
	if err := Migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	log.Printf("[store] SQLite store opened: %s", dbPath)
	return &sqliteStore{db: db}, nil
}

// conn returns the underlying *sql.Tx or *sql.DB for executing queries.
func (s *sqliteStore) conn() querier {
	if s.tx != nil {
		return s.tx
	}
	return s.db
}

// querier abstracts *sql.DB and *sql.Tx for query execution.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
}

// ===================== Node operations =====================

func (s *sqliteStore) ListNodes(ctx context.Context, filter NodeFilter) ([]Node, error) {
	query := "SELECT id, uri, name, source, port, username, password, region, country, enabled, tags, created_at, updated_at FROM nodes"
	var conditions []string
	var args []any

	if filter.Source != "" {
		conditions = append(conditions, "source = ?")
		args = append(args, filter.Source)
	}
	if filter.Region != "" {
		conditions = append(conditions, "region = ?")
		args = append(args, filter.Region)
	}
	if filter.Enabled != nil {
		conditions = append(conditions, "enabled = ?")
		if *filter.Enabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY id ASC"
	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
		if filter.Offset > 0 {
			query += fmt.Sprintf(" OFFSET %d", filter.Offset)
		}
	}

	rows, err := s.conn().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()

	return scanNodes(rows)
}

func (s *sqliteStore) ListManagedNodes(ctx context.Context, subscriptionID *int64) ([]ManagedNode, error) {
	query := `SELECT n.id, n.uri, n.name, n.source, n.port, n.username, n.password,
		n.region, n.country, n.enabled, n.tags, n.created_at, n.updated_at,
		COALESCE(GROUP_CONCAT(DISTINCT CASE WHEN subscriptions.enabled=1 THEN subscriptions.id END), '')
		FROM nodes n
		LEFT JOIN subscription_nodes ON subscription_nodes.node_id=n.id
		LEFT JOIN subscriptions ON subscriptions.id=subscription_nodes.subscription_id`
	var args []any
	if subscriptionID != nil {
		query += ` WHERE EXISTS (SELECT 1 FROM subscription_nodes filter_membership
			JOIN subscriptions filter_subscription ON filter_subscription.id=filter_membership.subscription_id
			WHERE filter_membership.node_id=n.id AND filter_membership.subscription_id=? AND filter_subscription.enabled=1)`
		args = append(args, *subscriptionID)
	} else {
		query += ` WHERE n.source<>? OR EXISTS (SELECT 1 FROM subscription_nodes visible_membership
			JOIN subscriptions visible_subscription ON visible_subscription.id=visible_membership.subscription_id
			WHERE visible_membership.node_id=n.id AND visible_subscription.enabled=1)`
		args = append(args, NodeSourceSubscription)
	}
	query += ` GROUP BY n.id ORDER BY n.id`

	rows, err := s.conn().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list managed nodes: %w", err)
	}
	defer rows.Close()
	var result []ManagedNode
	for rows.Next() {
		var managed ManagedNode
		var enabled int
		var createdAt, updatedAt, subscriptionIDs, tagsJSON string
		if err := rows.Scan(&managed.ID, &managed.URI, &managed.Name, &managed.Source, &managed.Port,
			&managed.Username, &managed.Password, &managed.Region, &managed.Country, &enabled,
			&tagsJSON, &createdAt, &updatedAt, &subscriptionIDs); err != nil {
			return nil, fmt.Errorf("scan managed node: %w", err)
		}
		managed.Enabled = enabled != 0
		if tagsJSON != "" && tagsJSON != "[]" {
			_ = json.Unmarshal([]byte(tagsJSON), &managed.Tags)
		}
		managed.CreatedAt, managed.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
		if subscriptionIDs != "" {
			for _, value := range strings.Split(subscriptionIDs, ",") {
				id, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("parse managed node subscription ID %q: %w", value, err)
				}
				managed.SubscriptionIDs = append(managed.SubscriptionIDs, id)
			}
		}
		if managed.SubscriptionIDs == nil {
			managed.SubscriptionIDs = []int64{}
		}
		result = append(result, managed)
	}
	return result, rows.Err()
}

func (s *sqliteStore) GetNode(ctx context.Context, id int64) (*Node, error) {
	row := s.conn().QueryRowContext(ctx,
		"SELECT id, uri, name, source, port, username, password, region, country, enabled, tags, created_at, updated_at FROM nodes WHERE id = ?", id)
	return scanNode(row)
}

func (s *sqliteStore) GetNodeByURI(ctx context.Context, uri string) (*Node, error) {
	row := s.conn().QueryRowContext(ctx,
		"SELECT id, uri, name, source, port, username, password, region, country, enabled, tags, created_at, updated_at FROM nodes WHERE uri = ?", uri)
	return scanNode(row)
}

func (s *sqliteStore) GetNodeByName(ctx context.Context, name string) (*Node, error) {
	row := s.conn().QueryRowContext(ctx,
		"SELECT id, uri, name, source, port, username, password, region, country, enabled, tags, created_at, updated_at FROM nodes WHERE name = ?", name)
	return scanNode(row)
}

func (s *sqliteStore) CreateNode(ctx context.Context, node *Node) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if node.CreatedAt.IsZero() {
		node.CreatedAt = time.Now().UTC()
	}
	if node.UpdatedAt.IsZero() {
		node.UpdatedAt = time.Now().UTC()
	}
	enabled := 0
	if node.Enabled {
		enabled = 1
	}

	var tagsJSON = "[]"
	if len(node.Tags) > 0 {
		if b, err := json.Marshal(node.Tags); err == nil {
			tagsJSON = string(b)
		}
	}

	result, err := s.conn().ExecContext(ctx,
		`INSERT INTO nodes (uri, name, source, port, username, password, region, country, enabled, tags, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		node.URI, node.Name, node.Source, node.Port,
		node.Username, node.Password, node.Region, node.Country,
		enabled, tagsJSON, now, now,
	)
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get last insert id: %w", err)
	}
	node.ID = id

	// Create initial stats row
	_, err = s.conn().ExecContext(ctx,
		"INSERT OR IGNORE INTO node_stats (node_id) VALUES (?)", id)
	if err != nil {
		return fmt.Errorf("create initial node stats: %w", err)
	}

	return nil
}

func (s *sqliteStore) UpdateNode(ctx context.Context, node *Node) error {
	now := time.Now().UTC().Format(time.RFC3339)
	enabled := 0
	if node.Enabled {
		enabled = 1
	}

	var tagsJSON = "[]"
	if len(node.Tags) > 0 {
		if b, err := json.Marshal(node.Tags); err == nil {
			tagsJSON = string(b)
		}
	}

	result, err := s.conn().ExecContext(ctx,
		`UPDATE nodes SET uri=?, name=?, source=?, port=?, username=?, password=?,
		 region=?, country=?, enabled=?, tags=?, updated_at=?
		 WHERE id=?`,
		node.URI, node.Name, node.Source, node.Port,
		node.Username, node.Password, node.Region, node.Country,
		enabled, tagsJSON, now, node.ID,
	)
	if err != nil {
		return fmt.Errorf("update node %d: %w", node.ID, err)
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("node %d not found", node.ID)
	}
	return nil
}

func (s *sqliteStore) DeleteNode(ctx context.Context, id int64) error {
	result, err := s.conn().ExecContext(ctx, "DELETE FROM nodes WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete node %d: %w", id, err)
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("node %d not found", id)
	}
	return nil
}

func (s *sqliteStore) DeleteNodesBySource(ctx context.Context, source string) (int64, error) {
	result, err := s.conn().ExecContext(ctx, "DELETE FROM nodes WHERE source = ?", source)
	if err != nil {
		return 0, fmt.Errorf("delete nodes by source %q: %w", source, err)
	}
	return result.RowsAffected()
}

func (s *sqliteStore) BulkUpsertNodes(ctx context.Context, nodes []Node) error {
	if len(nodes) == 0 {
		return nil
	}

	execFn := func(txStore *sqliteStore) error {
		now := time.Now().UTC().Format(time.RFC3339)
		stmt, err := txStore.conn().PrepareContext(ctx,
			`INSERT INTO nodes (uri, name, source, port, username, password, region, country, enabled, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(uri) DO UPDATE SET
			   name=excluded.name, source=excluded.source, port=excluded.port,
			   username=excluded.username, password=excluded.password,
			   region=excluded.region, country=excluded.country,
			   updated_at=excluded.updated_at`)
		if err != nil {
			return fmt.Errorf("prepare bulk upsert: %w", err)
		}
		defer stmt.Close()

		for i := range nodes {
			n := &nodes[i]
			enabled := 0
			if n.Enabled {
				enabled = 1
			}
			result, err := stmt.ExecContext(ctx,
				n.URI, n.Name, n.Source, n.Port,
				n.Username, n.Password, n.Region, n.Country,
				enabled, now, now,
			)
			if err != nil {
				return fmt.Errorf("upsert node %q: %w", n.URI, err)
			}
			id, _ := result.LastInsertId()
			if id > 0 {
				n.ID = id
			}
		}

		// Create stats rows for new nodes
		_, err = txStore.conn().ExecContext(ctx,
			"INSERT OR IGNORE INTO node_stats (node_id) SELECT id FROM nodes")
		if err != nil {
			return fmt.Errorf("create stats for new nodes: %w", err)
		}

		return nil
	}

	// If already in a transaction, execute directly
	if s.tx != nil {
		return execFn(s)
	}

	// Otherwise wrap in a transaction
	return s.WithTx(ctx, func(tx Store) error {
		return execFn(tx.(*sqliteStore))
	})
}

func (s *sqliteStore) CountNodes(ctx context.Context, filter NodeFilter) (int64, error) {
	query := "SELECT COUNT(*) FROM nodes"
	var conditions []string
	var args []any

	if filter.Source != "" {
		conditions = append(conditions, "source = ?")
		args = append(args, filter.Source)
	}
	if filter.Region != "" {
		conditions = append(conditions, "region = ?")
		args = append(args, filter.Region)
	}
	if filter.Enabled != nil {
		conditions = append(conditions, "enabled = ?")
		if *filter.Enabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	var count int64
	err := s.conn().QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

// ===================== Subscriptions =====================

const subscriptionColumns = `id, name, url, enabled, refresh_interval_seconds,
	refresh_timeout_seconds, sort_order, last_attempt, last_success, last_error,
	node_count, etag, last_modified, created_at, updated_at`

func (s *sqliteStore) ListSubscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := s.conn().QueryContext(ctx, "SELECT "+subscriptionColumns+" FROM subscriptions ORDER BY sort_order, id")
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	defer rows.Close()
	var subscriptions []Subscription
	for rows.Next() {
		subscription, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("scan subscription: %w", err)
		}
		subscriptions = append(subscriptions, subscription)
	}
	return subscriptions, rows.Err()
}

func (s *sqliteStore) GetSubscription(ctx context.Context, id int64) (*Subscription, error) {
	return scanSubscriptionRow(s.conn().QueryRowContext(ctx, "SELECT "+subscriptionColumns+" FROM subscriptions WHERE id = ?", id))
}

func (s *sqliteStore) GetSubscriptionByURL(ctx context.Context, url string) (*Subscription, error) {
	return scanSubscriptionRow(s.conn().QueryRowContext(ctx, "SELECT "+subscriptionColumns+" FROM subscriptions WHERE url = ?", url))
}

func (s *sqliteStore) CreateSubscription(ctx context.Context, subscription *Subscription) error {
	now := time.Now().UTC()
	result, err := s.conn().ExecContext(ctx, `INSERT INTO subscriptions
		(name, url, enabled, refresh_interval_seconds, refresh_timeout_seconds, sort_order,
		 last_attempt, last_success, last_error, node_count, etag, last_modified, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		subscription.Name, subscription.URL, boolToInt(subscription.Enabled),
		subscription.RefreshIntervalSeconds, subscription.RefreshTimeoutSeconds, subscription.SortOrder,
		formatTime(subscription.LastAttempt), formatTime(subscription.LastSuccess), subscription.LastError,
		subscription.NodeCount, subscription.ETag, subscription.LastModified, formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("create subscription: %w", err)
	}
	subscription.ID, err = result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get subscription id: %w", err)
	}
	subscription.CreatedAt, subscription.UpdatedAt = now, now
	return nil
}

func (s *sqliteStore) UpdateSubscription(ctx context.Context, subscription *Subscription) error {
	result, err := s.conn().ExecContext(ctx, `UPDATE subscriptions SET name=?, url=?, enabled=?,
		refresh_interval_seconds=?, refresh_timeout_seconds=?, sort_order=?, last_attempt=?,
		last_success=?, last_error=?, node_count=?, etag=?, last_modified=?, updated_at=? WHERE id=?`,
		subscription.Name, subscription.URL, boolToInt(subscription.Enabled), subscription.RefreshIntervalSeconds,
		subscription.RefreshTimeoutSeconds, subscription.SortOrder, formatTime(subscription.LastAttempt),
		formatTime(subscription.LastSuccess), subscription.LastError, subscription.NodeCount, subscription.ETag,
		subscription.LastModified, formatTime(time.Now().UTC()), subscription.ID)
	if err != nil {
		return fmt.Errorf("update subscription %d: %w", subscription.ID, err)
	}
	return requireAffected(result, fmt.Sprintf("subscription %d not found", subscription.ID))
}

func (s *sqliteStore) DeleteSubscription(ctx context.Context, id int64) error {
	result, err := s.conn().ExecContext(ctx, "DELETE FROM subscriptions WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete subscription %d: %w", id, err)
	}
	return requireAffected(result, fmt.Sprintf("subscription %d not found", id))
}

func (s *sqliteStore) SetSubscriptionEnabled(ctx context.Context, id int64, enabled bool) error {
	result, err := s.conn().ExecContext(ctx, "UPDATE subscriptions SET enabled=?, updated_at=? WHERE id=?",
		boolToInt(enabled), formatTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("set subscription %d enabled: %w", id, err)
	}
	return requireAffected(result, fmt.Sprintf("subscription %d not found", id))
}

func (s *sqliteStore) UpdateAllSubscriptionRefreshSettings(ctx context.Context, intervalSeconds, timeoutSeconds int) error {
	if intervalSeconds < 0 || timeoutSeconds < 0 || intervalSeconds == 0 && timeoutSeconds == 0 {
		return fmt.Errorf("at least one positive subscription refresh setting is required")
	}
	query := "UPDATE subscriptions SET updated_at=?"
	args := []any{formatTime(time.Now().UTC())}
	if intervalSeconds > 0 {
		query += ", refresh_interval_seconds=?"
		args = append(args, intervalSeconds)
	}
	if timeoutSeconds > 0 {
		query += ", refresh_timeout_seconds=?"
		args = append(args, timeoutSeconds)
	}
	_, err := s.conn().ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update subscription refresh settings: %w", err)
	}
	return nil
}

func (s *sqliteStore) ActivateSubscriptionExclusive(ctx context.Context, id int64) error {
	return s.runInTx(ctx, func(tx *sqliteStore) error {
		var exists int
		if err := tx.conn().QueryRowContext(ctx, "SELECT 1 FROM subscriptions WHERE id=?", id).Scan(&exists); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("subscription %d not found", id)
			}
			return err
		}
		_, err := tx.conn().ExecContext(ctx, `UPDATE subscriptions SET enabled=CASE WHEN id=? THEN 1 ELSE 0 END,
			updated_at=? WHERE enabled != CASE WHEN id=? THEN 1 ELSE 0 END`, id, formatTime(time.Now().UTC()), id)
		return err
	})
}

func (s *sqliteStore) ListSubscriptionNodes(ctx context.Context, subscriptionID int64) ([]SubscriptionNode, error) {
	rows, err := s.conn().QueryContext(ctx, `SELECT sn.subscription_id, sn.position,
		n.id, n.uri, n.name, n.source, n.port, n.username, n.password, n.region, n.country,
		n.enabled, n.tags, n.created_at, n.updated_at
		FROM subscription_nodes sn JOIN nodes n ON n.id=sn.node_id
		WHERE sn.subscription_id=? ORDER BY sn.position, n.id`, subscriptionID)
	if err != nil {
		return nil, fmt.Errorf("list subscription %d nodes: %w", subscriptionID, err)
	}
	defer rows.Close()
	var result []SubscriptionNode
	for rows.Next() {
		var member SubscriptionNode
		var enabled int
		var tagsJSON, createdAt, updatedAt string
		if err := rows.Scan(&member.SubscriptionID, &member.Position, &member.Node.ID, &member.Node.URI,
			&member.Node.Name, &member.Node.Source, &member.Node.Port, &member.Node.Username,
			&member.Node.Password, &member.Node.Region, &member.Node.Country, &enabled, &tagsJSON, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		member.Node.Enabled = enabled != 0
		if tagsJSON != "" && tagsJSON != "[]" {
			_ = json.Unmarshal([]byte(tagsJSON), &member.Node.Tags)
		}
		member.Node.CreatedAt, member.Node.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
		result = append(result, member)
	}
	return result, rows.Err()
}

func (s *sqliteStore) ListEffectiveSubscriptionNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.conn().QueryContext(ctx, `SELECT DISTINCT n.id, n.uri, n.name, n.source, n.port,
		n.username, n.password, n.region, n.country, n.enabled, n.tags, n.created_at, n.updated_at
		FROM nodes n JOIN subscription_nodes sn ON sn.node_id=n.id
		JOIN subscriptions s ON s.id=sn.subscription_id
		WHERE n.enabled=1 AND s.enabled=1 ORDER BY n.id`)
	if err != nil {
		return nil, fmt.Errorf("list effective subscription nodes: %w", err)
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (s *sqliteStore) ReplaceSubscriptionNodes(ctx context.Context, subscriptionID int64, nodes []SubscriptionNodeInput) error {
	return s.runInTx(ctx, func(tx *sqliteStore) error {
		return tx.replaceSubscriptionNodes(ctx, subscriptionID, nodes)
	})
}

func (s *sqliteStore) CommitSnapshot(ctx context.Context, subscriptionID int64, nodes []SubscriptionNodeInput, snapshot SubscriptionSnapshot) error {
	return s.runInTx(ctx, func(tx *sqliteStore) error {
		if err := tx.replaceSubscriptionNodes(ctx, subscriptionID, nodes); err != nil {
			return err
		}
		var nodeCount int
		if err := tx.conn().QueryRowContext(ctx,
			"SELECT COUNT(*) FROM subscription_nodes WHERE subscription_id=?", subscriptionID).Scan(&nodeCount); err != nil {
			return fmt.Errorf("count subscription %d nodes: %w", subscriptionID, err)
		}
		result, err := tx.conn().ExecContext(ctx, `UPDATE subscriptions SET last_attempt=?, last_success=?,
			last_error=?, node_count=?, etag=?, last_modified=?, updated_at=? WHERE id=?`,
			formatTime(snapshot.Attempt), formatTime(snapshot.Success), snapshot.Error, nodeCount, snapshot.ETag,
			snapshot.LastModified, formatTime(time.Now().UTC()), subscriptionID)
		if err != nil {
			return fmt.Errorf("commit subscription %d snapshot: %w", subscriptionID, err)
		}
		return requireAffected(result, fmt.Sprintf("subscription %d not found", subscriptionID))
	})
}

func (s *sqliteStore) replaceSubscriptionNodes(ctx context.Context, subscriptionID int64, nodes []SubscriptionNodeInput) error {
	var exists int
	if err := s.conn().QueryRowContext(ctx, "SELECT 1 FROM subscriptions WHERE id=?", subscriptionID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("subscription %d not found", subscriptionID)
		}
		return err
	}
	if _, err := s.conn().ExecContext(ctx, "DELETE FROM subscription_nodes WHERE subscription_id=?", subscriptionID); err != nil {
		return fmt.Errorf("clear subscription %d nodes: %w", subscriptionID, err)
	}
	now := formatTime(time.Now().UTC())
	for position, node := range nodes {
		_, err := s.conn().ExecContext(ctx, `INSERT INTO nodes
			(uri, name, source, port, username, password, region, country, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(uri) DO UPDATE SET name=excluded.name, port=excluded.port,
			username=excluded.username, password=excluded.password, region=excluded.region,
			country=excluded.country, updated_at=excluded.updated_at`, node.URI, node.Name,
			NodeSourceSubscription, node.Port, node.Username, node.Password, node.Region, node.Country,
			boolToInt(node.Enabled), now, now)
		if err != nil {
			return fmt.Errorf("upsert subscription node %q: %w", node.URI, err)
		}
		if _, err := s.conn().ExecContext(ctx, `INSERT INTO subscription_nodes (subscription_id, node_id, position)
			SELECT ?, id, ? FROM nodes WHERE uri=?
			ON CONFLICT(subscription_id, node_id) DO UPDATE SET position=excluded.position`,
			subscriptionID, position, node.URI); err != nil {
			return fmt.Errorf("link subscription node %q: %w", node.URI, err)
		}
	}
	if _, err := s.conn().ExecContext(ctx, "INSERT OR IGNORE INTO node_stats (node_id) SELECT id FROM nodes"); err != nil {
		return fmt.Errorf("create subscription node stats: %w", err)
	}
	return nil
}

// ===================== Node stats =====================

func (s *sqliteStore) GetNodeStats(ctx context.Context, nodeID int64) (*NodeStats, error) {
	row := s.conn().QueryRowContext(ctx,
		`SELECT node_id, failure_count, success_count, blacklisted, blacklisted_until,
		 last_error, last_failure_at, last_success_at, last_latency_ms,
		 available, initial_check_done, total_upload_bytes, total_download_bytes, updated_at
		 FROM node_stats WHERE node_id = ?`, nodeID)

	stats := &NodeStats{}
	var blacklistedUntilStr, lastFailureStr, lastSuccessStr, updatedAtStr string
	var blacklisted, available, initialCheckDone int

	err := row.Scan(
		&stats.NodeID, &stats.FailureCount, &stats.SuccessCount,
		&blacklisted, &blacklistedUntilStr,
		&stats.LastError, &lastFailureStr, &lastSuccessStr,
		&stats.LastLatencyMs, &available, &initialCheckDone,
		&stats.TotalUploadBytes, &stats.TotalDownloadBytes, &updatedAtStr,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get node stats %d: %w", nodeID, err)
	}

	stats.Blacklisted = blacklisted != 0
	stats.Available = available != 0
	stats.InitialCheckDone = initialCheckDone != 0
	stats.BlacklistedUntil = parseTime(blacklistedUntilStr)
	stats.LastFailureAt = parseTime(lastFailureStr)
	stats.LastSuccessAt = parseTime(lastSuccessStr)
	stats.UpdatedAt = parseTime(updatedAtStr)

	return stats, nil
}

func (s *sqliteStore) UpsertNodeStats(ctx context.Context, stats *NodeStats) error {
	now := time.Now().UTC().Format(time.RFC3339)
	blacklisted := 0
	if stats.Blacklisted {
		blacklisted = 1
	}
	available := 0
	if stats.Available {
		available = 1
	}
	initialCheckDone := 0
	if stats.InitialCheckDone {
		initialCheckDone = 1
	}

	_, err := s.conn().ExecContext(ctx,
		`INSERT INTO node_stats (node_id, failure_count, success_count, blacklisted, blacklisted_until,
		 last_error, last_failure_at, last_success_at, last_latency_ms, available, initial_check_done,
		 total_upload_bytes, total_download_bytes, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   failure_count=excluded.failure_count, success_count=excluded.success_count,
		   blacklisted=excluded.blacklisted, blacklisted_until=excluded.blacklisted_until,
		   last_error=excluded.last_error, last_failure_at=excluded.last_failure_at,
		   last_success_at=excluded.last_success_at, last_latency_ms=excluded.last_latency_ms,
		   available=excluded.available, initial_check_done=excluded.initial_check_done,
		   total_upload_bytes=excluded.total_upload_bytes, total_download_bytes=excluded.total_download_bytes,
		   updated_at=excluded.updated_at`,
		stats.NodeID, stats.FailureCount, stats.SuccessCount,
		blacklisted, formatTime(stats.BlacklistedUntil),
		stats.LastError, formatTime(stats.LastFailureAt), formatTime(stats.LastSuccessAt),
		stats.LastLatencyMs, available, initialCheckDone,
		stats.TotalUploadBytes, stats.TotalDownloadBytes, now,
	)
	return err
}

func (s *sqliteStore) RecordSuccess(ctx context.Context, nodeID int64, latencyMs int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.conn().ExecContext(ctx,
		`UPDATE node_stats SET
		 success_count = success_count + 1,
		 last_success_at = ?,
		 last_latency_ms = ?,
		 available = 1,
		 initial_check_done = 1,
		 updated_at = ?
		 WHERE node_id = ?`,
		now, latencyMs, now, nodeID,
	)
	return err
}

func (s *sqliteStore) RecordFailure(ctx context.Context, nodeID int64, errMsg string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.conn().ExecContext(ctx,
		`UPDATE node_stats SET
		 failure_count = failure_count + 1,
		 last_error = ?,
		 last_failure_at = ?,
		 updated_at = ?
		 WHERE node_id = ?`,
		errMsg, now, now, nodeID,
	)
	return err
}

func (s *sqliteStore) SetBlacklist(ctx context.Context, nodeID int64, until time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.conn().ExecContext(ctx,
		`UPDATE node_stats SET
		 blacklisted = 1,
		 blacklisted_until = ?,
		 failure_count = 0,
		 updated_at = ?
		 WHERE node_id = ?`,
		formatTime(until), now, nodeID,
	)
	return err
}

func (s *sqliteStore) ClearBlacklist(ctx context.Context, nodeID int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.conn().ExecContext(ctx,
		`UPDATE node_stats SET
		 blacklisted = 0,
		 blacklisted_until = '',
		 updated_at = ?
		 WHERE node_id = ?`,
		now, nodeID,
	)
	return err
}

func (s *sqliteStore) ClearAllBlacklists(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.conn().ExecContext(ctx,
		`UPDATE node_stats SET blacklisted = 0, blacklisted_until = '', updated_at = ? WHERE blacklisted = 1`,
		now,
	)
	return err
}

func (s *sqliteStore) BatchUpdateStats(ctx context.Context, updates []StatsUpdate) error {
	if len(updates) == 0 {
		return nil
	}

	execFn := func(txStore *sqliteStore) error {
		now := time.Now().UTC().Format(time.RFC3339)
		stmt, err := txStore.conn().PrepareContext(ctx,
			`INSERT INTO node_stats (node_id, failure_count, success_count, blacklisted, blacklisted_until,
			 last_error, last_failure_at, last_success_at, last_latency_ms, available, initial_check_done,
			 total_upload_bytes, total_download_bytes, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(node_id) DO UPDATE SET
			   failure_count=excluded.failure_count, success_count=excluded.success_count,
			   blacklisted=excluded.blacklisted, blacklisted_until=excluded.blacklisted_until,
			   last_error=excluded.last_error, last_failure_at=excluded.last_failure_at,
			   last_success_at=excluded.last_success_at, last_latency_ms=excluded.last_latency_ms,
			   available=excluded.available, initial_check_done=excluded.initial_check_done,
			   total_upload_bytes=excluded.total_upload_bytes, total_download_bytes=excluded.total_download_bytes,
			   updated_at=excluded.updated_at`)
		if err != nil {
			return fmt.Errorf("prepare batch stats: %w", err)
		}
		defer stmt.Close()

		for _, u := range updates {
			blacklisted := 0
			if u.Blacklisted {
				blacklisted = 1
			}
			available := 0
			if u.Available {
				available = 1
			}
			initialCheckDone := 0
			if u.InitialCheckDone {
				initialCheckDone = 1
			}

			_, err := stmt.ExecContext(ctx,
				u.NodeID, u.FailureCount, u.SuccessCount,
				blacklisted, formatTime(u.BlacklistedUntil),
				u.LastError, formatTime(u.LastFailureAt), formatTime(u.LastSuccessAt),
				u.LastLatencyMs, available, initialCheckDone,
				u.TotalUploadBytes, u.TotalDownloadBytes, now,
			)
			if err != nil {
				return fmt.Errorf("batch update stats for node %d: %w", u.NodeID, err)
			}
		}
		return nil
	}

	if s.tx != nil {
		return execFn(s)
	}
	return s.WithTx(ctx, func(tx Store) error {
		return execFn(tx.(*sqliteStore))
	})
}

func (s *sqliteStore) GetAllNodeStats(ctx context.Context) (map[int64]*NodeStats, error) {
	rows, err := s.conn().QueryContext(ctx,
		`SELECT node_id, failure_count, success_count, blacklisted, blacklisted_until,
		 last_error, last_failure_at, last_success_at, last_latency_ms,
		 available, initial_check_done, total_upload_bytes, total_download_bytes, updated_at
		 FROM node_stats`)
	if err != nil {
		return nil, fmt.Errorf("get all node stats: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]*NodeStats)
	for rows.Next() {
		stats := &NodeStats{}
		var blacklistedUntilStr, lastFailureStr, lastSuccessStr, updatedAtStr string
		var blacklisted, available, initialCheckDone int

		err := rows.Scan(
			&stats.NodeID, &stats.FailureCount, &stats.SuccessCount,
			&blacklisted, &blacklistedUntilStr,
			&stats.LastError, &lastFailureStr, &lastSuccessStr,
			&stats.LastLatencyMs, &available, &initialCheckDone,
			&stats.TotalUploadBytes, &stats.TotalDownloadBytes, &updatedAtStr,
		)
		if err != nil {
			return nil, fmt.Errorf("scan node stats: %w", err)
		}

		stats.Blacklisted = blacklisted != 0
		stats.Available = available != 0
		stats.InitialCheckDone = initialCheckDone != 0
		stats.BlacklistedUntil = parseTime(blacklistedUntilStr)
		stats.LastFailureAt = parseTime(lastFailureStr)
		stats.LastSuccessAt = parseTime(lastSuccessStr)
		stats.UpdatedAt = parseTime(updatedAtStr)

		result[stats.NodeID] = stats
	}
	return result, rows.Err()
}

// ===================== Timeline =====================

func (s *sqliteStore) AppendTimeline(ctx context.Context, nodeID int64, event TimelineEvent) error {
	_, err := s.conn().ExecContext(ctx,
		`INSERT INTO node_timeline (node_id, success, latency_ms, error, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		nodeID, boolToInt(event.Success), event.LatencyMs, event.Error,
		time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

func (s *sqliteStore) GetTimeline(ctx context.Context, nodeID int64, limit int) ([]TimelineEvent, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := s.conn().QueryContext(ctx,
		`SELECT id, node_id, success, latency_ms, error, created_at
		 FROM node_timeline WHERE node_id = ?
		 ORDER BY id DESC LIMIT ?`,
		nodeID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get timeline for node %d: %w", nodeID, err)
	}
	defer rows.Close()

	var events []TimelineEvent
	for rows.Next() {
		var evt TimelineEvent
		var success int
		var createdAtStr string
		err := rows.Scan(&evt.ID, &evt.NodeID, &success, &evt.LatencyMs, &evt.Error, &createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("scan timeline event: %w", err)
		}
		evt.Success = success != 0
		evt.CreatedAt = parseTime(createdAtStr)
		events = append(events, evt)
	}

	// Reverse to get chronological order
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events, rows.Err()
}

func (s *sqliteStore) CleanupTimeline(ctx context.Context, keepPerNode int) error {
	if keepPerNode <= 0 {
		keepPerNode = 20
	}

	_, err := s.conn().ExecContext(ctx,
		`DELETE FROM node_timeline WHERE id NOT IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY node_id ORDER BY id DESC) as rn
				FROM node_timeline
			) WHERE rn <= ?
		)`, keepPerNode,
	)
	return err
}

// ===================== Sessions =====================

func (s *sqliteStore) CreateSession(ctx context.Context, session *Session) error {
	_, err := s.conn().ExecContext(ctx,
		`INSERT INTO sessions (token, created_at, expires_at) VALUES (?, ?, ?)`,
		session.Token,
		session.CreatedAt.UTC().Format(time.RFC3339),
		session.ExpiresAt.UTC().Format(time.RFC3339),
	)
	return err
}

func (s *sqliteStore) GetSession(ctx context.Context, token string) (*Session, error) {
	row := s.conn().QueryRowContext(ctx,
		"SELECT token, created_at, expires_at FROM sessions WHERE token = ?", token)

	var sess Session
	var createdAtStr, expiresAtStr string
	err := row.Scan(&sess.Token, &createdAtStr, &expiresAtStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	sess.CreatedAt = parseTime(createdAtStr)
	sess.ExpiresAt = parseTime(expiresAtStr)
	return &sess, nil
}

func (s *sqliteStore) DeleteSession(ctx context.Context, token string) error {
	_, err := s.conn().ExecContext(ctx, "DELETE FROM sessions WHERE token = ?", token)
	return err
}

func (s *sqliteStore) CleanupExpiredSessions(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.conn().ExecContext(ctx, "DELETE FROM sessions WHERE expires_at < ?", now)
	return err
}

// ===================== Subscription status =====================

func (s *sqliteStore) GetSubscriptionStatus(ctx context.Context) (*SubscriptionStatus, error) {
	row := s.conn().QueryRowContext(ctx,
		`SELECT last_refresh, next_refresh, node_count, last_error,
		 refresh_count, is_refreshing, nodes_hash, updated_at
		 FROM subscription_status WHERE id = 1`)

	var status SubscriptionStatus
	var lastRefreshStr, nextRefreshStr, updatedAtStr string
	var isRefreshing int

	err := row.Scan(
		&lastRefreshStr, &nextRefreshStr, &status.NodeCount,
		&status.LastError, &status.RefreshCount, &isRefreshing,
		&status.NodesHash, &updatedAtStr,
	)
	if err == sql.ErrNoRows {
		return &SubscriptionStatus{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get subscription status: %w", err)
	}

	status.IsRefreshing = isRefreshing != 0
	status.LastRefresh = parseTime(lastRefreshStr)
	status.NextRefresh = parseTime(nextRefreshStr)
	status.UpdatedAt = parseTime(updatedAtStr)

	return &status, nil
}

func (s *sqliteStore) UpdateSubscriptionStatus(ctx context.Context, status *SubscriptionStatus) error {
	now := time.Now().UTC().Format(time.RFC3339)
	isRefreshing := 0
	if status.IsRefreshing {
		isRefreshing = 1
	}

	_, err := s.conn().ExecContext(ctx,
		`INSERT INTO subscription_status (id, last_refresh, next_refresh, node_count, last_error,
		 refresh_count, is_refreshing, nodes_hash, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   last_refresh=excluded.last_refresh, next_refresh=excluded.next_refresh,
		   node_count=excluded.node_count, last_error=excluded.last_error,
		   refresh_count=excluded.refresh_count, is_refreshing=excluded.is_refreshing,
		   nodes_hash=excluded.nodes_hash, updated_at=excluded.updated_at`,
		formatTime(status.LastRefresh), formatTime(status.NextRefresh),
		status.NodeCount, status.LastError, status.RefreshCount,
		isRefreshing, status.NodesHash, now,
	)
	return err
}

// ===================== Unlock detection results =====================

const unlockColumns = `node_id, tag, name, netflix_status, disney_plus_status, chatgpt_status,
	ip, ip_country, ip_iso_code, ip_region, ip_pure, error, duration_ms, checked_at,
	result_json, updated_at`

// serviceStatus extracts the Status of the named service from an UnlockResult's
// Services slice, defaulting to "" when the service was not recorded.
func serviceStatus(services []UnlockServiceResult, name string) string {
	for _, s := range services {
		if s.Name == name {
			return s.Status
		}
	}
	return ""
}

func (s *sqliteStore) UpsertUnlockResult(ctx context.Context, result *UnlockResult) error {
	if result == nil {
		return fmt.Errorf("upsert unlock result: nil result")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	checkedAt := now
	if !result.CheckedAt.IsZero() {
		checkedAt = formatTime(result.CheckedAt)
	}

	_, err := s.conn().ExecContext(ctx,
		`INSERT INTO node_unlock_results (`+unlockColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
		   tag=excluded.tag, name=excluded.name,
		   netflix_status=excluded.netflix_status, disney_plus_status=excluded.disney_plus_status,
		   chatgpt_status=excluded.chatgpt_status,
		   ip=excluded.ip, ip_country=excluded.ip_country, ip_iso_code=excluded.ip_iso_code,
		   ip_region=excluded.ip_region, ip_pure=excluded.ip_pure,
		   error=excluded.error, duration_ms=excluded.duration_ms, checked_at=excluded.checked_at,
		   result_json=excluded.result_json, updated_at=excluded.updated_at`,
		result.NodeID, result.Tag, result.Name,
		serviceStatus(result.Services, "netflix"), serviceStatus(result.Services, "disney_plus"),
		serviceStatus(result.Services, "chatgpt"),
		result.IP.IP, result.IP.Country, result.IP.ISOCode, result.IP.Region,
		boolToInt(result.IP.Pure), result.Error, result.Duration, checkedAt,
		result.ResultJSON, now,
	)
	if err != nil {
		return fmt.Errorf("upsert unlock result for node %d: %w", result.NodeID, err)
	}
	return nil
}

func (s *sqliteStore) GetUnlockResult(ctx context.Context, nodeID int64) (*UnlockResult, error) {
	row := s.conn().QueryRowContext(ctx,
		"SELECT "+unlockColumns+" FROM node_unlock_results WHERE node_id = ?", nodeID)
	return scanUnlockResult(row)
}

func (s *sqliteStore) ListUnlockResults(ctx context.Context) (map[int64]*UnlockResult, error) {
	rows, err := s.conn().QueryContext(ctx, "SELECT "+unlockColumns+" FROM node_unlock_results")
	if err != nil {
		return nil, fmt.Errorf("list unlock results: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]*UnlockResult)
	for rows.Next() {
		r, err := scanUnlockResult(rows)
		if err != nil {
			return nil, err
		}
		if r != nil {
			result[r.NodeID] = r
		}
	}
	return result, rows.Err()
}

// scanUnlockResult scans one node_unlock_results row into an UnlockResult,
// reconstructing the Services slice from the three indexed status columns when
// result_json is unavailable, otherwise from the stored JSON payload.
func scanUnlockResult(row scanner) (*UnlockResult, error) {
	var r UnlockResult
	var ipPure int
	var checkedAtStr, updatedAtStr, resultJSON string
	var netflixStatus, disneyPlusStatus, chatgptStatus string

	err := row.Scan(
		&r.NodeID, &r.Tag, &r.Name,
		&netflixStatus, &disneyPlusStatus, &chatgptStatus,
		&r.IP.IP, &r.IP.Country, &r.IP.ISOCode, &r.IP.Region, &ipPure,
		&r.Error, &r.Duration, &checkedAtStr,
		&resultJSON, &updatedAtStr,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan unlock result: %w", err)
	}

	r.IP.Pure = ipPure != 0
	r.CheckedAt = parseTime(checkedAtStr)
	r.UpdatedAt = parseTime(updatedAtStr)
	r.ResultJSON = resultJSON

	// Prefer the full JSON payload; fall back to the three indexed columns.
	if resultJSON != "" {
		var full struct {
			Services []UnlockServiceResult `json:"services"`
		}
		if json.Unmarshal([]byte(resultJSON), &full) == nil && len(full.Services) > 0 {
			r.Services = full.Services
		}
	}
	if r.Services == nil {
		r.Services = servicesFromStatuses(netflixStatus, disneyPlusStatus, chatgptStatus)
	}

	return &r, nil
}

// servicesFromStatuses rebuilds a minimal Services slice from the indexed
// status columns when no full JSON payload is stored.
func servicesFromStatuses(netflix, disneyPlus, chatgpt string) []UnlockServiceResult {
	display := map[string]string{
		"netflix":     "Netflix",
		"disney_plus": "Disney+",
		"chatgpt":     "ChatGPT",
	}
	return []UnlockServiceResult{
		{Name: "netflix", DisplayName: display["netflix"], Status: netflix},
		{Name: "disney_plus", DisplayName: display["disney_plus"], Status: disneyPlus},
		{Name: "chatgpt", DisplayName: display["chatgpt"], Status: chatgpt},
	}
}

// ===================== Group pool operations =====================

const groupPoolColumns = `id, name, bind_address, bind_port, protocol, username, password,
dispatch_mode, regions_json, explicit_node_ids_json, excluded_node_ids_json, failure_window_seconds,
failure_threshold, health_check_seconds, current_active_node_id, enabled,
subscription_enabled, subscription_token, subscription_mode, external_host, created_at, updated_at`

func (s *sqliteStore) ListGroupPools(ctx context.Context) ([]GroupPool, error) {
	rows, err := s.conn().QueryContext(ctx, "SELECT "+groupPoolColumns+" FROM group_pools ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("list group pools: %w", err)
	}
	var groups []GroupPool
	for rows.Next() {
		g, err := scanGroupPool(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for idx := range groups {
		states, err := s.listGroupNodeStates(ctx, groups[idx].ID)
		if err != nil {
			return nil, err
		}
		groups[idx].NodeStates = states
	}
	return groups, nil
}

func (s *sqliteStore) GetGroupPool(ctx context.Context, id int64) (*GroupPool, error) {
	g, err := scanGroupPool(s.conn().QueryRowContext(ctx, "SELECT "+groupPoolColumns+" FROM group_pools WHERE id = ?", id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.NodeStates, err = s.listGroupNodeStates(ctx, id)
	return &g, err
}

func (s *sqliteStore) CreateGroupPool(ctx context.Context, g *GroupPool) error {
	regions, _ := json.Marshal(g.Regions)
	nodeIDs, _ := json.Marshal(g.ExplicitNodeIDs)
	excludedNodeIDs, _ := json.Marshal(g.ExcludedNodeIDs)
	result, err := s.conn().ExecContext(ctx, `INSERT INTO group_pools
(name, bind_address, bind_port, protocol, username, password, dispatch_mode, regions_json,
 explicit_node_ids_json, excluded_node_ids_json, failure_window_seconds, failure_threshold, health_check_seconds,
 current_active_node_id, enabled, subscription_enabled, subscription_token, subscription_mode,
 external_host, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.Name, g.BindAddress, g.BindPort, g.Protocol, g.Username, g.Password, g.DispatchMode,
		string(regions), string(nodeIDs), string(excludedNodeIDs), g.FailureWindowSeconds, g.FailureThreshold,
		g.HealthCheckSeconds, g.CurrentActiveNodeID, boolToInt(g.Enabled), boolToInt(g.SubscriptionEnabled),
		g.SubscriptionToken, g.SubscriptionMode, g.ExternalHost, formatTime(time.Now()), formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("create group pool: %w", err)
	}
	g.ID, err = result.LastInsertId()
	return err
}

func (s *sqliteStore) UpdateGroupPool(ctx context.Context, g *GroupPool) error {
	regions, _ := json.Marshal(g.Regions)
	nodeIDs, _ := json.Marshal(g.ExplicitNodeIDs)
	excludedNodeIDs, _ := json.Marshal(g.ExcludedNodeIDs)
	result, err := s.conn().ExecContext(ctx, `UPDATE group_pools SET
name=?, bind_address=?, bind_port=?, protocol=?, username=?, password=?, dispatch_mode=?,
regions_json=?, explicit_node_ids_json=?, excluded_node_ids_json=?, failure_window_seconds=?, failure_threshold=?,
health_check_seconds=?, current_active_node_id=?, enabled=?, subscription_enabled=?, subscription_token=?,
subscription_mode=?, external_host=?, updated_at=? WHERE id=?`,
		g.Name, g.BindAddress, g.BindPort, g.Protocol, g.Username, g.Password, g.DispatchMode,
		string(regions), string(nodeIDs), string(excludedNodeIDs), g.FailureWindowSeconds, g.FailureThreshold,
		g.HealthCheckSeconds, g.CurrentActiveNodeID, boolToInt(g.Enabled), boolToInt(g.SubscriptionEnabled),
		g.SubscriptionToken, g.SubscriptionMode, g.ExternalHost, formatTime(time.Now()), g.ID)
	if err != nil {
		return fmt.Errorf("update group pool: %w", err)
	}
	return requireAffected(result, "group pool not found")
}

func (s *sqliteStore) UpdateGroupCurrentActiveNode(ctx context.Context, groupID, nodeID int64) error {
	result, err := s.conn().ExecContext(ctx,
		"UPDATE group_pools SET current_active_node_id=?, updated_at=? WHERE id=?",
		nodeID, formatTime(time.Now()), groupID)
	if err != nil {
		return fmt.Errorf("update group current active node: %w", err)
	}
	return requireAffected(result, "group pool not found")
}

func (s *sqliteStore) DeleteGroupPool(ctx context.Context, id int64) error {
	result, err := s.conn().ExecContext(ctx, "DELETE FROM group_pools WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete group pool: %w", err)
	}
	return requireAffected(result, "group pool not found")
}

func (s *sqliteStore) UpsertGroupNodeState(ctx context.Context, state *GroupNodeState) error {
	history, _ := json.Marshal(state.FailureHistory)
	_, err := s.conn().ExecContext(ctx, `INSERT INTO group_node_states
(group_id, node_id, failure_history_json, evicted, last_error, evicted_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(group_id, node_id) DO UPDATE SET failure_history_json=excluded.failure_history_json,
evicted=excluded.evicted, last_error=excluded.last_error, evicted_at=excluded.evicted_at,
updated_at=excluded.updated_at`, state.GroupID, state.NodeID, string(history), boolToInt(state.Evicted),
		state.LastError, formatTime(state.EvictedAt), formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("upsert group node state: %w", err)
	}
	return nil
}

func (s *sqliteStore) ClearGroupNodeState(ctx context.Context, groupID, nodeID int64) error {
	_, err := s.conn().ExecContext(ctx, "DELETE FROM group_node_states WHERE group_id = ? AND node_id = ?", groupID, nodeID)
	return err
}

func (s *sqliteStore) listGroupNodeStates(ctx context.Context, groupID int64) ([]GroupNodeState, error) {
	rows, err := s.conn().QueryContext(ctx, `SELECT group_id, node_id, failure_history_json, evicted,
last_error, evicted_at, updated_at FROM group_node_states WHERE group_id = ?`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group node states: %w", err)
	}
	defer rows.Close()
	var states []GroupNodeState
	for rows.Next() {
		var state GroupNodeState
		var history, evictedAt, updatedAt string
		var evicted int
		if err := rows.Scan(&state.GroupID, &state.NodeID, &history, &evicted, &state.LastError, &evictedAt, &updatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(history), &state.FailureHistory)
		state.Evicted = evicted != 0
		state.EvictedAt = parseTime(evictedAt)
		state.UpdatedAt = parseTime(updatedAt)
		states = append(states, state)
	}
	return states, rows.Err()
}

func scanGroupPool(row scanner) (GroupPool, error) {
	var g GroupPool
	var regions, nodeIDs, excludedNodeIDs, createdAt, updatedAt string
	var enabled, subscriptionEnabled int
	err := row.Scan(&g.ID, &g.Name, &g.BindAddress, &g.BindPort, &g.Protocol, &g.Username, &g.Password,
		&g.DispatchMode, &regions, &nodeIDs, &excludedNodeIDs, &g.FailureWindowSeconds, &g.FailureThreshold,
		&g.HealthCheckSeconds, &g.CurrentActiveNodeID, &enabled, &subscriptionEnabled,
		&g.SubscriptionToken, &g.SubscriptionMode, &g.ExternalHost, &createdAt, &updatedAt)
	if err != nil {
		return g, err
	}
	_ = json.Unmarshal([]byte(regions), &g.Regions)
	_ = json.Unmarshal([]byte(nodeIDs), &g.ExplicitNodeIDs)
	_ = json.Unmarshal([]byte(excludedNodeIDs), &g.ExcludedNodeIDs)
	g.Enabled = enabled != 0
	g.SubscriptionEnabled = subscriptionEnabled != 0
	g.CreatedAt, g.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	return g, nil
}

// ===================== Lifecycle =====================

func (s *sqliteStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *sqliteStore) WithTx(ctx context.Context, fn func(tx Store) error) error {
	if s.tx != nil {
		// Already in a transaction, just execute
		return fn(s)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	txStore := &sqliteStore{db: s.db, tx: tx}
	if err := fn(txStore); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *sqliteStore) runInTx(ctx context.Context, fn func(tx *sqliteStore) error) error {
	if s.tx != nil {
		return fn(s)
	}
	return s.WithTx(ctx, func(tx Store) error { return fn(tx.(*sqliteStore)) })
}

// ===================== Helpers =====================

type scanner interface {
	Scan(dest ...any) error
}

func scanSubscriptionRow(row *sql.Row) (*Subscription, error) {
	subscription, err := scanSubscription(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &subscription, nil
}

func scanSubscription(row scanner) (Subscription, error) {
	var subscription Subscription
	var enabled int
	var lastAttempt, lastSuccess, createdAt, updatedAt string
	err := row.Scan(&subscription.ID, &subscription.Name, &subscription.URL, &enabled,
		&subscription.RefreshIntervalSeconds, &subscription.RefreshTimeoutSeconds, &subscription.SortOrder,
		&lastAttempt, &lastSuccess, &subscription.LastError, &subscription.NodeCount, &subscription.ETag,
		&subscription.LastModified, &createdAt, &updatedAt)
	subscription.Enabled = enabled != 0
	subscription.LastAttempt, subscription.LastSuccess = parseTime(lastAttempt), parseTime(lastSuccess)
	subscription.CreatedAt, subscription.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	return subscription, err
}

func requireAffected(result sql.Result, notFound string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%s", notFound)
	}
	return nil
}

func scanNode(row *sql.Row) (*Node, error) {
	var n Node
	var enabled int
	var createdAtStr, updatedAtStr, tagsJSON string

	err := row.Scan(
		&n.ID, &n.URI, &n.Name, &n.Source, &n.Port,
		&n.Username, &n.Password, &n.Region, &n.Country,
		&enabled, &tagsJSON, &createdAtStr, &updatedAtStr,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	n.Enabled = enabled != 0
	if tagsJSON != "" && tagsJSON != "[]" {
		_ = json.Unmarshal([]byte(tagsJSON), &n.Tags)
	}
	n.CreatedAt = parseTime(createdAtStr)
	n.UpdatedAt = parseTime(updatedAtStr)
	return &n, nil
}

func scanNodes(rows *sql.Rows) ([]Node, error) {
	var nodes []Node
	for rows.Next() {
		var n Node
		var enabled int
		var createdAtStr, updatedAtStr, tagsJSON string

		err := rows.Scan(
			&n.ID, &n.URI, &n.Name, &n.Source, &n.Port,
			&n.Username, &n.Password, &n.Region, &n.Country,
			&enabled, &tagsJSON, &createdAtStr, &updatedAtStr,
		)
		if err != nil {
			return nil, err
		}

		n.Enabled = enabled != 0
		if tagsJSON != "" && tagsJSON != "[]" {
			_ = json.Unmarshal([]byte(tagsJSON), &n.Tags)
		}
		n.CreatedAt = parseTime(createdAtStr)
		n.UpdatedAt = parseTime(updatedAtStr)
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// Try other formats
		t, err = time.Parse("2006-01-02 15:04:05", s)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
