// Package store defines the data persistence layer for easy_proxies.
// It abstracts all storage operations behind the Store interface,
// allowing different backend implementations (e.g., SQLite).
package store

import (
	"context"
	"time"
)

// Store defines all data storage operations.
type Store interface {
	// --- Node operations ---

	// ListNodes returns nodes matching the given filter.
	ListNodes(ctx context.Context, filter NodeFilter) ([]Node, error)
	// ListManagedNodes returns nodes visible to node management. Subscription
	// nodes are included only through enabled subscription memberships.
	ListManagedNodes(ctx context.Context, subscriptionID *int64) ([]ManagedNode, error)

	// GetNode returns a node by its ID.
	GetNode(ctx context.Context, id int64) (*Node, error)

	// GetNodeByURI returns a node by its URI.
	GetNodeByURI(ctx context.Context, uri string) (*Node, error)

	// GetNodeByName returns a node by its name.
	GetNodeByName(ctx context.Context, name string) (*Node, error)

	// CreateNode inserts a new node and returns its assigned ID.
	CreateNode(ctx context.Context, node *Node) error

	// UpdateNode updates an existing node by ID.
	UpdateNode(ctx context.Context, node *Node) error

	// DeleteNode removes a node by ID (cascading to stats/timeline).
	DeleteNode(ctx context.Context, id int64) error

	// DeleteNodesBySource removes all nodes with the given source.
	// Returns the number of deleted rows.
	DeleteNodesBySource(ctx context.Context, source string) (int64, error)

	// BulkUpsertNodes inserts or updates nodes in a single transaction.
	// Nodes are matched by URI for upsert logic.
	BulkUpsertNodes(ctx context.Context, nodes []Node) error

	// CountNodes returns the total number of nodes matching the filter.
	CountNodes(ctx context.Context, filter NodeFilter) (int64, error)

	// --- Subscriptions ---
	ListSubscriptions(ctx context.Context) ([]Subscription, error)
	GetSubscription(ctx context.Context, id int64) (*Subscription, error)
	GetSubscriptionByURL(ctx context.Context, url string) (*Subscription, error)
	CreateSubscription(ctx context.Context, subscription *Subscription) error
	UpdateSubscription(ctx context.Context, subscription *Subscription) error
	DeleteSubscription(ctx context.Context, id int64) error
	SetSubscriptionEnabled(ctx context.Context, id int64, enabled bool) error
	UpdateAllSubscriptionRefreshSettings(ctx context.Context, intervalSeconds, timeoutSeconds int) error
	ActivateSubscriptionExclusive(ctx context.Context, id int64) error
	ListSubscriptionNodes(ctx context.Context, subscriptionID int64) ([]SubscriptionNode, error)
	// ListEffectiveSubscriptionNodes returns enabled nodes which are present in
	// at least one enabled subscription, de-duplicated by node ID.
	ListEffectiveSubscriptionNodes(ctx context.Context) ([]Node, error)
	ReplaceSubscriptionNodes(ctx context.Context, subscriptionID int64, nodes []SubscriptionNodeInput) error
	CommitSnapshot(ctx context.Context, subscriptionID int64, nodes []SubscriptionNodeInput, snapshot SubscriptionSnapshot) error

	// --- Node stats ---

	// GetNodeStats returns runtime statistics for a node.
	GetNodeStats(ctx context.Context, nodeID int64) (*NodeStats, error)

	// UpsertNodeStats creates or updates node statistics.
	UpsertNodeStats(ctx context.Context, stats *NodeStats) error

	// RecordSuccess increments success count and updates latency.
	RecordSuccess(ctx context.Context, nodeID int64, latencyMs int64) error

	// RecordFailure increments failure count and records the error.
	RecordFailure(ctx context.Context, nodeID int64, errMsg string) error

	// SetBlacklist marks a node as blacklisted until the given time.
	SetBlacklist(ctx context.Context, nodeID int64, until time.Time) error

	// ClearBlacklist removes the blacklist flag for a node.
	ClearBlacklist(ctx context.Context, nodeID int64) error

	// ClearAllBlacklists removes all blacklist flags.
	ClearAllBlacklists(ctx context.Context) error

	// BatchUpdateStats applies multiple stat updates in a single transaction.
	BatchUpdateStats(ctx context.Context, updates []StatsUpdate) error

	// GetAllNodeStats returns stats for all nodes (used for bulk restore).
	GetAllNodeStats(ctx context.Context) (map[int64]*NodeStats, error)

	// --- Timeline ---

	// AppendTimeline adds an event to a node's timeline.
	AppendTimeline(ctx context.Context, nodeID int64, event TimelineEvent) error

	// GetTimeline returns the most recent events for a node.
	GetTimeline(ctx context.Context, nodeID int64, limit int) ([]TimelineEvent, error)

	// CleanupTimeline removes old timeline events, keeping only the most recent per node.
	CleanupTimeline(ctx context.Context, keepPerNode int) error

	// --- Sessions ---

	// CreateSession stores a new session.
	CreateSession(ctx context.Context, session *Session) error

	// GetSession retrieves a session by token.
	GetSession(ctx context.Context, token string) (*Session, error)

	// DeleteSession removes a session by token.
	DeleteSession(ctx context.Context, token string) error

	// CleanupExpiredSessions removes all expired sessions.
	CleanupExpiredSessions(ctx context.Context) error

	// --- Subscription status ---

	// GetSubscriptionStatus returns the current subscription refresh status.
	GetSubscriptionStatus(ctx context.Context) (*SubscriptionStatus, error)

	// UpdateSubscriptionStatus creates or updates the subscription status.
	UpdateSubscriptionStatus(ctx context.Context, status *SubscriptionStatus) error

	// --- Lifecycle ---

	// Close releases all resources held by the store.
	Close() error

	// WithTx executes fn within a transaction. If fn returns an error,
	// the transaction is rolled back; otherwise it is committed.
	WithTx(ctx context.Context, fn func(tx Store) error) error
}

// --- Data models ---

// Node represents a proxy node stored in the database.
type Node struct {
	ID        int64     `json:"id"`
	URI       string    `json:"uri"`
	Name      string    `json:"name"`
	Source    string    `json:"source"` // inline, nodes_file, subscription, manual
	Port      uint16    `json:"port"`
	Username  string    `json:"username,omitempty"`
	Password  string    `json:"password,omitempty"`
	Region    string    `json:"region,omitempty"`
	Country   string    `json:"country,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ManagedNode is the node-management view of a node and its enabled subscriptions.
type ManagedNode struct {
	Node
	SubscriptionIDs []int64 `json:"subscription_ids"`
}

// NodeFilter specifies criteria for listing nodes.
type NodeFilter struct {
	Source  string // Filter by source (empty = all)
	Region  string // Filter by region (empty = all)
	Enabled *bool  // Filter by enabled status (nil = all)
	Limit   int    // Max results (0 = no limit)
	Offset  int    // Pagination offset
}

// Subscription is a remotely refreshed collection of nodes.
type Subscription struct {
	ID                     int64     `json:"id"`
	Name                   string    `json:"name"`
	URL                    string    `json:"url"`
	Enabled                bool      `json:"enabled"`
	RefreshIntervalSeconds int       `json:"refresh_interval_seconds"`
	RefreshTimeoutSeconds  int       `json:"refresh_timeout_seconds"`
	SortOrder              int       `json:"sort_order"`
	LastAttempt            time.Time `json:"last_attempt"`
	LastSuccess            time.Time `json:"last_success"`
	LastError              string    `json:"last_error"`
	NodeCount              int       `json:"node_count"`
	ETag                   string    `json:"etag"`
	LastModified           string    `json:"last_modified"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// SubscriptionNode describes a node's membership in a subscription.
type SubscriptionNode struct {
	SubscriptionID int64 `json:"subscription_id"`
	Position       int   `json:"position"`
	Node           Node  `json:"node"`
}

// SubscriptionNodeInput contains the node data required to commit a snapshot.
// Enabled is only used when a URI is first inserted; existing node state wins.
type SubscriptionNodeInput struct {
	URI      string `json:"uri"`
	Name     string `json:"name"`
	Port     uint16 `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Region   string `json:"region,omitempty"`
	Country  string `json:"country,omitempty"`
	Enabled  bool   `json:"enabled"`
}

// SubscriptionSnapshot is refresh metadata committed with a node snapshot.
type SubscriptionSnapshot struct {
	Attempt      time.Time
	Success      time.Time
	Error        string
	ETag         string
	LastModified string
}

// NodeStats holds runtime statistics for a node.
type NodeStats struct {
	NodeID             int64     `json:"node_id"`
	FailureCount       int       `json:"failure_count"`
	SuccessCount       int64     `json:"success_count"`
	Blacklisted        bool      `json:"blacklisted"`
	BlacklistedUntil   time.Time `json:"blacklisted_until"`
	LastError          string    `json:"last_error"`
	LastFailureAt      time.Time `json:"last_failure_at"`
	LastSuccessAt      time.Time `json:"last_success_at"`
	LastLatencyMs      int64     `json:"last_latency_ms"` // -1 = untested
	Available          bool      `json:"available"`
	InitialCheckDone   bool      `json:"initial_check_done"`
	TotalUploadBytes   int64     `json:"total_upload_bytes"`
	TotalDownloadBytes int64     `json:"total_download_bytes"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// StatsUpdate represents a batch update for node statistics.
type StatsUpdate struct {
	NodeID             int64
	FailureCount       int
	SuccessCount       int64
	Blacklisted        bool
	BlacklistedUntil   time.Time
	LastError          string
	LastFailureAt      time.Time
	LastSuccessAt      time.Time
	LastLatencyMs      int64
	Available          bool
	InitialCheckDone   bool
	TotalUploadBytes   int64
	TotalDownloadBytes int64
}

// TimelineEvent represents a single usage event for debug tracking.
type TimelineEvent struct {
	ID        int64     `json:"id"`
	NodeID    int64     `json:"node_id"`
	Success   bool      `json:"success"`
	LatencyMs int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Session represents a user authentication session.
type Session struct {
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SubscriptionStatus represents subscription refresh status.
type SubscriptionStatus struct {
	LastRefresh  time.Time `json:"last_refresh"`
	NextRefresh  time.Time `json:"next_refresh"`
	NodeCount    int       `json:"node_count"`
	LastError    string    `json:"last_error"`
	RefreshCount int       `json:"refresh_count"`
	IsRefreshing bool      `json:"is_refreshing"`
	NodesHash    string    `json:"nodes_hash"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Node source constants (matching config.NodeSource values).
const (
	NodeSourceInline       = "inline"
	NodeSourceFile         = "nodes_file"
	NodeSourceSubscription = "subscription"
	NodeSourceManual       = "manual"
)
