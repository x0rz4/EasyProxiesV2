package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	json "easy_proxies/internal/jsonx"
)

// sqliteMaxVariables bounds one statement's bound-variable count. SQLite's
// default SQLITE_MAX_VARIABLE_NUMBER is higher, but chunking well below it
// keeps batch reads safe on every build.
const sqliteMaxVariables = 900

// inClause renders "(?, ?, ...)" for n bound values. n must be positive.
func inClause(n int) string {
	if n <= 0 {
		return "(NULL)"
	}
	return "(" + strings.Repeat("?, ", n-1) + "?)"
}

// idArgs converts IDs to the any slice ExecContext/QueryContext expects.
func idArgs(ids []int64) []any {
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	return args
}

// chunkIDs splits IDs into slices of at most size elements.
func chunkIDs(ids []int64, size int) [][]int64 {
	if size <= 0 {
		size = sqliteMaxVariables
	}
	if len(ids) == 0 {
		return nil
	}
	chunks := make([][]int64, 0, (len(ids)+size-1)/size)
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
	}
	return chunks
}

// nodeIDCondition renders an optional "WHERE <column> IN (...)" suffix. An empty
// ID slice means "no restriction", matching the batch read contract.
func nodeIDCondition(column string, nodeIDs []int64) (string, []any) {
	if len(nodeIDs) == 0 {
		return "", nil
	}
	return " WHERE " + column + " IN " + inClause(len(nodeIDs)), idArgs(nodeIDs)
}

// loadByNodeIDs runs a keyed batch read, splitting oversized ID lists into
// chunks that stay under the bound-variable limit and merging the results. A nil
// or empty nodeIDs loads every node in one query.
func loadByNodeIDs[T any](ctx context.Context, nodeIDs []int64, load func(context.Context, []int64) (map[int64]T, error)) (map[int64]T, error) {
	if len(nodeIDs) <= sqliteMaxVariables {
		return load(ctx, nodeIDs)
	}
	merged := make(map[int64]T)
	for _, chunk := range chunkIDs(nodeIDs, sqliteMaxVariables) {
		partial, err := load(ctx, chunk)
		if err != nil {
			return nil, err
		}
		for nodeID, value := range partial {
			merged[nodeID] = value
		}
	}
	return merged, nil
}

// ListNodeSubscriptionIDs maps each node to the enabled subscriptions it belongs
// to, or every node when nodeIDs is nil.
func (s *sqliteStore) ListNodeSubscriptionIDs(ctx context.Context, nodeIDs []int64) (map[int64][]int64, error) {
	return loadByNodeIDs(ctx, nodeIDs, s.listNodeSubscriptionIDsChunk)
}

func (s *sqliteStore) listNodeSubscriptionIDsChunk(ctx context.Context, nodeIDs []int64) (map[int64][]int64, error) {
	query := `SELECT subscription_nodes.node_id, subscription_nodes.subscription_id
FROM subscription_nodes
JOIN subscriptions ON subscriptions.id = subscription_nodes.subscription_id AND subscriptions.enabled = 1`
	var args []any
	if len(nodeIDs) > 0 {
		query += " WHERE subscription_nodes.node_id IN " + inClause(len(nodeIDs))
		args = idArgs(nodeIDs)
	}
	query += " ORDER BY subscription_nodes.node_id, subscription_nodes.subscription_id"

	rows, err := s.conn().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list node subscription IDs: %w", err)
	}
	defer rows.Close()
	result := make(map[int64][]int64)
	for rows.Next() {
		var nodeID, subscriptionID int64
		if err := rows.Scan(&nodeID, &subscriptionID); err != nil {
			return nil, err
		}
		result[nodeID] = append(result[nodeID], subscriptionID)
	}
	return result, rows.Err()
}

// ===================== Tag CRUD =====================

const tagColumns = `id, name, color, description, COALESCE(mutex_group_id, 0), priority,
auto_enabled, rule_json, rule_version, builtin_key, created_at, updated_at`

func (s *sqliteStore) ListTags(ctx context.Context) ([]Tag, error) {
	rows, err := s.conn().QueryContext(ctx, "SELECT "+tagColumns+" FROM tags ORDER BY priority DESC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()
	var tags []Tag
	for rows.Next() {
		tag, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

func (s *sqliteStore) GetTag(ctx context.Context, id int64) (*Tag, error) {
	return s.getTagWhere(ctx, "id = ?", id)
}

func (s *sqliteStore) GetTagByName(ctx context.Context, name string) (*Tag, error) {
	return s.getTagWhere(ctx, "name = ?", name)
}

func (s *sqliteStore) getTagWhere(ctx context.Context, condition string, arg any) (*Tag, error) {
	tag, err := scanTag(s.conn().QueryRowContext(ctx, "SELECT "+tagColumns+" FROM tags WHERE "+condition, arg))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tag, nil
}

func (s *sqliteStore) CreateTag(ctx context.Context, tag *Tag) error {
	if tag.RuleVersion < 1 {
		tag.RuleVersion = 1
	}
	now := formatTime(time.Now())
	result, err := s.conn().ExecContext(ctx, `INSERT INTO tags
(name, color, description, mutex_group_id, priority, auto_enabled, rule_json, rule_version, builtin_key, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tag.Name, tag.Color, tag.Description, nullableID(tag.MutexGroupID), tag.Priority,
		boolToInt(tag.AutoEnabled), tag.RuleJSON, tag.RuleVersion, tag.BuiltinKey, now, now)
	if err != nil {
		return fmt.Errorf("create tag: %w", err)
	}
	tag.ID, err = result.LastInsertId()
	return err
}

func (s *sqliteStore) UpdateTag(ctx context.Context, tag *Tag) error {
	result, err := s.conn().ExecContext(ctx, `UPDATE tags SET
name=?, color=?, description=?, mutex_group_id=?, priority=?, auto_enabled=?,
rule_json=?, rule_version=?, builtin_key=?, updated_at=? WHERE id=?`,
		tag.Name, tag.Color, tag.Description, nullableID(tag.MutexGroupID), tag.Priority,
		boolToInt(tag.AutoEnabled), tag.RuleJSON, tag.RuleVersion, tag.BuiltinKey,
		formatTime(time.Now()), tag.ID)
	if err != nil {
		return fmt.Errorf("update tag: %w", err)
	}
	return requireAffected(result, "tag not found")
}

// DeleteTag removes the tag, cascades its assignments, drops it from every
// group pool tag filter, and refreshes the nodes.tags projection of the nodes
// that carried it.
func (s *sqliteStore) DeleteTag(ctx context.Context, id int64) error {
	return s.runInTx(ctx, func(tx *sqliteStore) error {
		affectedNodeIDs, err := tx.nodeIDsForTag(ctx, id)
		if err != nil {
			return err
		}
		result, err := tx.conn().ExecContext(ctx, "DELETE FROM tags WHERE id = ?", id)
		if err != nil {
			return fmt.Errorf("delete tag: %w", err)
		}
		if err := requireAffected(result, "tag not found"); err != nil {
			return err
		}
		if err := tx.removeTagFromGroupFilters(ctx, id); err != nil {
			return err
		}
		return tx.refreshNodeTagProjection(ctx, affectedNodeIDs)
	})
}

func (s *sqliteStore) nodeIDsForTag(ctx context.Context, tagID int64) ([]int64, error) {
	rows, err := s.conn().QueryContext(ctx, "SELECT DISTINCT node_id FROM node_tags WHERE tag_id = ?", tagID)
	if err != nil {
		return nil, fmt.Errorf("list nodes for tag: %w", err)
	}
	defer rows.Close()
	var nodeIDs []int64
	for rows.Next() {
		var nodeID int64
		if err := rows.Scan(&nodeID); err != nil {
			return nil, err
		}
		nodeIDs = append(nodeIDs, nodeID)
	}
	return nodeIDs, rows.Err()
}

// removeTagFromGroupFilters strips a deleted tag ID from both group pool tag
// filter columns so no group keeps referencing a tag that no longer exists.
func (s *sqliteStore) removeTagFromGroupFilters(ctx context.Context, tagID int64) error {
	rows, err := s.conn().QueryContext(ctx,
		"SELECT id, tag_whitelist_json, tag_blacklist_json FROM group_pools")
	if err != nil {
		return fmt.Errorf("list group tag filters: %w", err)
	}
	type update struct {
		id                   int64
		whitelist, blacklist []int64
	}
	var updates []update
	for rows.Next() {
		var row update
		var whitelistJSON, blacklistJSON string
		if err := rows.Scan(&row.id, &whitelistJSON, &blacklistJSON); err != nil {
			rows.Close()
			return err
		}
		var whitelist, blacklist []int64
		_ = json.Unmarshal([]byte(whitelistJSON), &whitelist)
		_ = json.Unmarshal([]byte(blacklistJSON), &blacklist)
		filteredWhitelist, whitelistChanged := removeID(whitelist, tagID)
		filteredBlacklist, blacklistChanged := removeID(blacklist, tagID)
		if !whitelistChanged && !blacklistChanged {
			continue
		}
		row.whitelist, row.blacklist = filteredWhitelist, filteredBlacklist
		updates = append(updates, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, row := range updates {
		whitelistJSON, _ := json.Marshal(row.whitelist)
		blacklistJSON, _ := json.Marshal(row.blacklist)
		if _, err := s.conn().ExecContext(ctx,
			"UPDATE group_pools SET tag_whitelist_json=?, tag_blacklist_json=?, updated_at=? WHERE id=?",
			string(whitelistJSON), string(blacklistJSON), formatTime(time.Now()), row.id); err != nil {
			return fmt.Errorf("clear tag from group pool %d: %w", row.id, err)
		}
	}
	return nil
}

func removeID(ids []int64, target int64) ([]int64, bool) {
	filtered := make([]int64, 0, len(ids))
	removed := false
	for _, id := range ids {
		if id == target {
			removed = true
			continue
		}
		filtered = append(filtered, id)
	}
	return filtered, removed
}

func scanTag(row scanner) (Tag, error) {
	var tag Tag
	var autoEnabled int
	var createdAt, updatedAt string
	err := row.Scan(&tag.ID, &tag.Name, &tag.Color, &tag.Description, &tag.MutexGroupID, &tag.Priority,
		&autoEnabled, &tag.RuleJSON, &tag.RuleVersion, &tag.BuiltinKey, &createdAt, &updatedAt)
	if err != nil {
		return tag, err
	}
	tag.AutoEnabled = autoEnabled != 0
	tag.CreatedAt, tag.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	return tag, nil
}

func nullableID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// ===================== Tag mutex groups =====================

func (s *sqliteStore) ListTagMutexGroups(ctx context.Context) ([]TagMutexGroup, error) {
	rows, err := s.conn().QueryContext(ctx,
		"SELECT id, name, description, created_at, updated_at FROM tag_mutex_groups ORDER BY id ASC")
	if err != nil {
		return nil, fmt.Errorf("list tag mutex groups: %w", err)
	}
	defer rows.Close()
	var groups []TagMutexGroup
	for rows.Next() {
		var group TagMutexGroup
		var createdAt, updatedAt string
		if err := rows.Scan(&group.ID, &group.Name, &group.Description, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		group.CreatedAt, group.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *sqliteStore) CreateTagMutexGroup(ctx context.Context, group *TagMutexGroup) error {
	now := formatTime(time.Now())
	result, err := s.conn().ExecContext(ctx,
		"INSERT INTO tag_mutex_groups (name, description, created_at, updated_at) VALUES (?, ?, ?, ?)",
		group.Name, group.Description, now, now)
	if err != nil {
		return fmt.Errorf("create tag mutex group: %w", err)
	}
	group.ID, err = result.LastInsertId()
	return err
}

func (s *sqliteStore) UpdateTagMutexGroup(ctx context.Context, group *TagMutexGroup) error {
	result, err := s.conn().ExecContext(ctx,
		"UPDATE tag_mutex_groups SET name=?, description=?, updated_at=? WHERE id=?",
		group.Name, group.Description, formatTime(time.Now()), group.ID)
	if err != nil {
		return fmt.Errorf("update tag mutex group: %w", err)
	}
	return requireAffected(result, "tag mutex group not found")
}

// DeleteTagMutexGroup detaches member tags through ON DELETE SET NULL; the tags
// themselves survive with no mutual exclusion.
func (s *sqliteStore) DeleteTagMutexGroup(ctx context.Context, id int64) error {
	result, err := s.conn().ExecContext(ctx, "DELETE FROM tag_mutex_groups WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete tag mutex group: %w", err)
	}
	return requireAffected(result, "tag mutex group not found")
}

// ===================== Tag assignments =====================

func (s *sqliteStore) ListNodeTags(ctx context.Context, filter NodeTagFilter) ([]NodeTag, error) {
	if len(filter.NodeIDs) > sqliteMaxVariables || len(filter.TagIDs) > sqliteMaxVariables {
		return s.listNodeTagsChunked(ctx, filter)
	}
	query := "SELECT node_id, tag_id, source, rule_version, matched_at, updated_at FROM node_tags"
	var conditions []string
	var args []any
	if len(filter.NodeIDs) > 0 {
		conditions = append(conditions, "node_id IN "+inClause(len(filter.NodeIDs)))
		args = append(args, idArgs(filter.NodeIDs)...)
	}
	if len(filter.TagIDs) > 0 {
		conditions = append(conditions, "tag_id IN "+inClause(len(filter.TagIDs)))
		args = append(args, idArgs(filter.TagIDs)...)
	}
	if filter.Source != "" {
		conditions = append(conditions, "source = ?")
		args = append(args, filter.Source)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY node_id ASC, tag_id ASC, source ASC"

	rows, err := s.conn().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list node tags: %w", err)
	}
	defer rows.Close()
	var assignments []NodeTag
	for rows.Next() {
		var assignment NodeTag
		var matchedAt, updatedAt string
		if err := rows.Scan(&assignment.NodeID, &assignment.TagID, &assignment.Source,
			&assignment.RuleVersion, &matchedAt, &updatedAt); err != nil {
			return nil, err
		}
		assignment.MatchedAt, assignment.UpdatedAt = parseTime(matchedAt), parseTime(updatedAt)
		assignments = append(assignments, assignment)
	}
	return assignments, rows.Err()
}

// listNodeTagsChunked splits an oversized ID filter across statements. Only one
// dimension is chunked at a time so the IN clauses stay bounded.
func (s *sqliteStore) listNodeTagsChunked(ctx context.Context, filter NodeTagFilter) ([]NodeTag, error) {
	chunked, rest := filter.NodeIDs, filter
	dimension := "node"
	if len(filter.TagIDs) > len(filter.NodeIDs) {
		chunked, dimension = filter.TagIDs, "tag"
	}
	var merged []NodeTag
	for _, chunk := range chunkIDs(chunked, sqliteMaxVariables) {
		if dimension == "node" {
			rest.NodeIDs = chunk
		} else {
			rest.TagIDs = chunk
		}
		assignments, err := s.ListNodeTags(ctx, rest)
		if err != nil {
			return nil, err
		}
		merged = append(merged, assignments...)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].NodeID != merged[j].NodeID {
			return merged[i].NodeID < merged[j].NodeID
		}
		if merged[i].TagID != merged[j].TagID {
			return merged[i].TagID < merged[j].TagID
		}
		return merged[i].Source < merged[j].Source
	})
	return merged, nil
}

func (s *sqliteStore) CountNodesByTag(ctx context.Context) (map[int64]int, error) {
	rows, err := s.conn().QueryContext(ctx,
		"SELECT tag_id, COUNT(DISTINCT node_id) FROM node_tags GROUP BY tag_id")
	if err != nil {
		return nil, fmt.Errorf("count nodes by tag: %w", err)
	}
	defer rows.Close()
	counts := make(map[int64]int)
	for rows.Next() {
		var tagID int64
		var count int
		if err := rows.Scan(&tagID, &count); err != nil {
			return nil, err
		}
		counts[tagID] = count
	}
	return counts, rows.Err()
}

// SetManualNodeTags replaces one node's manual assignments. Auto assignments
// stay untouched, and the nodes.tags projection is rebuilt from both sources.
func (s *sqliteStore) SetManualNodeTags(ctx context.Context, nodeID int64, tagIDs []int64) error {
	return s.runInTx(ctx, func(tx *sqliteStore) error {
		if _, err := tx.conn().ExecContext(ctx,
			"DELETE FROM node_tags WHERE node_id = ? AND source = ?", nodeID, NodeTagSourceManual); err != nil {
			return fmt.Errorf("clear manual node tags: %w", err)
		}
		if err := tx.insertNodeTags(ctx, nodeID, normalizeIDList(tagIDs), nil, NodeTagSourceManual); err != nil {
			return err
		}
		return tx.refreshNodeTagProjection(ctx, []int64{nodeID})
	})
}

// BatchUpdateManualNodeTags adds and removes manual assignments across many
// nodes. Removals are applied after additions so a tag listed in both ends up
// removed.
func (s *sqliteStore) BatchUpdateManualNodeTags(ctx context.Context, nodeIDs, addTagIDs, removeTagIDs []int64) error {
	nodes := normalizeIDList(nodeIDs)
	adds, removes := normalizeIDList(addTagIDs), normalizeIDList(removeTagIDs)
	if len(nodes) == 0 || (len(adds) == 0 && len(removes) == 0) {
		return nil
	}
	return s.runInTx(ctx, func(tx *sqliteStore) error {
		for _, nodeID := range nodes {
			if err := tx.insertNodeTags(ctx, nodeID, adds, nil, NodeTagSourceManual); err != nil {
				return err
			}
		}
		for _, chunk := range chunkIDs(removes, sqliteMaxVariables/2) {
			for _, nodeChunk := range chunkIDs(nodes, sqliteMaxVariables/2) {
				args := append(idArgs(nodeChunk), idArgs(chunk)...)
				args = append(args, NodeTagSourceManual)
				if _, err := tx.conn().ExecContext(ctx,
					"DELETE FROM node_tags WHERE node_id IN "+inClause(len(nodeChunk))+
						" AND tag_id IN "+inClause(len(chunk))+" AND source = ?", args...); err != nil {
					return fmt.Errorf("remove manual node tags: %w", err)
				}
			}
		}
		return tx.refreshNodeTagProjection(ctx, nodes)
	})
}

// ReplaceAutoNodeTags is the only writer of source='auto' rows. It deletes just
// those rows, so manual assignments cannot be clobbered by a recompute.
func (s *sqliteStore) ReplaceAutoNodeTags(ctx context.Context, assignments []NodeAutoTagAssignment) error {
	if len(assignments) == 0 {
		return nil
	}
	return s.runInTx(ctx, func(tx *sqliteStore) error {
		nodeIDs := make([]int64, 0, len(assignments))
		for _, assignment := range assignments {
			if assignment.NodeID <= 0 {
				continue
			}
			if _, err := tx.conn().ExecContext(ctx,
				"DELETE FROM node_tags WHERE node_id = ? AND source = ?",
				assignment.NodeID, NodeTagSourceAuto); err != nil {
				return fmt.Errorf("clear auto node tags: %w", err)
			}
			if err := tx.insertNodeTags(ctx, assignment.NodeID, assignment.TagIDs,
				assignment.RuleVersions, NodeTagSourceAuto); err != nil {
				return err
			}
			nodeIDs = append(nodeIDs, assignment.NodeID)
		}
		return tx.refreshNodeTagProjection(ctx, nodeIDs)
	})
}

// insertNodeTags writes assignments for one node. ruleVersions may be nil or
// shorter than tagIDs, in which case the missing entries record 0.
func (s *sqliteStore) insertNodeTags(ctx context.Context, nodeID int64, tagIDs []int64, ruleVersions []int, source string) error {
	if len(tagIDs) == 0 {
		return nil
	}
	now := formatTime(time.Now())
	for index, tagID := range tagIDs {
		if tagID <= 0 {
			continue
		}
		ruleVersion := 0
		if index < len(ruleVersions) {
			ruleVersion = ruleVersions[index]
		}
		if _, err := s.conn().ExecContext(ctx, `INSERT INTO node_tags
(node_id, tag_id, source, rule_version, matched_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id, tag_id, source) DO UPDATE SET rule_version=excluded.rule_version,
updated_at=excluded.updated_at`, nodeID, tagID, source, ruleVersion, now, now); err != nil {
			return fmt.Errorf("insert node tag %d/%d: %w", nodeID, tagID, err)
		}
	}
	return nil
}

// refreshNodeTagProjection rewrites nodes.tags for the given nodes from their
// assignments. It deliberately bypasses UpdateNode so tag churn never bumps
// nodes.updated_at, which identity reconciliation and subscription diffing read.
func (s *sqliteStore) refreshNodeTagProjection(ctx context.Context, nodeIDs []int64) error {
	unique := normalizeIDList(nodeIDs)
	if len(unique) == 0 {
		return nil
	}
	names := make(map[int64][]string, len(unique))
	for _, chunk := range chunkIDs(unique, sqliteMaxVariables) {
		rows, err := s.conn().QueryContext(ctx, `SELECT node_tags.node_id, tags.name
FROM node_tags JOIN tags ON tags.id = node_tags.tag_id
WHERE node_tags.node_id IN `+inClause(len(chunk)), idArgs(chunk)...)
		if err != nil {
			return fmt.Errorf("load node tag projection: %w", err)
		}
		for rows.Next() {
			var nodeID int64
			var name string
			if err := rows.Scan(&nodeID, &name); err != nil {
				rows.Close()
				return err
			}
			names[nodeID] = append(names[nodeID], name)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	for _, nodeID := range unique {
		projection := dedupeSorted(names[nodeID])
		encoded, err := json.Marshal(projection)
		if err != nil {
			return fmt.Errorf("encode node tag projection: %w", err)
		}
		if _, err := s.conn().ExecContext(ctx,
			"UPDATE nodes SET tags = ? WHERE id = ?", string(encoded), nodeID); err != nil {
			return fmt.Errorf("write node tag projection: %w", err)
		}
	}
	return nil
}

// dedupeSorted returns a sorted, duplicate-free copy, never nil, so the
// projection column always holds a JSON array.
func dedupeSorted(values []string) []string {
	unique := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}
