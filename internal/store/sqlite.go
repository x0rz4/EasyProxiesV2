package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/nodecodec"

	_ "modernc.org/sqlite"
)

// sqliteStore implements Store using SQLite.
type sqliteStore struct {
	writerDB *sql.DB
	readerDB *sql.DB
	tx       *sql.Tx // non-nil when operating inside WithTx
}

const (
	sqliteWriterConnections = 1
	sqliteReaderConnections = 4
)

// Open creates a new SQLite-backed Store at the given path.
// It applies all pending migrations and sets optimal PRAGMAs.
func Open(dbPath string) (Store, error) {
	connectionPragmas := "_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-64000)&_pragma=foreign_keys(ON)"
	writerDSN := dbPath + "?_pragma=journal_mode(WAL)&" + connectionPragmas

	writerDB, err := sql.Open("sqlite", writerDSN)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dbPath, err)
	}

	writerDB.SetMaxOpenConns(sqliteWriterConnections)
	writerDB.SetMaxIdleConns(sqliteWriterConnections)
	writerDB.SetConnMaxLifetime(0)

	if err := writerDB.Ping(); err != nil {
		_ = writerDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	if err := Migrate(writerDB); err != nil {
		_ = writerDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := reconcileNodeIdentities(writerDB); err != nil {
		_ = writerDB.Close()
		return nil, fmt.Errorf("reconcile node identities: %w", err)
	}

	// journal_mode is database-wide and was established by the writer above.
	// Reader connections only apply connection-local settings before becoming
	// query-only, avoiding journal-mode lock negotiation as the pool expands.
	readerDSN := dbPath + "?" + connectionPragmas + "&_pragma=query_only(ON)"
	readerDB, err := sql.Open("sqlite", readerDSN)
	if err != nil {
		_ = writerDB.Close()
		return nil, fmt.Errorf("open sqlite reader %q: %w", dbPath, err)
	}
	readerDB.SetMaxOpenConns(sqliteReaderConnections)
	readerDB.SetMaxIdleConns(sqliteReaderConnections)
	readerDB.SetConnMaxLifetime(0)
	if err := readerDB.Ping(); err != nil {
		_ = readerDB.Close()
		_ = writerDB.Close()
		return nil, fmt.Errorf("ping sqlite reader: %w", err)
	}
	var queryOnly int
	if err := readerDB.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil {
		_ = readerDB.Close()
		_ = writerDB.Close()
		return nil, fmt.Errorf("verify sqlite reader query_only: %w", err)
	}
	if queryOnly != 1 {
		_ = readerDB.Close()
		_ = writerDB.Close()
		return nil, errors.New("verify sqlite reader query_only: pragma is disabled")
	}

	log.Printf("[store] SQLite store opened: %s", dbPath)
	return &sqliteStore{writerDB: writerDB, readerDB: readerDB}, nil
}

// readConn routes pure reads to the reader pool. Reads made through a
// transaction-scoped store stay on that transaction to preserve consistency.
func (s *sqliteStore) readConn() querier {
	if s.tx != nil {
		return s.tx
	}
	return s.readerDB
}

// writeConn routes mutations and their supporting reads to the single-writer
// pool. A transaction-scoped store always resolves to its current transaction.
func (s *sqliteStore) writeConn() querier {
	if s.tx != nil {
		return s.tx
	}
	return s.writerDB
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
	if len(filter.NodeIDs) > sqliteMaxVariables {
		return s.listNodesChunked(ctx, filter)
	}
	query := "SELECT id, uri, name, source, port, username, password, region, country, enabled, tags, identity_hash, canonical_json, created_at, updated_at FROM nodes"
	conditions, args := nodeFilterConditions(filter)

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

	rows, err := s.readConn().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()

	return scanNodes(rows)
}

// listNodesChunked keeps a large NodeIDs filter under the SQLite bound-variable
// limit by querying in chunks and paginating the merged result in Go.
func (s *sqliteStore) listNodesChunked(ctx context.Context, filter NodeFilter) ([]Node, error) {
	limit, offset := filter.Limit, filter.Offset
	chunkFilter := filter
	chunkFilter.Limit, chunkFilter.Offset = 0, 0
	var merged []Node
	for _, chunk := range chunkIDs(filter.NodeIDs, sqliteMaxVariables) {
		chunkFilter.NodeIDs = chunk
		nodes, err := s.ListNodes(ctx, chunkFilter)
		if err != nil {
			return nil, err
		}
		merged = append(merged, nodes...)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	if offset > 0 {
		if offset >= len(merged) {
			return nil, nil
		}
		merged = merged[offset:]
	}
	if limit > 0 && limit < len(merged) {
		merged = merged[:limit]
	}
	return merged, nil
}

// nodeFilterConditions builds the shared WHERE fragments for node listing and
// counting so both stay in sync.
func nodeFilterConditions(filter NodeFilter) ([]string, []any) {
	var conditions []string
	var args []any
	if len(filter.NodeIDs) > 0 {
		conditions = append(conditions, "id IN "+inClause(len(filter.NodeIDs)))
		args = append(args, idArgs(filter.NodeIDs)...)
	}
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
	return conditions, args
}

func (s *sqliteStore) ListManagedNodes(ctx context.Context, subscriptionID *int64) ([]ManagedNode, error) {
	query := `SELECT n.id, n.uri, n.name, n.source, n.port, n.username, n.password,
		n.region, n.country, n.enabled, n.tags, n.identity_hash, n.canonical_json, n.created_at, n.updated_at,
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

	rows, err := s.readConn().QueryContext(ctx, query, args...)
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
			&tagsJSON, &managed.IdentityHash, &managed.CanonicalJSON, &createdAt, &updatedAt, &subscriptionIDs); err != nil {
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
	row := s.readConn().QueryRowContext(ctx,
		"SELECT id, uri, name, source, port, username, password, region, country, enabled, tags, identity_hash, canonical_json, created_at, updated_at FROM nodes WHERE id = ?", id)
	return scanNode(row)
}

func (s *sqliteStore) GetNodeByURI(ctx context.Context, uri string) (*Node, error) {
	row := s.readConn().QueryRowContext(ctx,
		"SELECT id, uri, name, source, port, username, password, region, country, enabled, tags, identity_hash, canonical_json, created_at, updated_at FROM nodes WHERE uri = ?", uri)
	return scanNode(row)
}

func (s *sqliteStore) GetNodeByIdentity(ctx context.Context, identityHash string) (*Node, error) {
	row := s.readConn().QueryRowContext(ctx,
		"SELECT id, uri, name, source, port, username, password, region, country, enabled, tags, identity_hash, canonical_json, created_at, updated_at FROM nodes WHERE identity_hash = ?", identityHash)
	return scanNode(row)
}

func (s *sqliteStore) GetNodeByName(ctx context.Context, name string) (*Node, error) {
	row := s.readConn().QueryRowContext(ctx,
		"SELECT id, uri, name, source, port, username, password, region, country, enabled, tags, identity_hash, canonical_json, created_at, updated_at FROM nodes WHERE name = ?", name)
	return scanNode(row)
}

func (s *sqliteStore) CreateNode(ctx context.Context, node *Node) error {
	if err := populateNodeIdentity(node, true); err != nil {
		return err
	}
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

	result, err := s.writeConn().ExecContext(ctx,
		`INSERT INTO nodes (uri, name, source, port, username, password, region, country, enabled, tags, identity_hash, canonical_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		node.URI, node.Name, node.Source, node.Port,
		node.Username, node.Password, node.Region, node.Country,
		enabled, tagsJSON, node.IdentityHash, node.CanonicalJSON, now, now,
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
	_, err = s.writeConn().ExecContext(ctx,
		"INSERT OR IGNORE INTO node_stats (node_id) VALUES (?)", id)
	if err != nil {
		return fmt.Errorf("create initial node stats: %w", err)
	}

	// Materialize the tags written above as real manual assignments. nodes.tags
	// is only a projection of node_tags, so tags that exist nowhere else would
	// be erased by the first recompute.
	return s.materializeManualNodeTags(ctx, node.ID, node.Tags)
}

// materializeManualNodeTags turns tag names into manual assignments, creating
// missing tags on the way, and rewrites the node's projection.
func (s *sqliteStore) materializeManualNodeTags(ctx context.Context, nodeID int64, names []string) error {
	wanted := dedupeSorted(names)
	if len(wanted) == 0 {
		return nil
	}
	return s.runInTx(ctx, func(tx *sqliteStore) error {
		tagIDs := make([]int64, 0, len(wanted))
		for _, name := range wanted {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if _, err := tx.writeConn().ExecContext(ctx,
				"INSERT OR IGNORE INTO tags (name) VALUES (?)", name); err != nil {
				return fmt.Errorf("create tag %q: %w", name, err)
			}
			var tagID int64
			if err := tx.writeConn().QueryRowContext(ctx,
				"SELECT id FROM tags WHERE name = ?", name).Scan(&tagID); err != nil {
				return fmt.Errorf("lookup tag %q: %w", name, err)
			}
			tagIDs = append(tagIDs, tagID)
		}
		if err := tx.insertNodeTags(ctx, nodeID, tagIDs, nil, NodeTagSourceManual); err != nil {
			return err
		}
		return tx.refreshNodeTagProjection(ctx, []int64{nodeID})
	})
}

func (s *sqliteStore) UpdateNode(ctx context.Context, node *Node) error {
	var previousIdentity string
	if err := s.writeConn().QueryRowContext(ctx, "SELECT identity_hash FROM nodes WHERE id=?", node.ID).Scan(&previousIdentity); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("node %d not found", node.ID)
		}
		return fmt.Errorf("lookup node %d identity: %w", node.ID, err)
	}
	if err := populateNodeIdentity(node, true); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	enabled := 0
	if node.Enabled {
		enabled = 1
	}

	// nodes.tags is deliberately absent: it is a projection of node_tags owned by
	// the tagging layer, so an UpdateNode caller carrying a stale (or empty) Tags
	// slice cannot drop assignments. Use SetManualNodeTags to change tags.
	result, err := s.writeConn().ExecContext(ctx,
		`UPDATE nodes SET uri=?, name=?, source=?, port=?, username=?, password=?,
		 region=?, country=?, enabled=?, identity_hash=?, canonical_json=?, updated_at=?
		 WHERE id=?`,
		node.URI, node.Name, node.Source, node.Port,
		node.Username, node.Password, node.Region, node.Country,
		enabled, node.IdentityHash, node.CanonicalJSON, now, node.ID,
	)
	if err != nil {
		return fmt.Errorf("update node %d: %w", node.ID, err)
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("node %d not found", node.ID)
	}
	if previousIdentity != "" && previousIdentity != node.IdentityHash {
		if err := s.clearNodeDetectionCache(ctx, node.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqliteStore) UpdateNodeLocation(ctx context.Context, nodeID int64, region, country string) error {
	result, err := s.writeConn().ExecContext(ctx, `UPDATE nodes SET region=?,country=?,updated_at=? WHERE id=?`,
		strings.ToLower(strings.TrimSpace(region)), strings.TrimSpace(country), formatTime(time.Now().UTC()), nodeID)
	if err != nil {
		return fmt.Errorf("update node %d landing location: %w", nodeID, err)
	}
	return requireAffected(result, fmt.Sprintf("node %d not found", nodeID))
}

// clearNodeDetectionCache drops the cached facts of a node whose identity
// changed. Auto tags are derived from exactly those facts, so they go too — an
// auto tag must never outlive its evidence. Manual assignments stay, and the
// caller is expected to enqueue the node for a recompute.
func (s *sqliteStore) clearNodeDetectionCache(ctx context.Context, nodeID int64) error {
	for _, table := range []string{"node_detection_results", "node_ip_quality_results", "node_unlock_results"} {
		if _, err := s.writeConn().ExecContext(ctx, "DELETE FROM "+table+" WHERE node_id=?", nodeID); err != nil {
			return fmt.Errorf("clear node %d cached detection from %s: %w", nodeID, table, err)
		}
	}
	if _, err := s.writeConn().ExecContext(ctx,
		"DELETE FROM node_tags WHERE node_id=? AND source=?", nodeID, NodeTagSourceAuto); err != nil {
		return fmt.Errorf("clear node %d auto tags: %w", nodeID, err)
	}
	return s.refreshNodeTagProjection(ctx, []int64{nodeID})
}

func (s *sqliteStore) DeleteNode(ctx context.Context, id int64) error {
	result, err := s.writeConn().ExecContext(ctx, "DELETE FROM nodes WHERE id = ?", id)
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
	result, err := s.writeConn().ExecContext(ctx, "DELETE FROM nodes WHERE source = ?", source)
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
		stmt, err := txStore.writeConn().PrepareContext(ctx,
			`INSERT INTO nodes (uri, name, source, port, username, password, region, country, enabled, identity_hash, canonical_json, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(identity_hash) WHERE identity_hash<>'' DO UPDATE SET
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
			if err := populateNodeIdentity(n, true); err != nil {
				return err
			}
			enabled := 0
			if n.Enabled {
				enabled = 1
			}
			result, err := stmt.ExecContext(ctx,
				n.URI, n.Name, n.Source, n.Port,
				n.Username, n.Password, n.Region, n.Country,
				enabled, n.IdentityHash, n.CanonicalJSON, now, now,
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
		_, err = txStore.writeConn().ExecContext(ctx,
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
	if len(filter.NodeIDs) > sqliteMaxVariables {
		var total int64
		chunkFilter := filter
		for _, chunk := range chunkIDs(filter.NodeIDs, sqliteMaxVariables) {
			chunkFilter.NodeIDs = chunk
			count, err := s.CountNodes(ctx, chunkFilter)
			if err != nil {
				return 0, err
			}
			total += count
		}
		return total, nil
	}
	query := "SELECT COUNT(*) FROM nodes"
	conditions, args := nodeFilterConditions(filter)

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	var count int64
	err := s.readConn().QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

// ===================== Subscriptions =====================

const subscriptionColumns = `id, name, url, format, user_agent, enabled, refresh_interval_seconds,
	refresh_timeout_seconds, sort_order, last_attempt, last_success, last_error,
	node_count, etag, last_modified, created_at, updated_at`

func (s *sqliteStore) ListSubscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := s.readConn().QueryContext(ctx, "SELECT "+subscriptionColumns+" FROM subscriptions ORDER BY sort_order, id")
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
	return scanSubscriptionRow(s.readConn().QueryRowContext(ctx, "SELECT "+subscriptionColumns+" FROM subscriptions WHERE id = ?", id))
}

func (s *sqliteStore) GetSubscriptionByURL(ctx context.Context, url string) (*Subscription, error) {
	return scanSubscriptionRow(s.readConn().QueryRowContext(ctx, "SELECT "+subscriptionColumns+" FROM subscriptions WHERE url = ?", url))
}

func (s *sqliteStore) CreateSubscription(ctx context.Context, subscription *Subscription) error {
	if subscription.Format == "" {
		subscription.Format = "auto"
	}
	now := time.Now().UTC()
	result, err := s.writeConn().ExecContext(ctx, `INSERT INTO subscriptions
		(name, url, format, user_agent, enabled, refresh_interval_seconds, refresh_timeout_seconds, sort_order,
		 last_attempt, last_success, last_error, node_count, etag, last_modified, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		subscription.Name, subscription.URL, subscription.Format, subscription.UserAgent, boolToInt(subscription.Enabled),
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
	if subscription.Format == "" {
		subscription.Format = "auto"
	}
	result, err := s.writeConn().ExecContext(ctx, `UPDATE subscriptions SET name=?, url=?, format=?, user_agent=?, enabled=?,
		refresh_interval_seconds=?, refresh_timeout_seconds=?, sort_order=?, last_attempt=?,
		last_success=?, last_error=?, node_count=?, etag=?, last_modified=?, updated_at=? WHERE id=?`,
		subscription.Name, subscription.URL, subscription.Format, subscription.UserAgent, boolToInt(subscription.Enabled), subscription.RefreshIntervalSeconds,
		subscription.RefreshTimeoutSeconds, subscription.SortOrder, formatTime(subscription.LastAttempt),
		formatTime(subscription.LastSuccess), subscription.LastError, subscription.NodeCount, subscription.ETag,
		subscription.LastModified, formatTime(time.Now().UTC()), subscription.ID)
	if err != nil {
		return fmt.Errorf("update subscription %d: %w", subscription.ID, err)
	}
	return requireAffected(result, fmt.Sprintf("subscription %d not found", subscription.ID))
}

func (s *sqliteStore) DeleteSubscription(ctx context.Context, id int64) error {
	return s.runInTx(ctx, func(tx *sqliteStore) error {
		var exists int
		if err := tx.writeConn().QueryRowContext(ctx, "SELECT 1 FROM subscriptions WHERE id=?", id).Scan(&exists); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("subscription %d not found", id)
			}
			return err
		}
		now := formatTime(time.Now().UTC())
		if _, err := tx.writeConn().ExecContext(ctx, `UPDATE nodes SET source=?, updated_at=?
			WHERE source=?
			AND id IN (SELECT node_id FROM subscription_nodes WHERE subscription_id=?)
			AND NOT EXISTS (
				SELECT 1 FROM subscription_nodes other
				WHERE other.node_id=nodes.id AND other.subscription_id<>?
			)`, NodeSourceManual, now, NodeSourceSubscription, id, id); err != nil {
			return fmt.Errorf("preserve nodes for subscription %d: %w", id, err)
		}
		result, err := tx.writeConn().ExecContext(ctx, "DELETE FROM subscriptions WHERE id=?", id)
		if err != nil {
			return fmt.Errorf("delete subscription %d: %w", id, err)
		}
		return requireAffected(result, fmt.Sprintf("subscription %d not found", id))
	})
}

func (s *sqliteStore) AdoptOrphanSubscriptionNodes(ctx context.Context) (int64, error) {
	result, err := s.writeConn().ExecContext(ctx, `UPDATE nodes SET source=?, updated_at=?
		WHERE source=? AND NOT EXISTS (
			SELECT 1 FROM subscription_nodes memberships WHERE memberships.node_id=nodes.id
		)`, NodeSourceManual, formatTime(time.Now().UTC()), NodeSourceSubscription)
	if err != nil {
		return 0, fmt.Errorf("adopt orphan subscription nodes: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count adopted orphan subscription nodes: %w", err)
	}
	return count, nil
}

func (s *sqliteStore) SetSubscriptionEnabled(ctx context.Context, id int64, enabled bool) error {
	result, err := s.writeConn().ExecContext(ctx, "UPDATE subscriptions SET enabled=?, updated_at=? WHERE id=?",
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
	_, err := s.writeConn().ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update subscription refresh settings: %w", err)
	}
	return nil
}

func (s *sqliteStore) ActivateSubscriptionExclusive(ctx context.Context, id int64) error {
	return s.runInTx(ctx, func(tx *sqliteStore) error {
		var exists int
		if err := tx.writeConn().QueryRowContext(ctx, "SELECT 1 FROM subscriptions WHERE id=?", id).Scan(&exists); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("subscription %d not found", id)
			}
			return err
		}
		_, err := tx.writeConn().ExecContext(ctx, `UPDATE subscriptions SET enabled=CASE WHEN id=? THEN 1 ELSE 0 END,
			updated_at=? WHERE enabled != CASE WHEN id=? THEN 1 ELSE 0 END`, id, formatTime(time.Now().UTC()), id)
		return err
	})
}

func (s *sqliteStore) ListSubscriptionNodes(ctx context.Context, subscriptionID int64) ([]SubscriptionNode, error) {
	rows, err := s.readConn().QueryContext(ctx, `SELECT sn.subscription_id, sn.position,
		n.id, n.uri, n.name, n.source, n.port, n.username, n.password, n.region, n.country,
		n.enabled, n.tags, n.identity_hash, n.canonical_json, n.created_at, n.updated_at
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
			&member.Node.Password, &member.Node.Region, &member.Node.Country, &enabled, &tagsJSON,
			&member.Node.IdentityHash, &member.Node.CanonicalJSON, &createdAt, &updatedAt); err != nil {
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
	rows, err := s.readConn().QueryContext(ctx, `SELECT DISTINCT n.id, n.uri, n.name, n.source, n.port,
		n.username, n.password, n.region, n.country, n.enabled, n.tags, n.identity_hash, n.canonical_json, n.created_at, n.updated_at
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
		if err := tx.mergeSubscriptionNodes(ctx, subscriptionID, nodes); err != nil {
			return err
		}
		var nodeCount int
		if err := tx.writeConn().QueryRowContext(ctx,
			"SELECT COUNT(*) FROM subscription_nodes WHERE subscription_id=?", subscriptionID).Scan(&nodeCount); err != nil {
			return fmt.Errorf("count subscription %d nodes: %w", subscriptionID, err)
		}
		result, err := tx.writeConn().ExecContext(ctx, `UPDATE subscriptions SET last_attempt=?, last_success=?,
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
	if err := s.requireSubscription(ctx, subscriptionID); err != nil {
		return err
	}
	if _, err := s.writeConn().ExecContext(ctx, "DELETE FROM subscription_nodes WHERE subscription_id=?", subscriptionID); err != nil {
		return fmt.Errorf("clear subscription %d nodes: %w", subscriptionID, err)
	}
	return s.upsertSubscriptionNodes(ctx, subscriptionID, nodes)
}

func (s *sqliteStore) mergeSubscriptionNodes(ctx context.Context, subscriptionID int64, nodes []SubscriptionNodeInput) error {
	if err := s.requireSubscription(ctx, subscriptionID); err != nil {
		return err
	}
	if len(nodes) == 0 {
		return errors.New("subscription snapshot contains no nodes")
	}
	for index := range nodes {
		prepareSubscriptionNodeIdentity(&nodes[index])
	}
	if err := s.reconcileSubscriptionLogicalNodes(ctx, subscriptionID, nodes); err != nil {
		return err
	}
	// Move retained members behind the current snapshot. Members present in the
	// new snapshot are upserted back to positions starting at zero below.
	if _, err := s.writeConn().ExecContext(ctx, `UPDATE subscription_nodes SET position=position+?
		WHERE subscription_id=?`, len(nodes), subscriptionID); err != nil {
		return fmt.Errorf("reorder retained subscription %d nodes: %w", subscriptionID, err)
	}
	return s.upsertSubscriptionNodes(ctx, subscriptionID, nodes)
}

type subscriptionLogicalCandidate struct {
	id              int64
	name            string
	identityHash    string
	source          string
	membershipCount int
}

// reconcileSubscriptionLogicalNodes treats a unique provider-supplied name as
// a stable logical slot inside one subscription. Providers frequently rotate
// the endpoint or credentials without changing that name; without this pass,
// the new semantic identity would be appended forever while the old listener
// and group references remained active.
//
// Reconciliation is deliberately conservative: a name must occur exactly once
// in the incoming snapshot, and only subscription-owned nodes exclusive to this
// subscription may be rewritten or merged. Legitimate same-name endpoints and
// nodes shared by multiple subscriptions are left untouched.
func (s *sqliteStore) reconcileSubscriptionLogicalNodes(ctx context.Context, subscriptionID int64, nodes []SubscriptionNodeInput) error {
	if s.tx == nil {
		return errors.New("logical subscription reconciliation requires a transaction")
	}
	incomingCounts := make(map[string]int, len(nodes))
	incomingByName := make(map[string]SubscriptionNodeInput, len(nodes))
	for _, node := range nodes {
		logicalName := normalizeSubscriptionLogicalName(node.LogicalName)
		if logicalName == "" {
			continue
		}
		incomingCounts[logicalName]++
		incomingByName[logicalName] = node
	}

	rows, err := s.writeConn().QueryContext(ctx, `SELECT n.id,n.name,n.identity_hash,n.source,
		(SELECT COUNT(*) FROM subscription_nodes memberships WHERE memberships.node_id=n.id)
		FROM subscription_nodes current_membership
		JOIN nodes n ON n.id=current_membership.node_id
		WHERE current_membership.subscription_id=? ORDER BY n.id`, subscriptionID)
	if err != nil {
		return fmt.Errorf("list logical nodes for subscription %d: %w", subscriptionID, err)
	}
	candidatesByName := make(map[string][]subscriptionLogicalCandidate)
	for rows.Next() {
		var candidate subscriptionLogicalCandidate
		if err := rows.Scan(&candidate.id, &candidate.name, &candidate.identityHash, &candidate.source, &candidate.membershipCount); err != nil {
			rows.Close()
			return fmt.Errorf("scan logical node for subscription %d: %w", subscriptionID, err)
		}
		logicalName := normalizeSubscriptionLogicalName(candidate.name)
		if logicalName != "" {
			candidatesByName[logicalName] = append(candidatesByName[logicalName], candidate)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for logicalName, input := range incomingByName {
		if incomingCounts[logicalName] != 1 {
			continue
		}
		eligible := make([]subscriptionLogicalCandidate, 0, len(candidatesByName[logicalName]))
		for _, candidate := range candidatesByName[logicalName] {
			if candidate.source == NodeSourceSubscription && candidate.membershipCount == 1 {
				eligible = append(eligible, candidate)
			}
		}
		if len(eligible) == 0 {
			continue
		}

		var targetID int64
		err := s.writeConn().QueryRowContext(ctx, "SELECT id FROM nodes WHERE identity_hash=?", input.IdentityHash).Scan(&targetID)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("resolve current identity for %q: %w", input.Name, err)
		}
		targetIsEligible := false
		for _, candidate := range eligible {
			if candidate.id == targetID {
				targetIsEligible = true
				break
			}
		}
		if targetID == 0 || targetIsEligible {
			// Reuse the oldest exclusive node so its port, admin state,
			// statistics and group references stay attached to a stable ID. If
			// a later duplicate already owns the incoming identity, merge it
			// first; deleting it releases the unique hash before the update.
			targetID = eligible[0].id
			previousIdentity := eligible[0].identityHash
			for _, candidate := range eligible[1:] {
				if err := mergeNodeReferences(s.tx, targetID, candidate.id); err != nil {
					return fmt.Errorf("merge stale logical node %d into %d: %w", candidate.id, targetID, err)
				}
			}
			if _, err := s.writeConn().ExecContext(ctx, `UPDATE nodes SET uri=?,name=?,username=?,password=?,
				region=CASE WHEN ?<>'' THEN ? ELSE region END,
				country=CASE WHEN ?<>'' THEN ? ELSE country END,
				identity_hash=?,canonical_json=?,updated_at=? WHERE id=?`,
				input.URI, input.Name, input.Username, input.Password,
				input.Region, input.Region, input.Country, input.Country,
				input.IdentityHash, input.CanonicalJSON, formatTime(time.Now().UTC()), targetID); err != nil {
				return fmt.Errorf("update logical subscription node %d: %w", targetID, err)
			}
			if previousIdentity != "" && previousIdentity != input.IdentityHash {
				if err := s.clearNodeDetectionCache(ctx, targetID); err != nil {
					return err
				}
			}
			continue
		}

		// The current identity already exists (including legacy accumulated
		// duplicates). Merge every stale exclusive copy into it so explicit
		// group membership, current selection and statistics follow the winner.
		for _, candidate := range eligible {
			if candidate.id == targetID {
				continue
			}
			if err := mergeNodeReferences(s.tx, targetID, candidate.id); err != nil {
				return fmt.Errorf("merge stale logical node %d into %d: %w", candidate.id, targetID, err)
			}
		}
	}
	return nil
}

func normalizeSubscriptionLogicalName(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func prepareSubscriptionNodeIdentity(node *SubscriptionNodeInput) {
	if node.IdentityHash != "" {
		return
	}
	identity, identityErr := nodecodec.ParseURI(node.URI)
	if identityErr != nil {
		node.IdentityHash = nodecodec.FallbackHash(node.URI)
		return
	}
	node.IdentityHash, node.CanonicalJSON = identity.Hash, identity.CanonicalJSON
}

func (s *sqliteStore) requireSubscription(ctx context.Context, subscriptionID int64) error {
	var exists int
	if err := s.writeConn().QueryRowContext(ctx, "SELECT 1 FROM subscriptions WHERE id=?", subscriptionID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("subscription %d not found", subscriptionID)
		}
		return err
	}
	return nil
}

func (s *sqliteStore) upsertSubscriptionNodes(ctx context.Context, subscriptionID int64, nodes []SubscriptionNodeInput) error {
	now := formatTime(time.Now().UTC())
	for position, node := range nodes {
		prepareSubscriptionNodeIdentity(&node)
		_, err := s.writeConn().ExecContext(ctx, `INSERT INTO nodes
			(uri, name, source, port, username, password, region, country, enabled, identity_hash, canonical_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(identity_hash) WHERE identity_hash<>'' DO UPDATE SET
			name=excluded.name,
			port=CASE WHEN nodes.port=0 THEN excluded.port ELSE nodes.port END,
			username=excluded.username, password=excluded.password,
			region=CASE WHEN excluded.region<>'' THEN excluded.region ELSE nodes.region END,
			country=CASE WHEN excluded.country<>'' THEN excluded.country ELSE nodes.country END,
			updated_at=excluded.updated_at`, node.URI, node.Name,
			NodeSourceSubscription, node.Port, node.Username, node.Password, node.Region, node.Country,
			boolToInt(node.Enabled), node.IdentityHash, node.CanonicalJSON, now, now)
		if err != nil {
			return fmt.Errorf("upsert subscription node %q: %w", node.URI, err)
		}
		if _, err := s.writeConn().ExecContext(ctx, `INSERT INTO subscription_nodes (subscription_id, node_id, position)
			SELECT ?, id, ? FROM nodes WHERE identity_hash=?
			ON CONFLICT(subscription_id, node_id) DO UPDATE SET position=excluded.position`,
			subscriptionID, position, node.IdentityHash); err != nil {
			return fmt.Errorf("link subscription node %q: %w", node.URI, err)
		}
	}
	if _, err := s.writeConn().ExecContext(ctx, "INSERT OR IGNORE INTO node_stats (node_id) SELECT id FROM nodes"); err != nil {
		return fmt.Errorf("create subscription node stats: %w", err)
	}
	return nil
}

// ===================== Node stats =====================

func (s *sqliteStore) GetNodeStats(ctx context.Context, nodeID int64) (*NodeStats, error) {
	row := s.readConn().QueryRowContext(ctx,
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

	_, err := s.writeConn().ExecContext(ctx,
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
	_, err := s.writeConn().ExecContext(ctx,
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
	_, err := s.writeConn().ExecContext(ctx,
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
	_, err := s.writeConn().ExecContext(ctx,
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
	_, err := s.writeConn().ExecContext(ctx,
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
	_, err := s.writeConn().ExecContext(ctx,
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
		stmt, err := txStore.writeConn().PrepareContext(ctx,
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
	return s.ListNodeStats(ctx, nil)
}

// ListNodeStats returns stats for the given nodes, or every node when nodeIDs
// is nil.
func (s *sqliteStore) ListNodeStats(ctx context.Context, nodeIDs []int64) (map[int64]*NodeStats, error) {
	return loadByNodeIDs(ctx, nodeIDs, s.listNodeStatsChunk)
}

func (s *sqliteStore) listNodeStatsChunk(ctx context.Context, nodeIDs []int64) (map[int64]*NodeStats, error) {
	where, args := nodeIDCondition("node_id", nodeIDs)
	rows, err := s.readConn().QueryContext(ctx,
		`SELECT node_id, failure_count, success_count, blacklisted, blacklisted_until,
		 last_error, last_failure_at, last_success_at, last_latency_ms,
		 available, initial_check_done, total_upload_bytes, total_download_bytes, updated_at
		 FROM node_stats`+where, args...)
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
	_, err := s.writeConn().ExecContext(ctx,
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

	rows, err := s.readConn().QueryContext(ctx,
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

	_, err := s.writeConn().ExecContext(ctx,
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
	_, err := s.writeConn().ExecContext(ctx,
		`INSERT INTO sessions (token, created_at, expires_at) VALUES (?, ?, ?)`,
		session.Token,
		session.CreatedAt.UTC().Format(time.RFC3339),
		session.ExpiresAt.UTC().Format(time.RFC3339),
	)
	return err
}

func (s *sqliteStore) GetSession(ctx context.Context, token string) (*Session, error) {
	row := s.readConn().QueryRowContext(ctx,
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
	_, err := s.writeConn().ExecContext(ctx, "DELETE FROM sessions WHERE token = ?", token)
	return err
}

func (s *sqliteStore) CleanupExpiredSessions(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.writeConn().ExecContext(ctx, "DELETE FROM sessions WHERE expires_at < ?", now)
	return err
}

// ===================== Subscription status =====================

func (s *sqliteStore) GetSubscriptionStatus(ctx context.Context) (*SubscriptionStatus, error) {
	row := s.readConn().QueryRowContext(ctx,
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

	_, err := s.writeConn().ExecContext(ctx,
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

	_, err := s.writeConn().ExecContext(ctx,
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
	row := s.readConn().QueryRowContext(ctx,
		"SELECT "+unlockColumns+" FROM node_unlock_results WHERE node_id = ?", nodeID)
	return scanUnlockResult(row)
}

func (s *sqliteStore) ListUnlockResults(ctx context.Context) (map[int64]*UnlockResult, error) {
	return s.ListUnlockResultsByIDs(ctx, nil)
}

// ListUnlockResultsByIDs returns unlock results for the given nodes, or every
// node when nodeIDs is nil.
func (s *sqliteStore) ListUnlockResultsByIDs(ctx context.Context, nodeIDs []int64) (map[int64]*UnlockResult, error) {
	return loadByNodeIDs(ctx, nodeIDs, s.listUnlockResultsChunk)
}

func (s *sqliteStore) listUnlockResultsChunk(ctx context.Context, nodeIDs []int64) (map[int64]*UnlockResult, error) {
	where, args := nodeIDCondition("node_id", nodeIDs)
	rows, err := s.readConn().QueryContext(ctx, "SELECT "+unlockColumns+" FROM node_unlock_results"+where, args...)
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

// ===================== Manual node diagnostics =====================

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableBool(value *bool) any {
	if value == nil {
		return nil
	}
	return boolToInt(*value)
}

func (s *sqliteStore) UpsertNodeDetectionResult(ctx context.Context, result *NodeDetectionResult) error {
	if result == nil {
		return errors.New("upsert node detection result: nil result")
	}
	now := time.Now().UTC()
	_, err := s.writeConn().ExecContext(ctx, `INSERT INTO node_detection_results (
		node_id,task_id,latency_status,latency_ms,latency_error,latency_checked_at,
		speed_status,average_bytes_per_second,peak_bytes_per_second,bytes_downloaded,
		speed_duration_ms,speed_error,speed_checked_at,exit_ip,exit_ip_family,exit_country,exit_country_code,
		exit_ip_status,exit_ip_error,exit_ip_checked_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET
		task_id=excluded.task_id,
		latency_status=CASE WHEN excluded.latency_status='untested' THEN node_detection_results.latency_status ELSE excluded.latency_status END,
		latency_ms=CASE WHEN excluded.latency_status='untested' THEN node_detection_results.latency_ms ELSE excluded.latency_ms END,
		latency_error=CASE WHEN excluded.latency_status='untested' THEN node_detection_results.latency_error ELSE excluded.latency_error END,
		latency_checked_at=CASE WHEN excluded.latency_status='untested' THEN node_detection_results.latency_checked_at ELSE excluded.latency_checked_at END,
		speed_status=CASE WHEN excluded.speed_status='untested' THEN node_detection_results.speed_status ELSE excluded.speed_status END,
		average_bytes_per_second=CASE WHEN excluded.speed_status='untested' THEN node_detection_results.average_bytes_per_second ELSE excluded.average_bytes_per_second END,
		peak_bytes_per_second=CASE WHEN excluded.speed_status='untested' THEN node_detection_results.peak_bytes_per_second ELSE excluded.peak_bytes_per_second END,
		bytes_downloaded=CASE WHEN excluded.speed_status='untested' THEN node_detection_results.bytes_downloaded ELSE excluded.bytes_downloaded END,
		speed_duration_ms=CASE WHEN excluded.speed_status='untested' THEN node_detection_results.speed_duration_ms ELSE excluded.speed_duration_ms END,
		speed_error=CASE WHEN excluded.speed_status='untested' THEN node_detection_results.speed_error ELSE excluded.speed_error END,
		speed_checked_at=CASE WHEN excluded.speed_status='untested' THEN node_detection_results.speed_checked_at ELSE excluded.speed_checked_at END,
		exit_ip=CASE WHEN excluded.exit_ip_status='untested' THEN node_detection_results.exit_ip ELSE excluded.exit_ip END,
		exit_ip_family=CASE WHEN excluded.exit_ip_status='untested' THEN node_detection_results.exit_ip_family ELSE excluded.exit_ip_family END,
		exit_country=CASE WHEN excluded.exit_ip_status='untested' THEN node_detection_results.exit_country ELSE excluded.exit_country END,
		exit_country_code=CASE WHEN excluded.exit_ip_status='untested' THEN node_detection_results.exit_country_code ELSE excluded.exit_country_code END,
		exit_ip_status=CASE WHEN excluded.exit_ip_status='untested' THEN node_detection_results.exit_ip_status ELSE excluded.exit_ip_status END,
		exit_ip_error=CASE WHEN excluded.exit_ip_status='untested' THEN node_detection_results.exit_ip_error ELSE excluded.exit_ip_error END,
		exit_ip_checked_at=CASE WHEN excluded.exit_ip_status='untested' THEN node_detection_results.exit_ip_checked_at ELSE excluded.exit_ip_checked_at END,
		updated_at=excluded.updated_at`,
		result.NodeID, result.TaskID, result.LatencyStatus, nullableInt64(result.LatencyMs), result.LatencyError, formatTime(result.LatencyCheckedAt),
		result.SpeedStatus, nullableInt64(result.AverageBytesPerSecond), nullableInt64(result.PeakBytesPerSecond), result.BytesDownloaded,
		result.SpeedDurationMs, result.SpeedError, formatTime(result.SpeedCheckedAt), result.ExitIP, result.ExitIPFamily, result.ExitCountry, result.ExitCountryCode,
		result.ExitIPStatus, result.ExitIPError, formatTime(result.ExitIPCheckedAt), formatTime(now))
	if err != nil {
		return fmt.Errorf("upsert node detection result %d: %w", result.NodeID, err)
	}
	return nil
}

func (s *sqliteStore) ListNodeDetectionResults(ctx context.Context) (map[int64]*NodeDetectionResult, error) {
	return s.ListNodeDetectionResultsByIDs(ctx, nil)
}

// ListNodeDetectionResultsByIDs returns detection results for the given nodes,
// or every node when nodeIDs is nil.
func (s *sqliteStore) ListNodeDetectionResultsByIDs(ctx context.Context, nodeIDs []int64) (map[int64]*NodeDetectionResult, error) {
	return loadByNodeIDs(ctx, nodeIDs, s.listNodeDetectionResultsChunk)
}

func (s *sqliteStore) listNodeDetectionResultsChunk(ctx context.Context, nodeIDs []int64) (map[int64]*NodeDetectionResult, error) {
	where, args := nodeIDCondition("node_id", nodeIDs)
	rows, err := s.readConn().QueryContext(ctx, `SELECT node_id,task_id,latency_status,latency_ms,latency_error,latency_checked_at,
		speed_status,average_bytes_per_second,peak_bytes_per_second,bytes_downloaded,speed_duration_ms,speed_error,speed_checked_at,
		exit_ip,exit_ip_family,exit_country,exit_country_code,exit_ip_status,exit_ip_error,exit_ip_checked_at,updated_at
		FROM node_detection_results`+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]*NodeDetectionResult)
	for rows.Next() {
		var item NodeDetectionResult
		var latency, average, peak sql.NullInt64
		var latencyAt, speedAt, exitAt, updatedAt string
		if err := rows.Scan(&item.NodeID, &item.TaskID, &item.LatencyStatus, &latency, &item.LatencyError, &latencyAt,
			&item.SpeedStatus, &average, &peak, &item.BytesDownloaded, &item.SpeedDurationMs, &item.SpeedError, &speedAt,
			&item.ExitIP, &item.ExitIPFamily, &item.ExitCountry, &item.ExitCountryCode, &item.ExitIPStatus, &item.ExitIPError, &exitAt, &updatedAt); err != nil {
			return nil, err
		}
		if latency.Valid {
			value := latency.Int64
			item.LatencyMs = &value
		}
		if average.Valid {
			value := average.Int64
			item.AverageBytesPerSecond = &value
		}
		if peak.Valid {
			value := peak.Int64
			item.PeakBytesPerSecond = &value
		}
		item.LatencyCheckedAt, item.SpeedCheckedAt = parseTime(latencyAt), parseTime(speedAt)
		item.ExitIPCheckedAt, item.UpdatedAt = parseTime(exitAt), parseTime(updatedAt)
		copyItem := item
		result[item.NodeID] = &copyItem
	}
	return result, rows.Err()
}

func (s *sqliteStore) UpsertNodeIPQualityResult(ctx context.Context, result *NodeIPQualityResult) error {
	if result == nil {
		return errors.New("upsert node IP quality result: nil result")
	}
	now := time.Now().UTC()
	_, err := s.writeConn().ExecContext(ctx, `INSERT INTO node_ip_quality_results (
		node_id,provider,task_id,status,ip,family,country,country_code,asn,org,isp,is_broadcast,is_residential,
		fraud_score,proxy,hosting,mobile,reason,checked_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(node_id,provider) DO UPDATE SET task_id=excluded.task_id,status=excluded.status,ip=excluded.ip,
		family=excluded.family,country=excluded.country,country_code=excluded.country_code,asn=excluded.asn,org=excluded.org,
		isp=excluded.isp,is_broadcast=excluded.is_broadcast,is_residential=excluded.is_residential,fraud_score=excluded.fraud_score,
		proxy=excluded.proxy,hosting=excluded.hosting,mobile=excluded.mobile,reason=excluded.reason,
		checked_at=excluded.checked_at,updated_at=excluded.updated_at`,
		result.NodeID, result.Provider, result.TaskID, result.Status, result.IP, result.Family, result.Country, result.CountryCode,
		result.ASN, result.Org, result.ISP, nullableBool(result.IsBroadcast), nullableBool(result.IsResidential), nullableInt(result.FraudScore),
		nullableBool(result.Proxy), nullableBool(result.Hosting), nullableBool(result.Mobile), result.Reason, formatTime(result.CheckedAt), formatTime(now))
	return err
}

func (s *sqliteStore) ListNodeIPQualityResults(ctx context.Context) (map[int64][]NodeIPQualityResult, error) {
	return s.ListNodeIPQualityResultsByIDs(ctx, nil)
}

// ListNodeIPQualityResultsByIDs returns IP quality results for the given nodes,
// or every node when nodeIDs is nil.
func (s *sqliteStore) ListNodeIPQualityResultsByIDs(ctx context.Context, nodeIDs []int64) (map[int64][]NodeIPQualityResult, error) {
	return loadByNodeIDs(ctx, nodeIDs, s.listNodeIPQualityResultsChunk)
}

func (s *sqliteStore) listNodeIPQualityResultsChunk(ctx context.Context, nodeIDs []int64) (map[int64][]NodeIPQualityResult, error) {
	where, args := nodeIDCondition("node_id", nodeIDs)
	rows, err := s.readConn().QueryContext(ctx, `SELECT node_id,provider,task_id,status,ip,family,country,country_code,asn,org,isp,
		is_broadcast,is_residential,fraud_score,proxy,hosting,mobile,reason,checked_at,updated_at
		FROM node_ip_quality_results`+where+` ORDER BY node_id,provider`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64][]NodeIPQualityResult)
	for rows.Next() {
		var item NodeIPQualityResult
		var broadcast, residential, fraud, proxy, hosting, mobile sql.NullInt64
		var checkedAt, updatedAt string
		if err := rows.Scan(&item.NodeID, &item.Provider, &item.TaskID, &item.Status, &item.IP, &item.Family, &item.Country, &item.CountryCode,
			&item.ASN, &item.Org, &item.ISP, &broadcast, &residential, &fraud, &proxy, &hosting, &mobile, &item.Reason, &checkedAt, &updatedAt); err != nil {
			return nil, err
		}
		item.IsBroadcast, item.IsResidential = nullBoolPointer(broadcast), nullBoolPointer(residential)
		item.Proxy, item.Hosting, item.Mobile = nullBoolPointer(proxy), nullBoolPointer(hosting), nullBoolPointer(mobile)
		if fraud.Valid {
			value := int(fraud.Int64)
			item.FraudScore = &value
		}
		item.CheckedAt, item.UpdatedAt = parseTime(checkedAt), parseTime(updatedAt)
		out[item.NodeID] = append(out[item.NodeID], item)
	}
	return out, rows.Err()
}

func nullBoolPointer(value sql.NullInt64) *bool {
	if !value.Valid {
		return nil
	}
	result := value.Int64 != 0
	return &result
}

func (s *sqliteStore) UpsertNodeDetectionTask(ctx context.Context, task *NodeDetectionTask) error {
	if task == nil {
		return errors.New("upsert node detection task: nil task")
	}
	now := time.Now().UTC()
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	_, err := s.writeConn().ExecContext(ctx, `INSERT INTO node_detection_tasks
		(id,status,stages_json,settings_json,stats_json,total_nodes,completed_nodes,downloaded_bytes,error,created_at,started_at,finished_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,stages_json=excluded.stages_json,
		settings_json=excluded.settings_json,stats_json=excluded.stats_json,total_nodes=excluded.total_nodes,
		completed_nodes=excluded.completed_nodes,downloaded_bytes=excluded.downloaded_bytes,error=excluded.error,
		started_at=excluded.started_at,finished_at=excluded.finished_at,updated_at=excluded.updated_at`,
		task.ID, task.Status, task.StagesJSON, task.SettingsJSON, task.StatsJSON, task.TotalNodes, task.CompletedNodes, task.DownloadedBytes,
		task.Error, formatTime(task.CreatedAt), formatTime(task.StartedAt), formatTime(task.FinishedAt), formatTime(now))
	return err
}

func (s *sqliteStore) ListNodeDetectionTasks(ctx context.Context, limit int) ([]NodeDetectionTask, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.readConn().QueryContext(ctx, `SELECT id,status,stages_json,settings_json,stats_json,total_nodes,completed_nodes,
		downloaded_bytes,error,created_at,started_at,finished_at,updated_at FROM node_detection_tasks ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeDetectionTask
	for rows.Next() {
		var item NodeDetectionTask
		var createdAt, startedAt, finishedAt, updatedAt string
		if err := rows.Scan(&item.ID, &item.Status, &item.StagesJSON, &item.SettingsJSON, &item.StatsJSON, &item.TotalNodes, &item.CompletedNodes,
			&item.DownloadedBytes, &item.Error, &createdAt, &startedAt, &finishedAt, &updatedAt); err != nil {
			return nil, err
		}
		item.CreatedAt, item.StartedAt, item.FinishedAt, item.UpdatedAt = parseTime(createdAt), parseTime(startedAt), parseTime(finishedAt), parseTime(updatedAt)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *sqliteStore) InterruptRunningNodeDetectionTasks(ctx context.Context) error {
	now := formatTime(time.Now().UTC())
	_, err := s.writeConn().ExecContext(ctx, `UPDATE node_detection_tasks SET status='interrupted',error='服务重启，检测任务已中断',finished_at=?,updated_at=? WHERE status IN ('pending','running')`, now, now)
	return err
}

func (s *sqliteStore) PruneNodeDetectionTasks(ctx context.Context, keep int) error {
	if keep < 1 {
		keep = 20
	}
	_, err := s.writeConn().ExecContext(ctx, `DELETE FROM node_detection_tasks WHERE id NOT IN (SELECT id FROM node_detection_tasks ORDER BY created_at DESC LIMIT ?)`, keep)
	return err
}

// ===================== Group pool operations =====================

const groupPoolColumns = `id, name, bind_address, bind_port, protocol, username, password,
dispatch_mode, regions_json, explicit_node_ids_json, excluded_node_ids_json,
tag_whitelist_json, tag_blacklist_json, tag_filter_match, failure_window_seconds,
failure_threshold, health_check_seconds, current_active_node_id, enabled,
subscription_enabled, subscription_token, subscription_mode, external_host, created_at, updated_at`

func (s *sqliteStore) ListGroupPools(ctx context.Context) ([]GroupPool, error) {
	rows, err := s.readConn().QueryContext(ctx, "SELECT "+groupPoolColumns+" FROM group_pools ORDER BY id")
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
	g, err := scanGroupPool(s.readConn().QueryRowContext(ctx, "SELECT "+groupPoolColumns+" FROM group_pools WHERE id = ?", id))
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
	tagWhitelist, tagBlacklist, tagFilterMatch := marshalGroupTagFilter(g)
	result, err := s.writeConn().ExecContext(ctx, `INSERT INTO group_pools
(name, bind_address, bind_port, protocol, username, password, dispatch_mode, regions_json,
 explicit_node_ids_json, excluded_node_ids_json, tag_whitelist_json, tag_blacklist_json, tag_filter_match,
 failure_window_seconds, failure_threshold, health_check_seconds,
 current_active_node_id, enabled, subscription_enabled, subscription_token, subscription_mode,
 external_host, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.Name, g.BindAddress, g.BindPort, g.Protocol, g.Username, g.Password, g.DispatchMode,
		string(regions), string(nodeIDs), string(excludedNodeIDs), tagWhitelist, tagBlacklist, tagFilterMatch,
		g.FailureWindowSeconds, g.FailureThreshold,
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
	tagWhitelist, tagBlacklist, tagFilterMatch := marshalGroupTagFilter(g)
	result, err := s.writeConn().ExecContext(ctx, `UPDATE group_pools SET
name=?, bind_address=?, bind_port=?, protocol=?, username=?, password=?, dispatch_mode=?,
regions_json=?, explicit_node_ids_json=?, excluded_node_ids_json=?,
tag_whitelist_json=?, tag_blacklist_json=?, tag_filter_match=?, failure_window_seconds=?, failure_threshold=?,
health_check_seconds=?, current_active_node_id=?, enabled=?, subscription_enabled=?, subscription_token=?,
subscription_mode=?, external_host=?, updated_at=? WHERE id=?`,
		g.Name, g.BindAddress, g.BindPort, g.Protocol, g.Username, g.Password, g.DispatchMode,
		string(regions), string(nodeIDs), string(excludedNodeIDs), tagWhitelist, tagBlacklist, tagFilterMatch,
		g.FailureWindowSeconds, g.FailureThreshold,
		g.HealthCheckSeconds, g.CurrentActiveNodeID, boolToInt(g.Enabled), boolToInt(g.SubscriptionEnabled),
		g.SubscriptionToken, g.SubscriptionMode, g.ExternalHost, formatTime(time.Now()), g.ID)
	if err != nil {
		return fmt.Errorf("update group pool: %w", err)
	}
	return requireAffected(result, "group pool not found")
}

// marshalGroupTagFilter serializes the tag filter columns, normalizing the
// match mode so a zero-valued GroupPool stores the schema default.
func marshalGroupTagFilter(g *GroupPool) (whitelist, blacklist, match string) {
	whitelistJSON, _ := json.Marshal(normalizeIDList(g.TagWhitelist))
	blacklistJSON, _ := json.Marshal(normalizeIDList(g.TagBlacklist))
	match = g.TagFilterMatch
	if match != TagFilterMatchAll {
		match = TagFilterMatchAny
	}
	return string(whitelistJSON), string(blacklistJSON), match
}

// normalizeIDList drops non-positive IDs and duplicates while preserving order,
// and never returns nil so the JSON column stays a valid array.
func normalizeIDList(ids []int64) []int64 {
	normalized := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	return normalized
}

func (s *sqliteStore) UpdateGroupCurrentActiveNode(ctx context.Context, groupID, nodeID int64) error {
	result, err := s.writeConn().ExecContext(ctx,
		"UPDATE group_pools SET current_active_node_id=?, updated_at=? WHERE id=?",
		nodeID, formatTime(time.Now()), groupID)
	if err != nil {
		return fmt.Errorf("update group current active node: %w", err)
	}
	return requireAffected(result, "group pool not found")
}

func (s *sqliteStore) DeleteGroupPool(ctx context.Context, id int64) error {
	result, err := s.writeConn().ExecContext(ctx, "DELETE FROM group_pools WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete group pool: %w", err)
	}
	return requireAffected(result, "group pool not found")
}

func (s *sqliteStore) UpsertGroupNodeState(ctx context.Context, state *GroupNodeState) error {
	history, _ := json.Marshal(state.FailureHistory)
	_, err := s.writeConn().ExecContext(ctx, `INSERT INTO group_node_states
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
	_, err := s.writeConn().ExecContext(ctx, "DELETE FROM group_node_states WHERE group_id = ? AND node_id = ?", groupID, nodeID)
	return err
}

func (s *sqliteStore) listGroupNodeStates(ctx context.Context, groupID int64) ([]GroupNodeState, error) {
	rows, err := s.readConn().QueryContext(ctx, `SELECT group_id, node_id, failure_history_json, evicted,
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
	var regions, nodeIDs, excludedNodeIDs, tagWhitelist, tagBlacklist, createdAt, updatedAt string
	var enabled, subscriptionEnabled int
	err := row.Scan(&g.ID, &g.Name, &g.BindAddress, &g.BindPort, &g.Protocol, &g.Username, &g.Password,
		&g.DispatchMode, &regions, &nodeIDs, &excludedNodeIDs, &tagWhitelist, &tagBlacklist, &g.TagFilterMatch,
		&g.FailureWindowSeconds, &g.FailureThreshold,
		&g.HealthCheckSeconds, &g.CurrentActiveNodeID, &enabled, &subscriptionEnabled,
		&g.SubscriptionToken, &g.SubscriptionMode, &g.ExternalHost, &createdAt, &updatedAt)
	if err != nil {
		return g, err
	}
	_ = json.Unmarshal([]byte(regions), &g.Regions)
	_ = json.Unmarshal([]byte(nodeIDs), &g.ExplicitNodeIDs)
	_ = json.Unmarshal([]byte(excludedNodeIDs), &g.ExcludedNodeIDs)
	_ = json.Unmarshal([]byte(tagWhitelist), &g.TagWhitelist)
	_ = json.Unmarshal([]byte(tagBlacklist), &g.TagBlacklist)
	if g.TagFilterMatch != TagFilterMatchAll {
		g.TagFilterMatch = TagFilterMatchAny
	}
	g.Enabled = enabled != 0
	g.SubscriptionEnabled = subscriptionEnabled != 0
	g.CreatedAt, g.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	return g, nil
}

// ===================== Lifecycle =====================

func (s *sqliteStore) Close() error {
	var readerErr, writerErr error
	if s.readerDB != nil {
		readerErr = s.readerDB.Close()
	}
	if s.writerDB != nil {
		writerErr = s.writerDB.Close()
	}
	return errors.Join(readerErr, writerErr)
}

func (s *sqliteStore) WithTx(ctx context.Context, fn func(tx Store) error) error {
	if s.tx != nil {
		// Already in a transaction, just execute
		return fn(s)
	}

	tx, err := s.writerDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	txStore := &sqliteStore{writerDB: s.writerDB, readerDB: s.readerDB, tx: tx}
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
	err := row.Scan(&subscription.ID, &subscription.Name, &subscription.URL, &subscription.Format, &subscription.UserAgent, &enabled,
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

func populateNodeIdentity(node *Node, allowFallback bool) error {
	identity, err := nodecodec.ParseURI(node.URI)
	if err != nil {
		if !allowFallback {
			return fmt.Errorf("invalid node URI: %w", err)
		}
		node.IdentityHash = nodecodec.FallbackHash(node.URI)
		node.CanonicalJSON = ""
		return nil
	}
	node.IdentityHash = identity.Hash
	node.CanonicalJSON = identity.CanonicalJSON
	return nil
}

func scanNode(row *sql.Row) (*Node, error) {
	var n Node
	var enabled int
	var createdAtStr, updatedAtStr, tagsJSON string

	err := row.Scan(
		&n.ID, &n.URI, &n.Name, &n.Source, &n.Port,
		&n.Username, &n.Password, &n.Region, &n.Country,
		&enabled, &tagsJSON, &n.IdentityHash, &n.CanonicalJSON, &createdAtStr, &updatedAtStr,
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
			&enabled, &tagsJSON, &n.IdentityHash, &n.CanonicalJSON, &createdAtStr, &updatedAtStr,
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
