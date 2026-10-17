package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/store"
)

// NodeManager exposes config node CRUD and reload operations.
type NodeManager interface {
	ListConfigNodes(ctx context.Context, subscriptionID *int64) ([]ManagedNodeConfig, error)
	CreateNode(ctx context.Context, node config.NodeConfig) (config.NodeConfig, error)
	UpdateNode(ctx context.Context, name string, node config.NodeConfig) (config.NodeConfig, error)
	DeleteNode(ctx context.Context, name string) error
	SetNodeEnabled(ctx context.Context, name string, enabled bool) error
	TriggerReload(ctx context.Context) error
}

// BatchNodeCreator is the optional bulk-write port. A runtime that implements it
// takes one lock, validates the whole batch (including building each outbound
// once) and writes it in one transaction; callers fall back to per-node
// CreateNode when it is absent, which is what the lightweight test managers do.
type BatchNodeCreator interface {
	CreateNodes(ctx context.Context, nodes []config.NodeConfig, dryRun bool) (NodeBatchResult, error)
}

// NodeBatchResult is the per-row outcome of a batch creation.
//
// Planned holds the normalized rows that were (or, for a dry run, would be)
// written. Created is the subset that actually reached the store. Failed rows
// carry a reason suitable for the import report.
type NodeBatchResult struct {
	Planned []config.NodeConfig `json:"planned,omitempty"`
	Created []config.NodeConfig `json:"created,omitempty"`
	Failed  []NodeCreateFailure `json:"failed,omitempty"`
}

// NodeCreateFailure describes one row the batch refused to create. Err keeps
// the typed cause so the HTTP layer can apply the same status mapping it uses
// for single-node creates; Reason is the operator-facing text.
type NodeCreateFailure struct {
	Index  int    `json:"index"`
	Name   string `json:"name,omitempty"`
	URI    string `json:"uri,omitempty"`
	Reason string `json:"reason"`
	Err    error  `json:"-"`
}

// IDNodeManager is the optional ID-addressed CRUD port. Node names are not
// unique (two subscriptions may ship the same display name), so the management
// UI addresses nodes by ID; the name-based methods stay as a compatibility
// layer for callers that only have a name.
type IDNodeManager interface {
	UpdateNodeByID(ctx context.Context, id int64, node config.NodeConfig) (config.NodeConfig, error)
	DeleteNodeByID(ctx context.Context, id int64) error
	SetNodeEnabledByID(ctx context.Context, id int64, enabled bool) error
}

type GroupRuntimeStatus struct {
	Status string `json:"runtime_status"`
	Error  string `json:"runtime_error,omitempty"`
}

// GroupRuntimeManager is implemented by runtimes that can apply one group
// without reloading every listener. The HTTP layer keeps a TriggerReload
// fallback for lightweight test managers and older integrations.
type GroupRuntimeManager interface {
	ApplyGroupRuntime(ctx context.Context, before, after *store.GroupPool) error
	ActivateGroupMember(ctx context.Context, groupID, nodeID int64) error
	GroupRuntimeStatus(groupID int64) GroupRuntimeStatus
}

// GroupMembershipManager is implemented by runtimes that can react to a node's
// facts changing — its tags or its landing region — by rebuilding only the
// groups whose member set moved. Callers keep a TriggerReload fallback, which is
// correct but rebuilds every listener in the process.
type GroupMembershipManager interface {
	ApplyGroupMembershipChanges(ctx context.Context, changedNodeIDs []int64) error
}

// ManagedNodeConfig is the flattened API representation used by node management.
type ManagedNodeConfig struct {
	// ID is the persisted node ID. It is what a tag assignment addresses, so the
	// management UI needs it alongside the connection settings.
	ID       int64  `json:"id,omitempty"`
	Name     string `json:"name"`
	URI      string `json:"uri"`
	Port     uint16 `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Region   string `json:"region,omitempty"`
	Country  string `json:"country,omitempty"`
	// Tags is the node's projected tag set (manual ∪ auto names). It is read-only
	// here: tags are written through the tagging endpoints, not node updates.
	Tags            []string          `json:"tags,omitempty"`
	Source          config.NodeSource `json:"source,omitempty"`
	Disabled        bool              `json:"disabled,omitempty"`
	SubscriptionIDs []int64           `json:"subscription_ids"`
}

// Sentinel errors for node operations.
var (
	ErrNodeNotFound = errors.New("节点不存在")
	ErrNodeConflict = errors.New("节点名称或端口已存在")
	ErrInvalidNode  = errors.New("无效的节点配置")
)

type NodeDuplicateError struct {
	ExistingID   int64
	ExistingName string
}

func (e *NodeDuplicateError) Error() string {
	return fmt.Sprintf("节点连接已存在: %s (ID %d)", e.ExistingName, e.ExistingID)
}

func (e *NodeDuplicateError) Unwrap() error { return ErrNodeConflict }

// ErrRefreshInProgress reports that another subscription refresh holds the
// single refresh slot. HTTP maps it to 409 rather than a server error.
var ErrRefreshInProgress = errors.New("刷新正在进行")

// SubscriptionRefresher interface for subscription manager.
type SubscriptionRefresher interface {
	RefreshNow() error
	Status() SubscriptionStatus
	List(ctx context.Context) ([]store.Subscription, error)
	Get(ctx context.Context, id int64) (*store.Subscription, error)
	Create(ctx context.Context, subscription store.Subscription) (*store.Subscription, error)
	Update(ctx context.Context, id int64, subscription store.Subscription) (*store.Subscription, error)
	Delete(ctx context.Context, id int64) error
	SetEnabled(ctx context.Context, id int64, enabled bool) error
	ActivateExclusive(ctx context.Context, id int64) error
	RefreshOne(ctx context.Context, id int64) error
	Nodes(ctx context.Context, id int64) ([]store.SubscriptionNode, error)
	ApplyConfig(cfg *config.Config)
}

// SubscriptionStatus represents subscription refresh status.
type SubscriptionStatus struct {
	Enabled           bool      `json:"enabled"`           // Whether auto-refresh is enabled in config
	HasSubscriptions  bool      `json:"has_subscriptions"` // Whether subscription URLs are configured
	LastRefresh       time.Time `json:"last_refresh"`
	NextRefresh       time.Time `json:"next_refresh"`
	NodeCount         int       `json:"node_count"`
	LastError         string    `json:"last_error,omitempty"`
	RefreshCount      int       `json:"refresh_count"`
	IsRefreshing      bool      `json:"is_refreshing"`
	RefreshingIDs     []int64   `json:"refreshing_ids,omitempty"` // subscriptions fetching right now
	NodesModified     bool      `json:"nodes_modified"` // True if nodes were modified since last refresh
	Parsed            int       `json:"parsed"`
	Created           int       `json:"created"`
	Updated           int       `json:"updated"`
	DuplicatesSkipped int       `json:"duplicates_skipped"`
	Invalid           int       `json:"invalid"`
}
