package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/nodecodec"
)

type identityMigrationNode struct {
	ID            int64
	URI           string
	Name          string
	Source        string
	Enabled       bool
	Tags          []string
	Region        string
	Country       string
	IdentityHash  string
	CanonicalJSON string
}

// reconcileNodeIdentities is the data phase of migration 9. Schema changes are
// applied first; this phase backfills and merges before the unique index exists.
func reconcileNodeIdentities(db *sql.DB) error {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, uri, name, source, enabled, tags, region, country FROM nodes ORDER BY id`)
	if err != nil {
		return err
	}
	var nodes []identityMigrationNode
	for rows.Next() {
		var node identityMigrationNode
		var enabled int
		var tagsJSON string
		if err := rows.Scan(&node.ID, &node.URI, &node.Name, &node.Source, &enabled, &tagsJSON, &node.Region, &node.Country); err != nil {
			rows.Close()
			return err
		}
		node.Enabled = enabled != 0
		_ = json.Unmarshal([]byte(tagsJSON), &node.Tags)
		if identity, parseErr := nodecodec.ParseURI(node.URI); parseErr == nil {
			node.IdentityHash, node.CanonicalJSON = identity.Hash, identity.CanonicalJSON
		} else {
			node.IdentityHash = nodecodec.FallbackHash(node.URI)
		}
		nodes = append(nodes, node)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	groups := make(map[string][]identityMigrationNode)
	for _, node := range nodes {
		groups[node.IdentityHash] = append(groups[node.IdentityHash], node)
	}
	for _, duplicates := range groups {
		sort.Slice(duplicates, func(i, j int) bool { return duplicates[i].ID < duplicates[j].ID })
		winner := mergeNodeMetadata(duplicates)
		if len(duplicates) > 1 {
			for _, loser := range duplicates[1:] {
				if err := mergeNodeReferences(tx, winner.ID, loser.ID); err != nil {
					return fmt.Errorf("merge node %d into %d: %w", loser.ID, winner.ID, err)
				}
			}
		}
		tagsJSON, _ := json.Marshal(winner.Tags)
		if _, err := tx.Exec(`UPDATE nodes SET name=?, source=?, enabled=?, tags=?, region=?, country=?, identity_hash=?, canonical_json=? WHERE id=?`,
			winner.Name, winner.Source, boolToInt(winner.Enabled), string(tagsJSON), winner.Region, winner.Country,
			winner.IdentityHash, winner.CanonicalJSON, winner.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_nodes_identity_hash ON nodes(identity_hash) WHERE identity_hash<>''`); err != nil {
		return fmt.Errorf("create node identity unique index: %w", err)
	}
	return tx.Commit()
}

func mergeNodeMetadata(nodes []identityMigrationNode) identityMigrationNode {
	winner := nodes[0]
	allEnabled := true
	tagSet := map[string]struct{}{}
	bestNamePriority := sourcePriority(winner.Source)
	for _, node := range nodes {
		allEnabled = allEnabled && node.Enabled
		for _, tag := range node.Tags {
			if tag != "" {
				tagSet[tag] = struct{}{}
			}
		}
		priority := sourcePriority(node.Source)
		if priority > sourcePriority(winner.Source) {
			winner.Source = node.Source
		}
		if node.Name != "" && (winner.Name == "" || priority > bestNamePriority) {
			winner.Name, bestNamePriority = node.Name, priority
		}
		if winner.Region == "" && node.Region != "" {
			winner.Region = node.Region
		}
		if winner.Country == "" && node.Country != "" {
			winner.Country = node.Country
		}
	}
	winner.Enabled = allEnabled
	winner.Tags = make([]string, 0, len(tagSet))
	for tag := range tagSet {
		winner.Tags = append(winner.Tags, tag)
	}
	sort.Strings(winner.Tags)
	return winner
}

func sourcePriority(source string) int {
	switch source {
	case NodeSourceInline:
		return 4
	case NodeSourceManual:
		return 3
	case NodeSourceFile:
		return 2
	default:
		return 1
	}
}

func mergeNodeReferences(tx *sql.Tx, winnerID, loserID int64) error {
	if _, err := tx.Exec(`INSERT INTO subscription_nodes(subscription_id,node_id,position)
		SELECT subscription_id,?,position FROM subscription_nodes WHERE node_id=?
		ON CONFLICT(subscription_id,node_id) DO UPDATE SET position=MIN(position,excluded.position)`, winnerID, loserID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM subscription_nodes WHERE node_id=?`, loserID); err != nil {
		return err
	}

	if err := mergeStats(tx, winnerID, loserID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE node_timeline SET node_id=? WHERE node_id=?`, winnerID, loserID); err != nil {
		return err
	}
	if err := mergeUnlockResult(tx, winnerID, loserID); err != nil {
		return err
	}
	if err := mergeDetectionResults(tx, winnerID, loserID); err != nil {
		return err
	}
	if err := rewriteGroupReferences(tx, winnerID, loserID); err != nil {
		return err
	}
	if err := mergeGroupStates(tx, winnerID, loserID); err != nil {
		return err
	}
	if err := mergeNodeTags(tx, winnerID, loserID); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM nodes WHERE id=?`, loserID)
	return err
}

// mergeNodeTags moves the loser's tag assignments to the winner, preserving the
// manual/auto split. Without this the loser's rows would vanish with the
// ON DELETE CASCADE below, silently dropping tags on every store.Open(). The
// caller rewrites the winner's nodes.tags projection from the merged name union.
func mergeNodeTags(tx *sql.Tx, winnerID, loserID int64) error {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO node_tags(node_id,tag_id,source,rule_version,matched_at,updated_at)
		SELECT ?,tag_id,source,rule_version,matched_at,updated_at FROM node_tags WHERE node_id=?`,
		winnerID, loserID); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM node_tags WHERE node_id=?`, loserID)
	return err
}

func mergeDetectionResults(tx *sql.Tx, winnerID, loserID int64) error {
	var winnerTime, loserTime string
	_ = tx.QueryRow(`SELECT updated_at FROM node_detection_results WHERE node_id=?`, winnerID).Scan(&winnerTime)
	_ = tx.QueryRow(`SELECT updated_at FROM node_detection_results WHERE node_id=?`, loserID).Scan(&loserTime)
	if loserTime != "" && loserTime > winnerTime {
		if _, err := tx.Exec(`DELETE FROM node_detection_results WHERE node_id=?`, winnerID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE node_detection_results SET node_id=? WHERE node_id=?`, winnerID, loserID); err != nil {
			return err
		}
	} else if _, err := tx.Exec(`DELETE FROM node_detection_results WHERE node_id=?`, loserID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO node_ip_quality_results(node_id,provider,task_id,status,ip,family,country,country_code,asn,org,isp,
		is_broadcast,is_residential,fraud_score,proxy,hosting,mobile,reason,checked_at,updated_at)
		SELECT ?,provider,task_id,status,ip,family,country,country_code,asn,org,isp,is_broadcast,is_residential,fraud_score,
		proxy,hosting,mobile,reason,checked_at,updated_at FROM node_ip_quality_results WHERE node_id=?
		ON CONFLICT(node_id,provider) DO UPDATE SET task_id=excluded.task_id,status=excluded.status,ip=excluded.ip,family=excluded.family,
		country=excluded.country,country_code=excluded.country_code,asn=excluded.asn,org=excluded.org,isp=excluded.isp,
		is_broadcast=excluded.is_broadcast,is_residential=excluded.is_residential,fraud_score=excluded.fraud_score,
		proxy=excluded.proxy,hosting=excluded.hosting,mobile=excluded.mobile,reason=excluded.reason,checked_at=excluded.checked_at,
		updated_at=excluded.updated_at WHERE excluded.checked_at > node_ip_quality_results.checked_at`, winnerID, loserID); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM node_ip_quality_results WHERE node_id=?`, loserID)
	return err
}

func mergeStats(tx *sql.Tx, winnerID, loserID int64) error {
	var latestError string
	var latestLatency, latestAvailable, latestInitial int
	_ = tx.QueryRow(`SELECT last_error,last_latency_ms,available,initial_check_done FROM node_stats
		WHERE node_id IN (?,?) ORDER BY updated_at DESC,node_id DESC LIMIT 1`, winnerID, loserID).
		Scan(&latestError, &latestLatency, &latestAvailable, &latestInitial)
	_, err := tx.Exec(`UPDATE node_stats SET
		failure_count=failure_count+COALESCE((SELECT failure_count FROM node_stats WHERE node_id=?),0),
		success_count=success_count+COALESCE((SELECT success_count FROM node_stats WHERE node_id=?),0),
		blacklisted=MAX(blacklisted,COALESCE((SELECT blacklisted FROM node_stats WHERE node_id=?),0)),
		blacklisted_until=MAX(blacklisted_until,COALESCE((SELECT blacklisted_until FROM node_stats WHERE node_id=?),'')),
		last_failure_at=MAX(last_failure_at,COALESCE((SELECT last_failure_at FROM node_stats WHERE node_id=?),'')),
		last_success_at=MAX(last_success_at,COALESCE((SELECT last_success_at FROM node_stats WHERE node_id=?),'')),
		last_error=?,last_latency_ms=?,available=?,initial_check_done=?,
		total_upload_bytes=total_upload_bytes+COALESCE((SELECT total_upload_bytes FROM node_stats WHERE node_id=?),0),
		total_download_bytes=total_download_bytes+COALESCE((SELECT total_download_bytes FROM node_stats WHERE node_id=?),0),
		updated_at=MAX(updated_at,COALESCE((SELECT updated_at FROM node_stats WHERE node_id=?),''))
		WHERE node_id=?`, loserID, loserID, loserID, loserID, loserID, loserID,
		latestError, latestLatency, latestAvailable, latestInitial,
		loserID, loserID, loserID, winnerID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM node_stats WHERE node_id=?`, loserID)
	return err
}

func mergeUnlockResult(tx *sql.Tx, winnerID, loserID int64) error {
	var winnerTime, loserTime string
	_ = tx.QueryRow(`SELECT checked_at FROM node_unlock_results WHERE node_id=?`, winnerID).Scan(&winnerTime)
	_ = tx.QueryRow(`SELECT checked_at FROM node_unlock_results WHERE node_id=?`, loserID).Scan(&loserTime)
	if loserTime != "" && loserTime > winnerTime {
		if _, err := tx.Exec(`DELETE FROM node_unlock_results WHERE node_id=?`, winnerID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE node_unlock_results SET node_id=? WHERE node_id=?`, winnerID, loserID)
		return err
	}
	_, err := tx.Exec(`DELETE FROM node_unlock_results WHERE node_id=?`, loserID)
	return err
}

func rewriteGroupReferences(tx *sql.Tx, winnerID, loserID int64) error {
	rows, err := tx.Query(`SELECT id,explicit_node_ids_json,excluded_node_ids_json,current_active_node_id FROM group_pools`)
	if err != nil {
		return err
	}
	type update struct {
		id                 int64
		explicit, excluded string
		current            int64
	}
	var updates []update
	for rows.Next() {
		var id, current int64
		var explicitJSON, excludedJSON string
		if err := rows.Scan(&id, &explicitJSON, &excludedJSON, &current); err != nil {
			rows.Close()
			return err
		}
		explicitJSON = replaceNodeIDJSON(explicitJSON, winnerID, loserID)
		excludedJSON = replaceNodeIDJSON(excludedJSON, winnerID, loserID)
		if current == loserID {
			current = winnerID
		}
		updates = append(updates, update{id, explicitJSON, excludedJSON, current})
	}
	rows.Close()
	for _, item := range updates {
		if _, err := tx.Exec(`UPDATE group_pools SET explicit_node_ids_json=?,excluded_node_ids_json=?,current_active_node_id=? WHERE id=?`, item.explicit, item.excluded, item.current, item.id); err != nil {
			return err
		}
	}
	return nil
}

func replaceNodeIDJSON(raw string, winnerID, loserID int64) string {
	var ids []int64
	_ = json.Unmarshal([]byte(raw), &ids)
	seen := map[int64]struct{}{}
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id == loserID {
			id = winnerID
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	encoded, _ := json.Marshal(result)
	return string(encoded)
}

func mergeGroupStates(tx *sql.Tx, winnerID, loserID int64) error {
	rows, err := tx.Query(`SELECT group_id,node_id,failure_history_json,evicted,last_error,evicted_at,updated_at
		FROM group_node_states WHERE node_id IN (?,?) ORDER BY group_id,node_id`, winnerID, loserID)
	if err != nil {
		return err
	}
	type state struct {
		history                         []int64
		evicted                         bool
		lastError, evictedAt, updatedAt string
	}
	states := map[int64]state{}
	for rows.Next() {
		var groupID, nodeID int64
		var historyJSON, lastError, evictedAt, updatedAt string
		var evicted int
		if err := rows.Scan(&groupID, &nodeID, &historyJSON, &evicted, &lastError, &evictedAt, &updatedAt); err != nil {
			rows.Close()
			return err
		}
		current := states[groupID]
		var history []int64
		_ = json.Unmarshal([]byte(historyJSON), &history)
		current.history = append(current.history, history...)
		current.evicted = current.evicted || evicted != 0
		if updatedAt >= current.updatedAt {
			current.lastError, current.updatedAt = lastError, updatedAt
		}
		if evictedAt > current.evictedAt {
			current.evictedAt = evictedAt
		}
		states[groupID] = current
	}
	rows.Close()
	if _, err := tx.Exec(`DELETE FROM group_node_states WHERE node_id IN (?,?)`, winnerID, loserID); err != nil {
		return err
	}
	for groupID, item := range states {
		sort.Slice(item.history, func(i, j int) bool { return item.history[i] < item.history[j] })
		item.history = uniqueInt64(item.history)
		historyJSON, _ := json.Marshal(item.history)
		if _, err := tx.Exec(`INSERT INTO group_node_states(group_id,node_id,failure_history_json,evicted,last_error,evicted_at,updated_at) VALUES(?,?,?,?,?,?,?)`, groupID, winnerID, string(historyJSON), boolToInt(item.evicted), item.lastError, item.evictedAt, item.updatedAt); err != nil {
			return err
		}
	}
	return nil
}

func uniqueInt64(values []int64) []int64 {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}
