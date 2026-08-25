package monitor

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/geoip"
	"easy_proxies/internal/group"
	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/nodecodec"
	"easy_proxies/internal/store"
	"easy_proxies/internal/unlock"

	"golang.org/x/sync/semaphore"
)

//go:embed assets/*
var embeddedFS embed.FS

// Session represents a user session with expiration.
type Session struct {
	Token     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// NodeManager exposes config node CRUD and reload operations.
type NodeManager interface {
	ListConfigNodes(ctx context.Context, subscriptionID *int64) ([]ManagedNodeConfig, error)
	CreateNode(ctx context.Context, node config.NodeConfig) (config.NodeConfig, error)
	UpdateNode(ctx context.Context, name string, node config.NodeConfig) (config.NodeConfig, error)
	DeleteNode(ctx context.Context, name string) error
	SetNodeEnabled(ctx context.Context, name string, enabled bool) error
	TriggerReload(ctx context.Context) error
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

// ManagedNodeConfig is the flattened API representation used by node management.
type ManagedNodeConfig struct {
	Name            string            `json:"name"`
	URI             string            `json:"uri"`
	Port            uint16            `json:"port"`
	Username        string            `json:"username,omitempty"`
	Password        string            `json:"password,omitempty"`
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

type ApplyPlan struct {
	NeedReload  bool     `json:"need_reload"`
	NeedRestart bool     `json:"need_restart"`
	Applied     []string `json:"applied"`
	Pending     []string `json:"pending"`
}

type SettingsUpdateResult struct {
	Message     string   `json:"message"`
	Saved       bool     `json:"saved"`
	NeedReload  bool     `json:"need_reload"`
	NeedRestart bool     `json:"need_restart"`
	Reloaded    bool     `json:"reloaded"`
	ReloadError string   `json:"reload_error,omitempty"`
	Applied     []string `json:"applied"`
	Pending     []string `json:"pending"`
}

type settingsValidationError struct{ err error }

func (e settingsValidationError) Error() string { return e.err.Error() }

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
	NodesModified     bool      `json:"nodes_modified"` // True if nodes were modified since last refresh
	Parsed            int       `json:"parsed"`
	Created           int       `json:"created"`
	Updated           int       `json:"updated"`
	DuplicatesSkipped int       `json:"duplicates_skipped"`
	Invalid           int       `json:"invalid"`
}

// Server exposes HTTP endpoints for monitoring.
type Server struct {
	cfg    Config
	cfgMu  sync.RWMutex   // protects cfgSrc pointer assignment and local cfg fields
	cfgSrc *config.Config // 可持久化的配置对象; fields protected by cfgSrc.mu
	mgr    *Manager
	srv    *http.Server
	logger *log.Logger
	store  store.Store // 数据存储

	// Session management
	sessionMu  sync.RWMutex
	sessions   map[string]*Session
	sessionTTL time.Duration

	// Concurrency control
	probeSem            *semaphore.Weighted
	groupMutationMu     sync.Mutex
	groupOperationLocks sync.Map // map[int64]*sync.Mutex

	// Lifecycle
	done chan struct{} // closed on Shutdown to stop background goroutines

	subRefresher   SubscriptionRefresher
	nodeMgr        NodeManager
	geoLookup      *geoip.Lookup // optional, used by unlock checks for IP purity
	geoLookupMu    sync.Mutex    // protects lazy open/reload of geoLookup
	geoLookupPath  string        // path the cached geoLookup was opened from
	geoLookupMtime time.Time     // mtime of the db file when geoLookup was opened
}

// NewServer constructs a server; it can be nil when disabled.
func NewServer(cfg Config, mgr *Manager, logger *log.Logger) *Server {
	if !cfg.Enabled || mgr == nil {
		return nil
	}
	if logger == nil {
		logger = log.Default()
	}

	// Calculate max concurrent probes
	maxConcurrentProbes := int64(runtime.NumCPU() * 4)
	if maxConcurrentProbes < 10 {
		maxConcurrentProbes = 10
	}

	s := &Server{
		cfg:        cfg,
		mgr:        mgr,
		logger:     logger,
		sessions:   make(map[string]*Session),
		sessionTTL: 24 * time.Hour,
		probeSem:   semaphore.NewWeighted(maxConcurrentProbes),
		done:       make(chan struct{}),
	}

	// Start session cleanup goroutine
	go s.cleanupExpiredSessions()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth", s.handleAuth)
	mux.HandleFunc("/api/settings", s.withAuth(s.handleSettings))
	mux.HandleFunc("/api/nodes", s.withAuth(s.handleNodes))
	mux.HandleFunc("/api/nodes/config", s.withAuth(s.handleConfigNodes))
	mux.HandleFunc("/api/nodes/config/batch-toggle", s.withAuth(s.handleConfigNodesBatchToggle))
	mux.HandleFunc("/api/nodes/config/batch-delete", s.withAuth(s.handleConfigNodesBatchDelete))
	mux.HandleFunc("/api/nodes/config/", s.withAuth(s.handleConfigNodeItem))
	mux.HandleFunc("/api/nodes/probe-all", s.withAuth(s.handleProbeAll))
	mux.HandleFunc("/api/nodes/unlock-all", s.withAuth(s.handleUnlockAll))
	mux.HandleFunc("/api/nodes/unlock-results", s.withAuth(s.handleUnlockResults))
	mux.HandleFunc("/api/nodes/traffic/stream", s.withAuth(s.handleTrafficStream))
	mux.HandleFunc("/api/nodes/", s.withAuth(s.handleNodeAction))
	mux.HandleFunc("/api/debug", s.withAuth(s.handleDebug))
	mux.HandleFunc("/api/debug/stream", s.withAuth(s.handleDebugStream))
	mux.HandleFunc("/api/export", s.withAuth(s.handleExport))
	mux.HandleFunc("/api/import", s.withAuth(s.handleImport))
	mux.HandleFunc("/api/subscription/status", s.withAuth(s.handleSubscriptionStatus))
	mux.HandleFunc("/api/subscription/refresh", s.withAuth(s.handleSubscriptionRefresh))
	mux.HandleFunc("/api/subscriptions", s.withAuth(s.handleSubscriptions))
	mux.HandleFunc("/api/subscriptions/", s.withAuth(s.handleSubscriptionItem))
	mux.HandleFunc("/api/reload", s.withAuth(s.handleReload))
	mux.HandleFunc("/api/groups", s.withAuth(s.handleGroups))
	mux.HandleFunc("/api/groups/", s.withAuth(s.handleGroupItem))
	mux.HandleFunc("/sub/", s.handleGroupSubscription)

	// GeoIP database management
	mux.HandleFunc("/api/geoip/status", s.withAuth(s.handleGeoipStatus))
	mux.HandleFunc("/api/geoip/download", s.withAuth(s.handleGeoipDownload))
	mux.HandleFunc("/api/geoip/update", s.withAuth(s.handleGeoipUpdate))

	// Default handler for static assets (React App)
	mux.HandleFunc("/", s.handleIndex)
	s.srv = &http.Server{Addr: cfg.Listen, Handler: mux}
	return s
}

// SetSubscriptionRefresher sets the subscription refresher for API endpoints.
func (s *Server) SetSubscriptionRefresher(sr SubscriptionRefresher) {
	if s != nil {
		s.subRefresher = sr
	}
}

// SetNodeManager enables config-node CRUD endpoints.
func (s *Server) SetNodeManager(nm NodeManager) {
	if s != nil {
		s.nodeMgr = nm
	}
}

// SetGeoipLookup binds the runtime GeoIP lookup used by unlock checks to
// classify the node's exit IP. May be nil (GeoIP disabled); unlock checks
// then fall back to the trace endpoint's coarse loc field.
func (s *Server) SetGeoipLookup(l *geoip.Lookup) {
	if s != nil {
		s.geoLookup = l
	}
}

// geoipLookupForCheck returns a GeoIP lookup suitable for an unlock check.
// It first prefers the runtime lookup injected via SetGeoipLookup (kept live
// by the builder); when none is available it lazily opens one from the
// configured database path, re-opening if the on-disk file changed (e.g.
// after the user downloaded/updated the IP library via the WebUI).
func (s *Server) geoipLookupForCheck() *geoip.Lookup {
	if s != nil && s.geoLookup != nil && s.geoLookup.IsEnabled() {
		return s.geoLookup
	}
	if s == nil {
		return nil
	}
	dbPath := s.geoipDatabasePath()
	if dbPath == "" || !s.geoipEnabled() {
		return nil
	}

	s.geoLookupMu.Lock()
	defer s.geoLookupMu.Unlock()

	st, err := os.Stat(dbPath)
	if err != nil {
		return nil
	}
	mtime := st.ModTime()
	if s.geoLookup != nil && s.geoLookupPath == dbPath && !mtime.After(s.geoLookupMtime) {
		return s.geoLookup
	}

	// Reopen: close the stale lookup and open a fresh one bound to dbPath.
	if s.geoLookup != nil {
		s.geoLookup.Close()
		s.geoLookup = nil
	}
	l, err := geoip.New(dbPath)
	if err != nil || l == nil {
		return nil
	}
	s.geoLookup = l
	s.geoLookupPath = dbPath
	s.geoLookupMtime = mtime
	return s.geoLookup
}

// SetStore sets the data store for session persistence and other operations.
func (s *Server) SetStore(st store.Store) {
	if s != nil {
		s.store = st
	}
}

// SetConfig binds the persistable config object for settings API.
func (s *Server) SetConfig(cfg *config.Config) {
	if s == nil {
		return
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	s.cfgSrc = cfg
	if cfg != nil {
		cfg.RLock()
		s.cfg.ExternalIP = cfg.ExternalIP
		s.cfg.ProbeTarget = cfg.Management.ProbeTarget
		s.cfg.SkipCertVerify = cfg.SkipCertVerify
		cfg.RUnlock()
	}
}

// allSettingsResponse is the JSON structure for GET /api/settings.
type allSettingsResponse struct {
	// Global
	Mode           string `json:"mode"`
	LogLevel       string `json:"log_level"`
	ExternalIP     string `json:"external_ip"`
	SkipCertVerify bool   `json:"skip_cert_verify"`

	// Listener
	ListenerAddress  string `json:"listener_address"`
	ListenerPort     uint16 `json:"listener_port"`
	ListenerProtocol string `json:"listener_protocol"`
	ListenerUsername string `json:"listener_username"`
	ListenerPassword string `json:"listener_password"`

	// Multi-port
	MultiPortAddress  string `json:"multi_port_address"`
	MultiPortBasePort uint16 `json:"multi_port_base_port"`
	MultiPortProtocol string `json:"multi_port_protocol"`
	MultiPortUsername string `json:"multi_port_username"`
	MultiPortPassword string `json:"multi_port_password"`

	// Pool
	PoolMode              string `json:"pool_mode"`
	PoolFailureThreshold  int    `json:"pool_failure_threshold"`
	PoolBlacklistDuration string `json:"pool_blacklist_duration"`

	// Management
	ManagementEnabled             bool   `json:"management_enabled"`
	ManagementListen              string `json:"management_listen"`
	ManagementProbeTarget         string `json:"management_probe_target"`
	ManagementPassword            string `json:"management_password"`
	ManagementHealthCheckInterval string `json:"management_health_check_interval"`

	// Subscription refresh
	SubRefreshEnabled            bool   `json:"sub_refresh_enabled"`
	SubRefreshInterval           string `json:"sub_refresh_interval"`
	SubRefreshTimeout            string `json:"sub_refresh_timeout"`
	SubRefreshHealthCheckTimeout string `json:"sub_refresh_health_check_timeout"`
	SubRefreshDrainTimeout       string `json:"sub_refresh_drain_timeout"`
	SubRefreshMinAvailableNodes  int    `json:"sub_refresh_min_available_nodes"`

	// GeoIP
	GeoIPEnabled            bool   `json:"geoip_enabled"`
	GeoIPDatabasePath       string `json:"geoip_database_path"`
	GeoIPAutoUpdateEnabled  bool   `json:"geoip_auto_update_enabled"`
	GeoIPAutoUpdateInterval string `json:"geoip_auto_update_interval"`

	// Subscriptions
	Subscriptions []string `json:"subscriptions"`
}

// allSettingsRequest is the JSON structure for PUT /api/settings.
type allSettingsRequest struct {
	// Global
	Mode           string `json:"mode"`
	LogLevel       string `json:"log_level"`
	ExternalIP     string `json:"external_ip"`
	SkipCertVerify bool   `json:"skip_cert_verify"`

	// Listener
	ListenerAddress  string `json:"listener_address"`
	ListenerPort     uint16 `json:"listener_port"`
	ListenerProtocol string `json:"listener_protocol"`
	ListenerUsername string `json:"listener_username"`
	ListenerPassword string `json:"listener_password"`

	// Multi-port
	MultiPortAddress  string `json:"multi_port_address"`
	MultiPortBasePort uint16 `json:"multi_port_base_port"`
	MultiPortProtocol string `json:"multi_port_protocol"`
	MultiPortUsername string `json:"multi_port_username"`
	MultiPortPassword string `json:"multi_port_password"`

	// Pool
	PoolMode              string `json:"pool_mode"`
	PoolFailureThreshold  int    `json:"pool_failure_threshold"`
	PoolBlacklistDuration string `json:"pool_blacklist_duration"`

	// Management
	ManagementEnabled             *bool  `json:"management_enabled"`
	ManagementListen              string `json:"management_listen"`
	ManagementProbeTarget         string `json:"management_probe_target"`
	ManagementPassword            string `json:"management_password"`
	ManagementHealthCheckInterval string `json:"management_health_check_interval"`

	// Subscription refresh
	SubRefreshEnabled            bool   `json:"sub_refresh_enabled"`
	SubRefreshInterval           string `json:"sub_refresh_interval"`
	SubRefreshTimeout            string `json:"sub_refresh_timeout"`
	SubRefreshHealthCheckTimeout string `json:"sub_refresh_health_check_timeout"`
	SubRefreshDrainTimeout       string `json:"sub_refresh_drain_timeout"`
	SubRefreshMinAvailableNodes  int    `json:"sub_refresh_min_available_nodes"`

	// GeoIP
	GeoIPEnabled            bool   `json:"geoip_enabled"`
	GeoIPDatabasePath       string `json:"geoip_database_path"`
	GeoIPAutoUpdateEnabled  bool   `json:"geoip_auto_update_enabled"`
	GeoIPAutoUpdateInterval string `json:"geoip_auto_update_interval"`

	// Subscriptions
	Subscriptions []string `json:"subscriptions"`
}

// getAllSettings reads all config fields into a flat response (thread-safe).
func (s *Server) getAllSettings() allSettingsResponse {
	s.cfgMu.RLock()
	c := s.cfgSrc
	s.cfgMu.RUnlock()

	if c == nil {
		return allSettingsResponse{}
	}

	c.RLock()
	defer c.RUnlock()
	mgmtEnabled := true
	if c.Management.Enabled != nil {
		mgmtEnabled = *c.Management.Enabled
	}

	return allSettingsResponse{
		Mode:           c.Mode,
		LogLevel:       c.LogLevel,
		ExternalIP:     c.ExternalIP,
		SkipCertVerify: c.SkipCertVerify,

		ListenerAddress:  c.Listener.Address,
		ListenerPort:     c.Listener.Port,
		ListenerProtocol: c.Listener.Protocol,
		ListenerUsername: c.Listener.Username,
		ListenerPassword: c.Listener.Password,

		MultiPortAddress:  c.MultiPort.Address,
		MultiPortBasePort: c.MultiPort.BasePort,
		MultiPortProtocol: c.MultiPort.Protocol,
		MultiPortUsername: c.MultiPort.Username,
		MultiPortPassword: c.MultiPort.Password,

		PoolMode:              c.Pool.Mode,
		PoolFailureThreshold:  c.Pool.FailureThreshold,
		PoolBlacklistDuration: c.Pool.BlacklistDuration.String(),

		ManagementEnabled:             mgmtEnabled,
		ManagementListen:              c.Management.Listen,
		ManagementProbeTarget:         c.Management.ProbeTarget,
		ManagementPassword:            c.Management.Password,
		ManagementHealthCheckInterval: c.Management.HealthCheckInterval.String(),

		SubRefreshEnabled:            c.SubscriptionRefresh.Enabled,
		SubRefreshInterval:           c.SubscriptionRefresh.Interval.String(),
		SubRefreshTimeout:            c.SubscriptionRefresh.Timeout.String(),
		SubRefreshHealthCheckTimeout: c.SubscriptionRefresh.HealthCheckTimeout.String(),
		SubRefreshDrainTimeout:       c.SubscriptionRefresh.DrainTimeout.String(),
		SubRefreshMinAvailableNodes:  c.SubscriptionRefresh.MinAvailableNodes,

		GeoIPEnabled:            c.GeoIP.Enabled,
		GeoIPDatabasePath:       c.GeoIP.DatabasePath,
		GeoIPAutoUpdateEnabled:  c.GeoIP.AutoUpdateEnabled,
		GeoIPAutoUpdateInterval: c.GeoIP.AutoUpdateInterval.String(),

		Subscriptions: c.Subscriptions,
	}
}

// updateAllSettings applies all settings from request and persists to config file.
func (s *Server) updateAllSettings(ctx context.Context, req allSettingsRequest) (SettingsUpdateResult, error) {
	// Validate request before applying
	if err := config.ValidateSettingsRequest(
		req.Mode, req.ListenerPort, req.MultiPortBasePort,
		req.ListenerProtocol, req.MultiPortProtocol,
		req.PoolBlacklistDuration, req.SubRefreshInterval, req.SubRefreshTimeout,
		req.SubRefreshHealthCheckTimeout, req.SubRefreshDrainTimeout,
		req.GeoIPAutoUpdateInterval, req.ManagementHealthCheckInterval,
	); err != nil {
		return SettingsUpdateResult{}, settingsValidationError{fmt.Errorf("参数验证失败: %w", err)}
	}

	s.cfgMu.RLock()
	c := s.cfgSrc
	s.cfgMu.RUnlock()

	if c == nil {
		return SettingsUpdateResult{}, errors.New("配置存储未初始化")
	}
	old := c.Snapshot()
	updated := old.Clone()
	c = updated

	// Global
	c.Mode = req.Mode
	c.LogLevel = req.LogLevel
	c.ExternalIP = strings.TrimSpace(req.ExternalIP)
	c.SkipCertVerify = req.SkipCertVerify

	// Listener
	c.Listener.Address = req.ListenerAddress
	c.Listener.Port = req.ListenerPort
	if p, err := config.NormalizeInboundProtocol(req.ListenerProtocol); err == nil {
		c.Listener.Protocol = p
	}
	c.Listener.Username = req.ListenerUsername
	c.Listener.Password = req.ListenerPassword

	// Multi-port
	c.MultiPort.Address = req.MultiPortAddress
	c.MultiPort.BasePort = req.MultiPortBasePort
	if p, err := config.NormalizeInboundProtocol(req.MultiPortProtocol); err == nil {
		c.MultiPort.Protocol = p
	}
	c.MultiPort.Username = req.MultiPortUsername
	c.MultiPort.Password = req.MultiPortPassword

	// Pool
	c.Pool.Mode = req.PoolMode
	c.Pool.FailureThreshold = req.PoolFailureThreshold
	if d, err := time.ParseDuration(req.PoolBlacklistDuration); err == nil && d > 0 {
		c.Pool.BlacklistDuration = d
	}

	// Management
	if req.ManagementEnabled != nil {
		c.Management.Enabled = req.ManagementEnabled
	}
	c.Management.Listen = req.ManagementListen
	c.Management.ProbeTarget = strings.TrimSpace(req.ManagementProbeTarget)
	c.Management.Password = req.ManagementPassword
	if d, err := time.ParseDuration(req.ManagementHealthCheckInterval); err == nil && d > 0 {
		c.Management.HealthCheckInterval = d
	}

	// Subscription refresh
	c.SubscriptionRefresh.Enabled = req.SubRefreshEnabled
	if d, err := time.ParseDuration(req.SubRefreshInterval); err == nil && d > 0 {
		c.SubscriptionRefresh.Interval = d
	}
	if d, err := time.ParseDuration(req.SubRefreshTimeout); err == nil && d > 0 {
		c.SubscriptionRefresh.Timeout = d
	}
	if d, err := time.ParseDuration(req.SubRefreshHealthCheckTimeout); err == nil && d > 0 {
		c.SubscriptionRefresh.HealthCheckTimeout = d
	}
	if d, err := time.ParseDuration(req.SubRefreshDrainTimeout); err == nil && d > 0 {
		c.SubscriptionRefresh.DrainTimeout = d
	}
	c.SubscriptionRefresh.MinAvailableNodes = req.SubRefreshMinAvailableNodes

	// GeoIP
	c.GeoIP.Enabled = req.GeoIPEnabled
	c.GeoIP.DatabasePath = req.GeoIPDatabasePath
	c.GeoIP.AutoUpdateEnabled = req.GeoIPAutoUpdateEnabled
	if d, err := time.ParseDuration(req.GeoIPAutoUpdateInterval); err == nil && d > 0 {
		c.GeoIP.AutoUpdateInterval = d
	}

	// Subscriptions
	// Legacy subscriptions are managed by the subscription API.
	c.Subscriptions = append([]string(nil), old.Subscriptions...)
	plan := settingsApplyPlan(old, c)

	if err := c.SaveSettings(); err != nil {
		return SettingsUpdateResult{}, fmt.Errorf("保存配置失败: %w", err)
	}

	s.cfgMu.Lock()
	s.cfgSrc = c
	s.cfg.ExternalIP = c.ExternalIP
	s.cfg.ProbeTarget = c.Management.ProbeTarget
	s.cfg.SkipCertVerify = c.SkipCertVerify
	s.cfg.Password = c.Management.Password
	s.cfgMu.Unlock()

	if s.mgr != nil {
		if err := s.mgr.UpdateProbeTarget(c.Management.ProbeTarget); err != nil {
			s.logger.Printf("更新探测目标失败: %v", err)
		}
		s.mgr.SetHealthCheckInterval(c.Management.HealthCheckInterval)
	}
	if s.subRefresher != nil {
		s.subRefresher.ApplyConfig(c)
	}
	if s.store != nil && (old.SubscriptionRefresh.Interval != c.SubscriptionRefresh.Interval || old.SubscriptionRefresh.Timeout != c.SubscriptionRefresh.Timeout) {
		interval, timeout := 0, 0
		if old.SubscriptionRefresh.Interval != c.SubscriptionRefresh.Interval {
			interval = int(c.SubscriptionRefresh.Interval.Seconds())
		}
		if old.SubscriptionRefresh.Timeout != c.SubscriptionRefresh.Timeout {
			timeout = int(c.SubscriptionRefresh.Timeout.Seconds())
		}
		if err := s.store.UpdateAllSubscriptionRefreshSettings(ctx, interval, timeout); err != nil {
			s.logger.Printf("批量更新订阅刷新设置失败: %v", err)
		}
	}
	result := SettingsUpdateResult{Saved: true, NeedReload: plan.NeedReload, NeedRestart: plan.NeedRestart, Applied: plan.Applied, Pending: plan.Pending}
	if plan.NeedReload && s.nodeMgr != nil {
		if err := s.nodeMgr.TriggerReload(ctx); err != nil {
			result.ReloadError = err.Error()
		} else {
			result.NeedReload, result.Reloaded = false, true
			result.Applied = append(result.Applied, "runtime_config")
			result.Pending = removeString(result.Pending, "runtime_config")
		}
	}
	if result.NeedRestart {
		result.Message = "设置已保存；管理服务启用状态或监听地址变更，需重启进程生效"
	} else if result.NeedReload {
		result.Message = "设置已保存；运行时重载未完成"
	} else {
		result.Message = "设置已保存并应用"
	}
	return result, nil
}

func settingsApplyPlan(old, updated *config.Config) ApplyPlan {
	plan := ApplyPlan{Applied: []string{}, Pending: []string{}}
	plan.NeedRestart = old.ManagementEnabled() != updated.ManagementEnabled() || old.Management.Listen != updated.Management.Listen
	a, b := old.Clone(), updated.Clone()
	a.Management.Enabled, b.Management.Enabled = nil, nil
	a.Management.Listen, b.Management.Listen = "", ""
	a.Management.Password, b.Management.Password = "", ""
	a.Management.ProbeTarget, b.Management.ProbeTarget = "", ""
	a.Management.HealthCheckInterval, b.Management.HealthCheckInterval = 0, 0
	a.ExternalIP, b.ExternalIP = "", ""
	a.SubscriptionRefresh.Enabled, b.SubscriptionRefresh.Enabled = false, false
	a.Subscriptions, b.Subscriptions = nil, nil
	plan.NeedReload = !reflect.DeepEqual(a, b)
	if plan.NeedReload {
		plan.Pending = append(plan.Pending, "runtime_config")
	}
	if plan.NeedRestart {
		plan.Pending = append(plan.Pending, "management_server")
	}
	if old.Management.Password != updated.Management.Password {
		plan.Applied = append(plan.Applied, "management_password")
	}
	if old.Management.ProbeTarget != updated.Management.ProbeTarget {
		plan.Applied = append(plan.Applied, "management_probe_target")
	}
	if old.Management.HealthCheckInterval != updated.Management.HealthCheckInterval {
		plan.Applied = append(plan.Applied, "management_health_check_interval")
	}
	if old.ExternalIP != updated.ExternalIP {
		plan.Applied = append(plan.Applied, "external_ip")
	}
	if old.SubscriptionRefresh.Enabled != updated.SubscriptionRefresh.Enabled {
		plan.Applied = append(plan.Applied, "sub_refresh_enabled")
	}
	return plan
}

func removeString(values []string, target string) []string {
	result := values[:0]
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}

// Start launches the HTTP server.
func (s *Server) Start(ctx context.Context) {
	if s == nil || s.srv == nil {
		return
	}
	s.logger.Printf("Starting monitor server on %s", s.cfg.Listen)
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("❌ Monitor server error: %v", err)
		}
	}()
	// Give server a moment to start and check for immediate errors
	time.Sleep(100 * time.Millisecond)
	s.logger.Printf("✅ Monitor server started on http://%s", s.cfg.Listen)

	go func() {
		<-ctx.Done()
		s.Shutdown(context.Background())
	}()
}

// Shutdown stops the server gracefully.
func (s *Server) Shutdown(ctx context.Context) {
	if s == nil || s.srv == nil {
		return
	}
	// Signal background goroutines to stop
	select {
	case <-s.done:
		// already closed
	default:
		close(s.done)
	}
	_ = s.srv.Shutdown(ctx)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// First check if this is an API request that wasn't matched
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Try to serve static file
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		// Clean the path to avoid directory traversal
		cleanPath := "assets" + r.URL.Path
		_, err := embeddedFS.Open(cleanPath)
		if err == nil {
			// If file exists, serve it
			r.URL.Path = cleanPath // rewrite path for FileServer
			http.FileServer(http.FS(embeddedFS)).ServeHTTP(w, r)
			return
		}
	}

	// For root or non-existent files (SPA routing), serve index.html
	data, err := embeddedFS.ReadFile("assets/index.html")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// 返回所有注册节点，让前端根据状态过滤展示
	allNodes := s.mgr.Snapshot()
	totalNodes := len(allNodes)

	// Calculate region statistics and traffic totals
	regionStats := make(map[string]int)
	regionHealthy := make(map[string]int)
	for _, snap := range allNodes {
		region := snap.Region
		if region == "" {
			region = "other"
		}
		regionStats[region]++
		// Count healthy nodes per region
		if snap.InitialCheckDone && snap.Available && !snap.Blacklisted {
			regionHealthy[region]++
		}
	}

	traffic := s.mgr.TrafficSummary(false)

	payload := map[string]any{
		"nodes":           allNodes,
		"total_nodes":     totalNodes,
		"total_upload":    traffic.TotalUpload,
		"total_download":  traffic.TotalDownload,
		"upload_speed":    traffic.UploadSpeed,
		"download_speed":  traffic.DownloadSpeed,
		"traffic_sampled": traffic.SampledAt,
		"region_stats":    regionStats,
		"region_healthy":  regionHealthy,
	}
	writeJSON(w, payload)
}

func (s *Server) handleDebug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	snapshots := s.mgr.Snapshot()
	var totalCalls, totalSuccess int64
	debugNodes := make([]map[string]any, 0, len(snapshots))
	for _, snap := range snapshots {
		totalCalls += snap.SuccessCount + int64(snap.FailureCount)
		totalSuccess += snap.SuccessCount
		debugNodes = append(debugNodes, map[string]any{
			"tag":                snap.Tag,
			"name":               snap.Name,
			"mode":               snap.Mode,
			"port":               snap.Port,
			"failure_count":      snap.FailureCount,
			"success_count":      snap.SuccessCount,
			"active_connections": snap.ActiveConnections,
			"last_latency_ms":    snap.LastLatencyMs,
			"last_success":       snap.LastSuccess,
			"last_failure":       snap.LastFailure,
			"last_error":         snap.LastError,
			"blacklisted":        snap.Blacklisted,
			"total_upload":       snap.TotalUpload,
			"total_download":     snap.TotalDownload,
			"timeline":           snap.Timeline,
		})
	}
	var successRate float64
	if totalCalls > 0 {
		successRate = float64(totalSuccess) / float64(totalCalls) * 100
	}
	writeJSON(w, map[string]any{
		"nodes":         debugNodes,
		"total_calls":   totalCalls,
		"total_success": totalSuccess,
		"success_rate":  successRate,
	})
}

func (s *Server) handleDebugStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	events, unsubscribe := s.mgr.SubscribeDebugLogs()
	defer unsubscribe()
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case event := <-events:
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err = fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handleNodeAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/nodes/"), "/")
	if len(parts) < 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	tag := parts[0]
	if tag == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch action {
	case "probe":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		latency, err := s.mgr.Probe(ctx, tag)
		if err != nil {
			writeAPIError(w, http.StatusBadGateway, err.Error())
			return
		}
		latencyMs := latency.Milliseconds()
		if latencyMs == 0 && latency > 0 {
			latencyMs = 1 // Round up sub-millisecond latencies to 1ms
		}
		writeJSON(w, map[string]any{"message": "探测成功", "latency_ms": latencyMs})
	case "release":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := s.mgr.Release(tag); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"message": "已解除拉黑"})
	case "speedtest":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		dialer, err := s.mgr.DialerFor(tag)
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			flusher.Flush()
			return
		}

		// Wrap dialer slightly to fit signature
		wrappedDialer := func(ctx context.Context, network, addr string) (interface{}, error) {
			return dialer(ctx, network, addr)
		}

		runner := &SpeedtestRunner{}
		err = runner.Run(r.Context(), wrappedDialer, func(mbps float64, isDone bool) {
			if isDone {
				fmt.Fprintf(w, "event: done\ndata: {\"mbps\": %.2f}\n\n", mbps)
			} else {
				fmt.Fprintf(w, "event: progress\ndata: {\"mbps\": %.2f}\n\n", mbps)
			}
			flusher.Flush()
		})
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			flusher.Flush()
		}
	case "unlock":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Unlock checks issue several HTTP requests through the node, so allow
		// more time than a single latency probe.
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		dialer, err := s.mgr.DialerFor(tag)
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		name := tag
		snap := s.mgr.SnapshotForTag(tag)
		if snap != nil && snap.Name != "" {
			name = snap.Name
		}
		result, err := unlock.Check(ctx, unlock.DialFunc(dialer), tag, name, s.geoipLookupForCheck(), 25*time.Second)
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		s.persistUnlockResult(snap, result)
		writeJSON(w, result)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// handleProbeAll probes all nodes in batches and returns results via SSE
func (s *Server) handleProbeAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	// Get all nodes
	snapshots := s.mgr.Snapshot()
	total := len(snapshots)
	if total == 0 {
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"complete","total":0,"success":0,"failed":0}`)
		flusher.Flush()
		return
	}

	// Send start event
	fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"type":"start","total":%d}`, total))
	flusher.Flush()

	// Create context with timeout
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	// Probe all nodes with semaphore control
	type probeResult struct {
		tag     string
		name    string
		latency int64
		err     string
	}
	results := make(chan probeResult, total)
	var wg sync.WaitGroup

	// Launch probes with semaphore control
	for _, snap := range snapshots {
		wg.Add(1)
		go func(snap Snapshot) {
			defer wg.Done()

			// Acquire semaphore permit
			if err := s.probeSem.Acquire(ctx, 1); err != nil {
				results <- probeResult{
					tag:  snap.Tag,
					name: snap.Name,
					err:  "probe cancelled: " + err.Error(),
				}
				return
			}
			defer s.probeSem.Release(1)

			// Execute probe
			probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
			defer probeCancel()

			latency, err := s.mgr.Probe(probeCtx, snap.Tag)
			if err != nil {
				results <- probeResult{
					tag:     snap.Tag,
					name:    snap.Name,
					latency: -1,
					err:     err.Error(),
				}
			} else {
				results <- probeResult{
					tag:     snap.Tag,
					name:    snap.Name,
					latency: latency.Milliseconds(),
					err:     "",
				}
			}
		}(snap)
	}

	// Wait for all probes to complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	successCount := 0
	failedCount := 0
	count := 0

	for result := range results {
		count++
		if result.err != "" {
			failedCount++
		} else {
			successCount++
		}

		progress := float64(count) / float64(total) * 100
		status := "success"
		if result.err != "" {
			status = "error"
		}

		eventData := fmt.Sprintf(`{"type":"progress","tag":"%s","name":"%s","latency":%d,"status":"%s","error":"%s","current":%d,"total":%d,"progress":%.1f}`,
			result.tag, result.name, result.latency, status, result.err, count, total, progress)
		fmt.Fprintf(w, "data: %s\n\n", eventData)
		flusher.Flush()
	}

	// Send complete event
	fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"type":"complete","total":%d,"success":%d,"failed":%d}`, total, successCount, failedCount))
	flusher.Flush()
}

// handleUnlockAll runs unlock checks for all nodes and streams results via SSE.
// The event sequence mirrors handleProbeAll: {"type":"start","total":N},
// repeated {"type":"progress",...} (one per node, carrying the full Result),
// and a final {"type":"complete",...}. Each progress event contains the
// node's unlock.Result serialized as the "result" field.
func (s *Server) handleUnlockAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	// Get all nodes
	snapshots := s.mgr.Snapshot()
	total := len(snapshots)
	if total == 0 {
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"complete","total":0,"success":0,"failed":0}`)
		flusher.Flush()
		return
	}

	// Send start event
	fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"type":"start","total":%d}`, total))
	flusher.Flush()

	// Unlock checks are heavier than latency probes, so allow a generous
	// overall deadline (each node can take up to ~25s).
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	type unlockResult struct {
		result *unlock.Result
		err    string
	}
	results := make(chan unlockResult, total)
	var wg sync.WaitGroup

	// Launch checks with the same semaphore used by probes to bound concurrency.
	for _, snap := range snapshots {
		wg.Add(1)
		go func(snap Snapshot) {
			defer wg.Done()

			// Acquire semaphore permit
			if err := s.probeSem.Acquire(ctx, 1); err != nil {
				results <- unlockResult{err: "unlock cancelled: " + err.Error()}
				return
			}
			defer s.probeSem.Release(1)

			// Execute unlock check
			checkCtx, checkCancel := context.WithTimeout(ctx, 60*time.Second)
			defer checkCancel()

			dialer, err := s.mgr.DialerFor(snap.Tag)
			if err != nil {
				results <- unlockResult{err: err.Error()}
				return
			}
			res, err := unlock.Check(checkCtx, unlock.DialFunc(dialer), snap.Tag, snap.Name, s.geoipLookupForCheck(), 25*time.Second)
			if err != nil {
				results <- unlockResult{err: err.Error()}
				return
			}
			s.persistUnlockResult(&snap, res)
			results <- unlockResult{result: res}
		}(snap)
	}

	// Wait for all checks to complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect and stream results
	successCount := 0
	failedCount := 0
	count := 0

	// progressEvent is the SSE payload for one completed node.
	type progressEvent struct {
		Type     string         `json:"type"`
		Tag      string         `json:"tag"`
		Name     string         `json:"name"`
		Status   string         `json:"status"`
		Error    string         `json:"error,omitempty"`
		Result   *unlock.Result `json:"result,omitempty"`
		Current  int            `json:"current"`
		Total    int            `json:"total"`
		Progress float64        `json:"progress"`
	}

	for result := range results {
		count++
		hasError := result.err != ""
		if hasError {
			failedCount++
		} else {
			successCount++
		}

		progress := float64(count) / float64(total) * 100

		ev := progressEvent{
			Type:     "progress",
			Current:  count,
			Total:    total,
			Progress: progress,
		}
		if hasError {
			ev.Status = "error"
			ev.Error = result.err
		} else {
			ev.Tag = result.result.Tag
			ev.Name = result.result.Name
			ev.Status = "success"
			ev.Result = result.result
		}

		data, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// Send complete event
	fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"type":"complete","total":%d,"success":%d,"failed":%d}`, total, successCount, failedCount))
	flusher.Flush()
}

// handleUnlockResults returns the latest persisted unlock detection result for
// every node that has one, keyed by node tag. It lets the WebUI show previously
// saved detections without re-running the checks. It is read-only and never
// touches the live probes.
func (s *Server) handleUnlockResults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.store == nil {
		writeJSON(w, map[string]any{"results": map[string]any{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	stored, err := s.store.ListUnlockResults(ctx)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}

	// unlockResultView is the JSON shape served to the WebUI. It mirrors the
	// live unlock.Result plus a checked_at timestamp, omitting the internal
	// node id and the redundant result_json blob (Services are already
	// reconstructed server-side from that blob).
	type unlockResultView struct {
		Tag       string                      `json:"tag"`
		Name      string                      `json:"name"`
		Services  []store.UnlockServiceResult `json:"services"`
		IP        store.UnlockIPInfo          `json:"ip"`
		Error     string                      `json:"error,omitempty"`
		Duration  int64                       `json:"duration_ms"`
		CheckedAt time.Time                   `json:"checked_at"`
	}

	out := make(map[string]unlockResultView, len(stored))
	for _, res := range stored {
		if res == nil || res.Tag == "" {
			continue
		}
		out[res.Tag] = unlockResultView{
			Tag:       res.Tag,
			Name:      res.Name,
			Services:  res.Services,
			IP:        res.IP,
			Error:     res.Error,
			Duration:  res.Duration,
			CheckedAt: res.CheckedAt,
		}
	}
	writeJSON(w, map[string]any{"results": out})
}

func (s *Server) handleTrafficStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	send := func(payload map[string]any) bool {
		data, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Initial snapshot
	initial := s.mgr.TrafficSummary(true)
	if !send(map[string]any{
		"type":           "traffic",
		"node_count":     initial.NodeCount,
		"total_upload":   initial.TotalUpload,
		"total_download": initial.TotalDownload,
		"upload_speed":   initial.UploadSpeed,
		"download_speed": initial.DownloadSpeed,
		"sampled_at":     initial.SampledAt,
		"nodes":          initial.Nodes,
	}) {
		return
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
			summary := s.mgr.TrafficSummary(true)
			ok := send(map[string]any{
				"type":           "traffic",
				"node_count":     summary.NodeCount,
				"total_upload":   summary.TotalUpload,
				"total_download": summary.TotalDownload,
				"upload_speed":   summary.UploadSpeed,
				"download_speed": summary.DownloadSpeed,
				"sampled_at":     summary.SampledAt,
				"nodes":          summary.Nodes,
			})
			if !ok {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

// persistUnlockResult stores an unlock.Result to the store keyed by the node ID
// resolved from the snapshot's URI (falling back to its Name). It is best-effort:
// the DB write never fails the check response or the SSE stream; persistence
// errors are only logged. It is safe to call with a nil store or nil snapshot.
func (s *Server) persistUnlockResult(snap *Snapshot, result *unlock.Result) {
	if s == nil || s.store == nil || result == nil || snap == nil {
		return
	}
	nodeID, ok := s.nodeIDForSnapshot(*snap)
	if !ok || nodeID == 0 {
		return
	}

	// Serialize the full result once and store it verbatim so the WebUI can
	// reconstruct per-service detail/region exactly as reported.
	resultJSON := ""
	if data, err := json.Marshal(result); err == nil {
		resultJSON = string(data)
	}

	stored := &store.UnlockResult{
		NodeID:     nodeID,
		Tag:        result.Tag,
		Name:       result.Name,
		Error:      result.Error,
		Duration:   result.Duration,
		CheckedAt:  time.Now().UTC(),
		ResultJSON: resultJSON,
		IP: store.UnlockIPInfo{
			IP:         result.IP.IP,
			Country:    result.IP.Country,
			ISOCode:    result.IP.ISOCode,
			Region:     result.IP.Region,
			Pure:       result.IP.Pure,
			ASN:        result.IP.ASN,
			Org:        result.IP.Org,
			IPType:     result.IP.IPType,
			UsageType:  result.IP.UsageType,
			FraudScore: result.IP.FraudScore,
			RiskLevel:  result.IP.RiskLevel,
		},
	}
	for _, svc := range result.Services {
		stored.Services = append(stored.Services, store.UnlockServiceResult{
			Name:        svc.Name,
			DisplayName: svc.DisplayName,
			Status:      svc.Status,
			Region:      svc.Region,
			Detail:      svc.Detail,
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.UpsertUnlockResult(ctx, stored); err != nil {
		s.logger.Printf("[unlock] failed to persist result for node %d (%s): %v", nodeID, snap.Tag, err)
	}

	// Auto-tagging logic
	if node, err := s.store.GetNode(ctx, nodeID); err == nil && node != nil {
		var newTags []string
		if result.IP.Pure {
			newTags = append(newTags, "原生IP")
		}
		if result.IP.RiskLevel == "High" || result.IP.RiskLevel == "Medium" {
			newTags = append(newTags, "高风险")
		}
		for _, svc := range result.Services {
			if svc.Status == unlock.StatusUnlocked {
				newTags = append(newTags, svc.DisplayName+"解锁")
			}
		}

		node.Tags = newTags
		if err := s.store.UpdateNode(ctx, node); err != nil {
			s.logger.Printf("[unlock] failed to update tags for node %d: %v", nodeID, err)
		}
	}
}

// nodeIDForSnapshot resolves a monitor Snapshot to its store node ID by URI,
// falling back to the node name, reusing the resolution pattern used by
// flushStatsToStore. Returns (0, false) when the node is not in the store.
func (s *Server) nodeIDForSnapshot(snap Snapshot) (int64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if snap.URI != "" {
		if n, err := s.store.GetNodeByURI(ctx, snap.URI); err == nil && n != nil && n.ID != 0 {
			return n.ID, true
		}
		if identity, identityErr := nodecodec.ParseURI(snap.URI); identityErr == nil {
			if n, err := s.store.GetNodeByIdentity(ctx, identity.Hash); err == nil && n != nil && n.ID != 0 {
				return n.ID, true
			}
		}
	}
	if snap.Name != "" {
		if n, err := s.store.GetNodeByName(ctx, snap.Name); err == nil && n != nil && n.ID != 0 {
			return n.ID, true
		}
	}
	return 0, false
}

// withAuth 认证中间件，如果配置了密码则需要验证
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 如果没有配置密码，直接放行
		s.cfgMu.RLock()
		password := s.cfg.Password
		s.cfgMu.RUnlock()
		if password == "" {
			next(w, r)
			return
		}

		// 检查 Cookie 中的 session token
		cookie, err := r.Cookie("session_token")
		if err == nil && s.validateSession(cookie.Value) {
			next(w, r)
			return
		}

		// 检查 Authorization header (Bearer token)
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if s.validateSession(token) {
				next(w, r)
				return
			}
		}

		// 未授权
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "未授权，请先登录"})
	}
}

// handleAuth 处理登录认证
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	password := s.cfg.Password
	s.cfgMu.RUnlock()
	// 如果没有配置密码，直接返回成功（不需要token）
	if password == "" {
		writeJSON(w, map[string]any{"message": "无需密码", "no_password": true})
		return
	}

	// GET 请求用于检查是否需要密码（供前端初始化时使用）
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"message": "需要密码", "no_password": false})
		return
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}

	// 使用 constant-time 比较防止时序攻击
	if !secureCompareStrings(req.Password, password) {
		// 添加随机延迟防止暴力破解
		time.Sleep(time.Duration(100+mathrand.Intn(200)) * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "密码错误"})
		return
	}

	// 创建新会话
	session, err := s.createSession()
	if err != nil {
		s.logger.Printf("Failed to create session: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]any{"error": "服务器错误"})
		return
	}

	// 设置 HttpOnly Cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    session.Token,
		Path:     "/",
		HttpOnly: true,
		Secure:   false, // 生产环境应启用 HTTPS 并设为 true
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.sessionTTL.Seconds()),
	})

	writeJSON(w, map[string]any{
		"message": "登录成功",
		"token":   session.Token,
	})
}

// handleExport 导出所有可用节点的原始代理 URI（如 trojan://、vless:// 等），每行一个
// 导出的内容可以直接用于导入
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// 只导出初始检查通过的可用节点
	snapshots := s.mgr.SnapshotFiltered(true)
	var lines []string

	for _, snap := range snapshots {
		// 导出节点的原始 URI
		if snap.URI == "" {
			continue
		}
		lines = append(lines, snap.URI)
	}

	// 返回纯文本，每行一个 URI
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=nodes_export.txt")
	_, _ = w.Write([]byte(strings.Join(lines, "\n")))
}

// handleImport 导入节点配置，支持以下格式（自动识别，可混合）：
//   - Clash YAML 文档（含 `proxies:` 顶级键）
//   - Clash YAML 行内列表项（`- { name: ..., type: ss, ... }`，每行一个）
//   - Base64 编码的 v2ray 订阅内容
//   - 代理 URI 列表（每行一个，如 trojan://、vless://、http://、socks5:// 等）
//   - Markdown 链接或尖括号自动链接形式的代理 URI
//
// 解析出的节点统一转成标准 URI 后入库，与导出格式互通。
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.ensureNodeManager(w) {
		return
	}

	var req struct {
		Content string `json:"content"` // 节点配置文本（URI / Clash YAML / Base64 均可）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}

	content := strings.TrimSpace(req.Content)
	if content == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "导入内容为空"})
		return
	}

	// 统一解析为节点列表（支持 Clash YAML / Base64 / URI 列表）
	parsedNodes, parseIssues, err := config.ParseImportContentReport(content)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": fmt.Sprintf("解析导入内容失败: %v", err)})
		return
	}
	existingNodes, err := s.nodeMgr.ListConfigNodes(r.Context(), nil)
	if err != nil {
		s.respondNodeError(w, err)
		return
	}
	usedNames := make(map[string]struct{}, len(existingNodes)+len(parsedNodes))
	usedIdentities := make(map[string]ManagedNodeConfig, len(existingNodes)+len(parsedNodes))
	endpointIdentities := make(map[string]map[string]string, len(existingNodes)+len(parsedNodes))
	for _, existing := range existingNodes {
		usedNames[strings.TrimSpace(existing.Name)] = struct{}{}
		if identity, identityErr := nodecodec.ParseURI(existing.URI); identityErr == nil {
			usedIdentities[identity.Hash] = existing
			if endpointIdentities[identity.EndpointKey] == nil {
				endpointIdentities[identity.EndpointKey] = map[string]string{}
			}
			endpointIdentities[identity.EndpointKey][identity.Hash] = existing.Name
		}
	}

	var imported int
	var duplicatesSkipped int
	errs := append([]string(nil), parseIssues...)
	var duplicateGroups []map[string]any
	var endpointCollisions []map[string]any
	generatedNameIndex := 1

	for i := range parsedNodes {
		node := parsedNodes[i]
		node.URI = strings.TrimSpace(node.URI)
		if node.URI == "" {
			continue
		}

		identity, identityErr := nodecodec.ParseURI(node.URI)
		if identityErr != nil {
			errs = append(errs, fmt.Sprintf("无效的代理 URI: %v", identityErr))
			continue
		}
		if existing, duplicate := usedIdentities[identity.Hash]; duplicate {
			duplicatesSkipped++
			duplicateGroups = append(duplicateGroups, map[string]any{"existing_node": existing.Name, "incoming_node": node.Name})
			continue
		}
		if identities := endpointIdentities[identity.EndpointKey]; len(identities) > 0 {
			var names []string
			for _, existingName := range identities {
				names = append(names, existingName)
			}
			sort.Strings(names)
			endpointCollisions = append(endpointCollisions, map[string]any{"endpoint": identity.EndpointKey, "existing_nodes": names, "incoming_node": node.Name})
		}

		// 名称：优先用解析结果，其次从 URI fragment 提取，最后生成唯一名称。
		name := strings.TrimSpace(node.Name)
		if name == "" {
			if parsed, perr := url.Parse(node.URI); perr == nil && parsed.Fragment != "" {
				if decoded, decErr := url.QueryUnescape(parsed.Fragment); decErr == nil {
					name = decoded
				} else {
					name = parsed.Fragment
				}
			}
		}
		if name == "" {
			for {
				name = fmt.Sprintf("imported-%d", generatedNameIndex)
				generatedNameIndex++
				if _, exists := usedNames[name]; !exists {
					break
				}
			}
		} else {
			name = uniqueImportedNodeName(name, usedNames)
		}
		usedNames[name] = struct{}{}
		node.Name = name

		if !config.IsProxyURI(node.URI) {
			errs = append(errs, fmt.Sprintf("无效的代理 URI: %s", truncateStr(node.URI, 60)))
			continue
		}

		if _, err := s.nodeMgr.CreateNode(r.Context(), node); err != nil {
			errs = append(errs, fmt.Sprintf("添加节点 %q 失败: %v", name, err))
			continue
		}
		imported++
		created := ManagedNodeConfig{Name: node.Name, URI: node.URI}
		usedIdentities[identity.Hash] = created
		if endpointIdentities[identity.EndpointKey] == nil {
			endpointIdentities[identity.EndpointKey] = map[string]string{}
		}
		endpointIdentities[identity.EndpointKey][identity.Hash] = node.Name
	}

	result := map[string]any{
		"message":            fmt.Sprintf("成功导入 %d 个节点，跳过 %d 个重复项", imported, duplicatesSkipped),
		"imported":           imported,
		"parsed":             len(parsedNodes),
		"created":            imported,
		"updated":            0,
		"duplicates_skipped": duplicatesSkipped,
		"invalid":            len(errs),
	}
	if len(duplicateGroups) > 0 {
		result["duplicate_groups"] = duplicateGroups
	}
	if len(endpointCollisions) > 0 {
		result["endpoint_collisions"] = endpointCollisions
	}
	if len(errs) > 0 {
		result["errors"] = errs
	}
	writeJSON(w, result)
}

func uniqueImportedNodeName(base string, used map[string]struct{}) string {
	if _, exists := used[base]; !exists {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
}

// truncateStr truncates a string to maxLen and appends "..." if truncated.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// handleSettings handles GET/PUT for all system settings.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp := s.getAllSettings()
		writeJSON(w, resp)
	case http.MethodPut:
		var req allSettingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "请求格式错误"})
			return
		}

		result, err := s.updateAllSettings(r.Context(), req)
		if err != nil {
			status := http.StatusInternalServerError
			var validationErr settingsValidationError
			if errors.As(err, &validationErr) {
				status = http.StatusBadRequest
			}
			w.WriteHeader(status)
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, result)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleSubscriptionStatus returns the current subscription refresh status.
// Works even when subRefresher is nil by reading config directly.
func (s *Server) handleSubscriptionStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if s.subRefresher == nil {
		// No subscription manager — read config directly to provide accurate status
		s.cfgMu.RLock()
		c := s.cfgSrc
		s.cfgMu.RUnlock()

		resp := map[string]any{
			"enabled":           false,
			"has_subscriptions": false,
			"message":           "订阅管理器未初始化",
		}
		if c != nil {
			c.RLock()
			resp["enabled"] = c.SubscriptionRefresh.Enabled
			resp["has_subscriptions"] = len(c.Subscriptions) > 0
			c.RUnlock()
		}
		writeJSON(w, resp)
		return
	}

	status := s.subRefresher.Status()
	writeJSON(w, map[string]any{
		"enabled":            status.Enabled,
		"has_subscriptions":  status.HasSubscriptions,
		"last_refresh":       status.LastRefresh,
		"next_refresh":       status.NextRefresh,
		"node_count":         status.NodeCount,
		"last_error":         status.LastError,
		"refresh_count":      status.RefreshCount,
		"is_refreshing":      status.IsRefreshing,
		"parsed":             status.Parsed,
		"created":            status.Created,
		"updated":            status.Updated,
		"duplicates_skipped": status.DuplicatesSkipped,
		"invalid":            status.Invalid,
	})
}

// handleSubscriptionRefresh triggers an immediate subscription refresh.
func (s *Server) handleSubscriptionRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if s.subRefresher == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{"error": "订阅管理器未初始化，请重启程序"})
		return
	}

	if err := s.subRefresher.RefreshNow(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}

	status := s.subRefresher.Status()
	writeJSON(w, map[string]any{
		"message":            "刷新成功",
		"node_count":         status.NodeCount,
		"parsed":             status.Parsed,
		"created":            status.Created,
		"updated":            status.Updated,
		"duplicates_skipped": status.DuplicatesSkipped,
		"invalid":            status.Invalid,
	})
}

func (s *Server) handleSubscriptions(w http.ResponseWriter, r *http.Request) {
	if s.subRefresher == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "订阅管理器未初始化")
		return
	}
	switch r.Method {
	case http.MethodGet:
		subs, err := s.subRefresher.List(r.Context())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"subscriptions": subs})
	case http.MethodPost:
		var input store.Subscription
		if err := decodeJSON(r, &input); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		sub, err := s.subRefresher.Create(r.Context(), input)
		if err != nil {
			writeAPIError(w, subscriptionErrorStatus(err), err.Error())
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, sub)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
	}
}

func (s *Server) handleSubscriptionItem(w http.ResponseWriter, r *http.Request) {
	if s.subRefresher == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "订阅管理器未初始化")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/subscriptions/")
	parts := strings.Split(path, "/")
	if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
		writeAPIError(w, http.StatusNotFound, "接口不存在")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "无效的订阅 ID")
		return
	}
	if len(parts) == 1 {
		s.handleSubscriptionCRUD(w, r, id)
		return
	}
	switch parts[1] {
	case "enabled":
		if r.Method != http.MethodPatch {
			writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
			return
		}
		var input struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decodeJSON(r, &input); err != nil || input.Enabled == nil {
			writeAPIError(w, http.StatusBadRequest, "enabled 字段必填")
			return
		}
		err = s.subRefresher.SetEnabled(r.Context(), id, *input.Enabled)
	case "activate":
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
			return
		}
		err = s.subRefresher.ActivateExclusive(r.Context(), id)
	case "refresh":
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
			return
		}
		err = s.subRefresher.RefreshOne(r.Context(), id)
		if err == nil {
			status := s.subRefresher.Status()
			writeJSON(w, map[string]any{"ok": true, "parsed": status.Parsed, "created": status.Created,
				"updated": status.Updated, "duplicates_skipped": status.DuplicatesSkipped, "invalid": status.Invalid})
			return
		}
	case "nodes":
		if r.Method != http.MethodGet {
			writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
			return
		}
		var nodes []store.SubscriptionNode
		nodes, err = s.subRefresher.Nodes(r.Context(), id)
		if err == nil {
			writeJSON(w, map[string]any{"nodes": nodes})
			return
		}
	default:
		writeAPIError(w, http.StatusNotFound, "接口不存在")
		return
	}
	if err != nil {
		writeAPIError(w, subscriptionErrorStatus(err), err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSubscriptionCRUD(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodGet:
		sub, err := s.subRefresher.Get(r.Context(), id)
		if err != nil {
			writeAPIError(w, subscriptionErrorStatus(err), err.Error())
			return
		}
		writeJSON(w, sub)
	case http.MethodPut:
		var input store.Subscription
		if err := decodeJSON(r, &input); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		sub, err := s.subRefresher.Update(r.Context(), id, input)
		if err != nil {
			writeAPIError(w, subscriptionErrorStatus(err), err.Error())
			return
		}
		writeJSON(w, sub)
	case http.MethodDelete:
		if err := s.subRefresher.Delete(r.Context(), id); err != nil {
			writeAPIError(w, subscriptionErrorStatus(err), err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
	}
}

func decodeJSON(r *http.Request, dst any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("请求格式错误: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("请求只能包含一个 JSON 对象")
	}
	return nil
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"error": message})
}

func subscriptionErrorStatus(err error) int {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "不存在") || strings.Contains(message, "not found") {
		return http.StatusNotFound
	}
	if strings.Contains(message, "不能为空") || strings.Contains(message, "必须") || strings.Contains(message, "invalid") || strings.Contains(message, "unique") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// nodePayload is the JSON request body for node CRUD operations.
type nodePayload struct {
	Name     string `json:"name"`
	URI      string `json:"uri"`
	Port     uint16 `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (p nodePayload) toConfig() config.NodeConfig {
	return config.NodeConfig{
		Name:     p.Name,
		URI:      p.URI,
		Port:     p.Port,
		Username: p.Username,
		Password: p.Password,
	}
}

// handleConfigNodes handles GET (list) and POST (create) for config nodes.
func (s *Server) handleConfigNodes(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		var subscriptionID *int64
		if raw := r.URL.Query().Get("subscription_id"); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{"error": "subscription_id 必须是正整数"})
				return
			}
			subscriptionID = &id
		}
		nodes, err := s.nodeMgr.ListConfigNodes(r.Context(), subscriptionID)
		if err != nil {
			s.respondNodeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"nodes": nodes})
	case http.MethodPost:
		var payload nodePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "请求格式错误"})
			return
		}
		node, err := s.nodeMgr.CreateNode(r.Context(), payload.toConfig())
		if err != nil {
			s.respondNodeError(w, err)
			return
		}
		reloadError := s.reloadAfterGroupMutation(r.Context())
		writeJSON(w, map[string]any{"node": node, "message": "节点已添加", "reloaded": reloadError == "", "reload_error": reloadError})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleConfigNodeItem handles PUT (update) and DELETE for a specific config node.
func (s *Server) handleConfigNodeItem(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}

	namePart := strings.TrimPrefix(r.URL.Path, "/api/nodes/config/")
	nodeName, err := url.PathUnescape(namePart)
	if err != nil || nodeName == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "节点名称无效"})
		return
	}

	switch r.Method {
	case http.MethodPut:
		var payload nodePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "请求格式错误"})
			return
		}
		node, err := s.nodeMgr.UpdateNode(r.Context(), nodeName, payload.toConfig())
		if err != nil {
			s.respondNodeError(w, err)
			return
		}
		reloadError := s.reloadAfterGroupMutation(r.Context())
		writeJSON(w, map[string]any{"node": node, "message": "节点已更新", "reloaded": reloadError == "", "reload_error": reloadError})
	case http.MethodPatch:
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "请求格式错误"})
			return
		}
		if body.Enabled == nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "缺少 enabled 字段"})
			return
		}
		if err := s.nodeMgr.SetNodeEnabled(r.Context(), nodeName, *body.Enabled); err != nil {
			s.respondNodeError(w, err)
			return
		}
		action := "已启用"
		if !*body.Enabled {
			action = "已禁用"
		}
		// Auto-reload after toggle
		reloadMsg := ""
		if err := s.nodeMgr.TriggerReload(r.Context()); err != nil {
			s.logger.Printf("auto-reload after toggle failed: %v", err)
			reloadMsg = "（自动重载失败，请手动重载）"
		} else {
			reloadMsg = "（已自动重载）"
		}
		writeJSON(w, map[string]any{"message": fmt.Sprintf("节点 %s %s%s", nodeName, action, reloadMsg)})
	case http.MethodDelete:
		if err := s.nodeMgr.DeleteNode(r.Context(), nodeName); err != nil {
			s.respondNodeError(w, err)
			return
		}
		reloadError := s.reloadAfterGroupMutation(r.Context())
		writeJSON(w, map[string]any{"message": "节点已删除", "reloaded": reloadError == "", "reload_error": reloadError})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleConfigNodesBatchToggle handles batch enable/disable for multiple nodes.
func (s *Server) handleConfigNodesBatchToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.ensureNodeManager(w) {
		return
	}

	var body struct {
		Names   []string `json:"names"`
		Enabled bool     `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}
	if len(body.Names) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "节点列表为空"})
		return
	}

	var errs []string
	successCount := 0
	for _, name := range body.Names {
		if err := s.nodeMgr.SetNodeEnabled(r.Context(), name, body.Enabled); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		} else {
			successCount++
		}
	}

	action := "启用"
	if !body.Enabled {
		action = "禁用"
	}

	// Auto-reload after batch toggle
	reloadMsg := ""
	if successCount > 0 {
		if err := s.nodeMgr.TriggerReload(r.Context()); err != nil {
			s.logger.Printf("auto-reload after batch toggle failed: %v", err)
			reloadMsg = "（自动重载失败，请手动重载）"
		} else {
			reloadMsg = "（已自动重载）"
		}
	}

	result := map[string]any{
		"message": fmt.Sprintf("成功%s %d 个节点%s", action, successCount, reloadMsg),
		"success": successCount,
		"total":   len(body.Names),
	}
	if len(errs) > 0 {
		result["errors"] = errs
	}
	writeJSON(w, result)
}

// handleConfigNodesBatchDelete handles batch deletion for multiple nodes.
func (s *Server) handleConfigNodesBatchDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.ensureNodeManager(w) {
		return
	}

	var body struct {
		Names []string `json:"names"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}
	if len(body.Names) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "节点列表为空"})
		return
	}

	var errs []string
	successCount := 0
	for _, name := range body.Names {
		if err := s.nodeMgr.DeleteNode(r.Context(), name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		} else {
			successCount++
		}
	}

	// Auto-reload after batch delete
	reloadMsg := ""
	if successCount > 0 {
		if err := s.nodeMgr.TriggerReload(r.Context()); err != nil {
			s.logger.Printf("auto-reload after batch delete failed: %v", err)
			reloadMsg = "（自动重载失败，请手动重载）"
		} else {
			reloadMsg = "（已自动重载）"
		}
	}

	result := map[string]any{
		"message": fmt.Sprintf("成功删除 %d 个节点%s", successCount, reloadMsg),
		"success": successCount,
		"total":   len(body.Names),
	}
	if len(errs) > 0 {
		result["errors"] = errs
	}
	writeJSON(w, result)
}

// handleReload triggers a configuration reload.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.ensureNodeManager(w) {
		return
	}

	if err := s.nodeMgr.TriggerReload(r.Context()); err != nil {
		s.respondNodeError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"message": "重载成功，现有连接已被中断",
	})
}

func (s *Server) ensureNodeManager(w http.ResponseWriter) bool {
	if s.nodeMgr == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{"error": "节点管理未启用"})
		return false
	}
	return true
}

func (s *Server) respondNodeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	payload := map[string]any{"error": err.Error()}
	var duplicate *NodeDuplicateError
	switch {
	case errors.Is(err, ErrNodeNotFound):
		status = http.StatusNotFound
	case errors.As(err, &duplicate):
		status = http.StatusConflict
		payload["existing_node_id"] = duplicate.ExistingID
		payload["existing_node_name"] = duplicate.ExistingName
	case errors.Is(err, ErrNodeConflict), errors.Is(err, ErrInvalidNode):
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	writeJSON(w, payload)
}

// geoipResponse is the JSON structure returned by the GeoIP management endpoints.
type geoipResponse struct {
	Enabled    bool                     `json:"enabled"`
	Database   geoip.DatabaseStatusInfo `json:"database"`
	Message    string                   `json:"message,omitempty"`
	ReloadHint bool                     `json:"reload_hint,omitempty"` // true when the runtime GeoIP lookup should be reloaded to pick up the new db
}

// geoipDatabasePath reads the configured GeoIP database path under cfgSrc.
func (s *Server) geoipDatabasePath() string {
	s.cfgMu.RLock()
	c := s.cfgSrc
	s.cfgMu.RUnlock()
	if c == nil {
		return ""
	}
	c.RLock()
	defer c.RUnlock()
	return c.GeoIP.DatabasePath
}

// geoipEnabled reads the configured GeoIP enabled flag under cfgSrc.
func (s *Server) geoipEnabled() bool {
	s.cfgMu.RLock()
	c := s.cfgSrc
	s.cfgMu.RUnlock()
	if c == nil {
		return false
	}
	c.RLock()
	defer c.RUnlock()
	return c.GeoIP.Enabled
}

type groupPoolInput struct {
	Name                 string   `json:"name"`
	BindAddress          string   `json:"bind_address"`
	BindPort             uint16   `json:"bind_port"`
	Protocol             string   `json:"protocol"`
	Username             string   `json:"username"`
	Password             string   `json:"password"`
	DispatchMode         string   `json:"dispatch_mode"`
	Regions              []string `json:"regions"`
	ExplicitNodeIDs      []int64  `json:"explicit_node_ids"`
	ExcludedNodeIDs      *[]int64 `json:"excluded_node_ids"`
	FailureWindowSeconds int      `json:"failure_window_seconds"`
	FailureThreshold     int      `json:"failure_threshold"`
	HealthCheckSeconds   int      `json:"health_check_seconds"`
	Enabled              *bool    `json:"enabled"`
	SubscriptionEnabled  *bool    `json:"subscription_enabled"`
	SubscriptionMode     string   `json:"subscription_mode"`
	ExternalHost         string   `json:"external_host"`
}

type groupMemberResponse struct {
	NodeID       int64     `json:"node_id"`
	Tag          string    `json:"tag"`
	Name         string    `json:"name"`
	Region       string    `json:"region,omitempty"`
	Country      string    `json:"country,omitempty"`
	Status       string    `json:"status"`
	FailureCount int       `json:"failure_count"`
	LastError    string    `json:"last_error,omitempty"`
	EvictedAt    time.Time `json:"evicted_at,omitempty"`
	LatencyMs    int64     `json:"latency_ms"`
	Available    bool      `json:"available"`
	IsActive     bool      `json:"is_active"`
}

type groupNodeOptionResponse struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	URI              string `json:"uri"`
	Region           string `json:"region,omitempty"`
	Country          string `json:"country,omitempty"`
	Enabled          bool   `json:"enabled"`
	Tag              string `json:"tag,omitempty"`
	Status           string `json:"status"`
	LatencyMs        int64  `json:"latency_ms"`
	Available        bool   `json:"available"`
	InitialCheckDone bool   `json:"initial_check_done"`
	Selectable       bool   `json:"selectable"`
}

type groupPoolResponse struct {
	store.GroupPool
	RuntimeStatus    string                `json:"runtime_status"`
	RuntimeError     string                `json:"runtime_error,omitempty"`
	CurrentActiveTag string                `json:"current_active_tag,omitempty"`
	Members          []groupMemberResponse `json:"members"`
	MemberCount      int                   `json:"member_count"`
	AliveCount       int                   `json:"alive_count"`
	EvictedCount     int                   `json:"evicted_count"`
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "数据存储不可用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.writeGroupList(w, r)
	case http.MethodPost:
		var input groupPoolInput
		if err := decodeJSON(r, &input); err != nil {
			writeAPIError(w, http.StatusBadRequest, "无效的请求数据")
			return
		}
		s.groupMutationMu.Lock()
		defer s.groupMutationMu.Unlock()
		group, removedNodeIDs, err := s.groupFromInput(r.Context(), input, nil)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.store.CreateGroupPool(r.Context(), group); err != nil {
			writeAPIError(w, http.StatusConflict, err.Error())
			return
		}
		reloadError := s.applyGroupRuntimeMutation(r.Context(), nil, group)
		if reloadError != "" {
			rolledBack := s.store.DeleteGroupPool(r.Context(), group.ID) == nil
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]any{"error": reloadError, "reloaded": false, "reload_error": reloadError, "rolled_back": rolledBack})
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"group": group, "reloaded": true, "reload_error": "",
			"removed_unavailable_node_ids": removedNodeIDs})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGroupItem(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "数据存储不可用")
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/groups/"), "/"), "/")
	groupID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || groupID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "无效的分组 ID")
		return
	}
	mutationRequest := (len(parts) == 3 && parts[1] == "members" && r.Method == http.MethodDelete) ||
		(len(parts) == 1 && (r.Method == http.MethodPut || r.Method == http.MethodDelete))
	if mutationRequest {
		operationLock := s.groupOperationLock(groupID)
		operationLock.Lock()
		defer operationLock.Unlock()
	}
	if len(parts) == 4 && parts[1] == "members" && parts[3] == "activate" && r.Method == http.MethodPost {
		nodeID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || nodeID <= 0 {
			writeAPIError(w, http.StatusBadRequest, "无效的节点 ID")
			return
		}
		groupPool, lookupErr := s.store.GetGroupPool(r.Context(), groupID)
		if lookupErr != nil || groupPool == nil {
			writeAPIError(w, http.StatusNotFound, "分组不存在")
			return
		}
		if !groupPool.Enabled {
			writeAPIError(w, http.StatusConflict, "分组未启用")
			return
		}
		activateErr := error(nil)
		if runtimeManager, ok := s.nodeMgr.(GroupRuntimeManager); ok {
			activateErr = runtimeManager.ActivateGroupMember(r.Context(), groupID, nodeID)
		} else {
			activateErr = group.ActivateMember(groupID, nodeID)
		}
		if activateErr != nil {
			writeAPIError(w, http.StatusConflict, activateErr.Error())
			return
		}
		writeJSON(w, map[string]any{"message": "当前出口已切换", "group_id": groupID, "node_id": nodeID})
		return
	}
	if len(parts) == 4 && parts[1] == "members" && parts[3] == "restore" && r.Method == http.MethodPost {
		nodeID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || nodeID <= 0 {
			writeAPIError(w, http.StatusBadRequest, "无效的节点 ID")
			return
		}
		if err := s.store.ClearGroupNodeState(r.Context(), groupID, nodeID); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := group.RestoreGroupMember(groupID, nodeID); err != nil {
			// A disabled/invalid node may not currently have a runtime; the cleared
			// persisted state still guarantees restoration on the next reload.
			s.logger.Printf("restore group runtime: %v", err)
		}
		writeJSON(w, map[string]any{"message": "节点已恢复", "group_id": groupID, "node_id": nodeID})
		return
	}
	if len(parts) == 3 && parts[1] == "members" && r.Method == http.MethodDelete {
		nodeID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || nodeID <= 0 {
			writeAPIError(w, http.StatusBadRequest, "无效的节点 ID")
			return
		}
		runtimeState, ok := group.GroupRuntimeSnapshots()[groupID]
		if !ok {
			writeAPIError(w, http.StatusConflict, "分组当前未运行")
			return
		}
		isMember := false
		for _, member := range runtimeState.Members {
			if member.NodeID == nodeID {
				isMember = true
				break
			}
		}
		if !isMember {
			writeAPIError(w, http.StatusNotFound, "节点不是当前运行成员")
			return
		}
		s.groupMutationMu.Lock()
		groupPool, err := s.store.GetGroupPool(r.Context(), groupID)
		if err != nil || groupPool == nil {
			s.groupMutationMu.Unlock()
			writeAPIError(w, http.StatusNotFound, "分组不存在")
			return
		}
		originalGroup := cloneGroupPool(groupPool)
		groupPool.ExplicitNodeIDs = removeInt64(groupPool.ExplicitNodeIDs, nodeID)
		if !containsInt64(groupPool.ExcludedNodeIDs, nodeID) {
			groupPool.ExcludedNodeIDs = append(groupPool.ExcludedNodeIDs, nodeID)
		}
		if groupPool.CurrentActiveNodeID == nodeID || runtimeState.CurrentNodeID == nodeID {
			groupPool.CurrentActiveNodeID = 0
		}
		err = s.store.UpdateGroupPool(r.Context(), groupPool)
		s.groupMutationMu.Unlock()
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		reloadError := s.applyGroupRuntimeMutation(r.Context(), originalGroup, groupPool)
		if reloadError != "" {
			rolledBack := s.store.UpdateGroupPool(r.Context(), originalGroup) == nil
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]any{"error": reloadError, "reloaded": false, "reload_error": reloadError, "rolled_back": rolledBack})
			return
		}
		writeJSON(w, map[string]any{"message": "节点已从当前分组移除", "group_id": groupID, "node_id": nodeID,
			"reloaded": true, "reload_error": ""})
		return
	}
	if len(parts) == 3 && parts[1] == "subscription" && parts[2] == "reset-token" && r.Method == http.MethodPost {
		groupPool, err := s.store.GetGroupPool(r.Context(), groupID)
		if err != nil || groupPool == nil {
			writeAPIError(w, http.StatusNotFound, "分组不存在")
			return
		}
		token, err := s.generateSessionToken()
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "生成订阅 Token 失败")
			return
		}
		groupPool.SubscriptionToken = token
		if err := s.store.UpdateGroupPool(r.Context(), groupPool); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"message": "订阅 Token 已重置", "token": token})
		return
	}
	if len(parts) != 1 {
		writeAPIError(w, http.StatusNotFound, "接口不存在")
		return
	}
	existing, err := s.store.GetGroupPool(r.Context(), groupID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		writeAPIError(w, http.StatusNotFound, "分组不存在")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var input groupPoolInput
		if err := decodeJSON(r, &input); err != nil {
			writeAPIError(w, http.StatusBadRequest, "无效的请求数据")
			return
		}
		updated, removedNodeIDs, err := s.groupFromInput(r.Context(), input, existing)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.store.UpdateGroupPool(r.Context(), updated); err != nil {
			writeAPIError(w, http.StatusConflict, err.Error())
			return
		}
		reloadError := s.applyGroupRuntimeMutation(r.Context(), existing, updated)
		if reloadError != "" {
			rolledBack := s.store.UpdateGroupPool(r.Context(), existing) == nil
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]any{"error": reloadError, "reloaded": false, "reload_error": reloadError, "rolled_back": rolledBack})
			return
		}
		writeJSON(w, map[string]any{"group": updated, "reloaded": reloadError == "", "reload_error": reloadError,
			"removed_unavailable_node_ids": removedNodeIDs})
	case http.MethodDelete:
		if reloadError := s.applyGroupRuntimeMutation(r.Context(), existing, nil); reloadError != "" {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]any{"error": reloadError, "reloaded": false, "reload_error": reloadError, "rolled_back": true})
			return
		}
		if err := s.store.DeleteGroupPool(r.Context(), groupID); err != nil {
			_ = s.applyGroupRuntimeMutation(r.Context(), nil, existing)
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"message": "分组已删除", "reloaded": true, "reload_error": ""})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) writeGroupList(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListGroupPools(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	nodes, _ := s.store.ListNodes(r.Context(), store.NodeFilter{})
	nodeByID := make(map[int64]store.Node, len(nodes))
	for _, node := range nodes {
		nodeByID[node.ID] = node
	}
	monitorByTag := make(map[string]Snapshot)
	if s.mgr != nil {
		for _, snapshot := range s.mgr.Snapshot() {
			monitorByTag[snapshot.Tag] = snapshot
		}
	}
	runtimes := group.GroupRuntimeSnapshots()
	nodeOptions, err := s.groupNodeOptions(r.Context(), groups, monitorByTag)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	responses := make([]groupPoolResponse, 0, len(groups))
	for _, group := range groups {
		response := groupPoolResponse{GroupPool: group, Members: []groupMemberResponse{}}
		if runtimeManager, ok := s.nodeMgr.(GroupRuntimeManager); ok {
			runtimeStatus := runtimeManager.GroupRuntimeStatus(group.ID)
			response.RuntimeStatus, response.RuntimeError = runtimeStatus.Status, runtimeStatus.Error
		} else if !group.Enabled {
			response.RuntimeStatus = "stopped"
		} else {
			response.RuntimeStatus = "starting"
		}
		if runtimeState, ok := runtimes[group.ID]; ok {
			if response.RuntimeStatus == "starting" {
				response.RuntimeStatus = "ready"
			}
			response.CurrentActiveTag = runtimeState.CurrentTag
			for _, member := range runtimeState.Members {
				node := nodeByID[member.NodeID]
				mon := monitorByTag[member.Tag]
				status := member.Status
				if status == "ALIVE" && mon.InitialCheckDone && !mon.Available {
					status = "SUSPECT"
				}
				item := groupMemberResponse{NodeID: member.NodeID, Tag: member.Tag, Name: node.Name,
					Region: mon.Region, Country: mon.Country, Status: status, FailureCount: member.FailureCount,
					LastError: member.LastError, EvictedAt: member.EvictedAt, LatencyMs: mon.LastLatencyMs,
					Available: mon.Available, IsActive: member.NodeID == runtimeState.CurrentNodeID}
				if item.Region == "" {
					item.Region = node.Region
				}
				if item.Country == "" {
					item.Country = node.Country
				}
				response.Members = append(response.Members, item)
				if status == "EVICTED" {
					response.EvictedCount++
				} else if status == "ALIVE" {
					response.AliveCount++
				}
			}
		}
		response.MemberCount = len(response.Members)
		responses = append(responses, response)
	}
	writeJSON(w, map[string]any{"groups": responses, "nodes": nodeOptions, "port_range": map[string]int{"start": 10000, "end": 19999}})
}

func (s *Server) groupNodeOptions(ctx context.Context, groups []store.GroupPool, monitorByTag map[string]Snapshot) ([]groupNodeOptionResponse, error) {
	managedNodes, err := s.store.ListManagedNodes(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("读取节点管理列表: %w", err)
	}
	referenced := make(map[int64]struct{})
	for _, groupPool := range groups {
		for _, nodeID := range groupPool.ExplicitNodeIDs {
			referenced[nodeID] = struct{}{}
		}
	}
	monitorByURI := make(map[string]Snapshot, len(monitorByTag))
	monitorByIdentity := make(map[string]Snapshot, len(monitorByTag))
	monitorByName := make(map[string]Snapshot, len(monitorByTag))
	for _, snapshot := range monitorByTag {
		if snapshot.URI != "" {
			monitorByURI[snapshot.URI] = snapshot
			if identity, identityErr := nodecodec.ParseURI(snapshot.URI); identityErr == nil {
				monitorByIdentity[identity.Hash] = snapshot
			}
		}
		if snapshot.Name != "" {
			monitorByName[snapshot.Name] = snapshot
		}
	}
	options := make([]groupNodeOptionResponse, 0, len(managedNodes))
	for _, managed := range managedNodes {
		snapshot, found := monitorByURI[managed.URI]
		if !found && managed.IdentityHash != "" {
			snapshot, found = monitorByIdentity[managed.IdentityHash]
		}
		if !found {
			snapshot, found = monitorByName[managed.Name]
		}
		status := "pending"
		if !managed.Enabled {
			status = "disabled"
		} else if found && snapshot.Blacklisted {
			status = "blacklisted"
		} else if found && snapshot.InitialCheckDone && snapshot.Available {
			status = "normal"
		} else if found && snapshot.InitialCheckDone {
			status = "unavailable"
		}
		selectable := managed.Enabled && found && snapshot.InitialCheckDone && snapshot.Available && !snapshot.Blacklisted
		if _, isReferenced := referenced[managed.ID]; !selectable && !isReferenced {
			continue
		}
		latencyMs := int64(-1)
		if found {
			latencyMs = snapshot.LastLatencyMs
		}
		region, country := managed.Region, managed.Country
		if found && snapshot.Region != "" {
			region = snapshot.Region
		}
		if found && snapshot.Country != "" {
			country = snapshot.Country
		}
		options = append(options, groupNodeOptionResponse{ID: managed.ID, Name: managed.Name, URI: managed.URI,
			Region: region, Country: country, Enabled: managed.Enabled, Tag: snapshot.Tag, Status: status,
			LatencyMs: latencyMs, Available: found && snapshot.Available,
			InitialCheckDone: found && snapshot.InitialCheckDone, Selectable: selectable})
	}
	return options, nil
}

func (s *Server) selectableGroupNodeIDs(ctx context.Context) (map[int64]struct{}, error) {
	monitorByTag := make(map[string]Snapshot)
	if s.mgr != nil {
		for _, snapshot := range s.mgr.Snapshot() {
			monitorByTag[snapshot.Tag] = snapshot
		}
	}
	options, err := s.groupNodeOptions(ctx, nil, monitorByTag)
	if err != nil {
		return nil, err
	}
	selectable := make(map[int64]struct{}, len(options))
	for _, option := range options {
		if option.Selectable {
			selectable[option.ID] = struct{}{}
		}
	}
	return selectable, nil
}

func (s *Server) groupFromInput(ctx context.Context, input groupPoolInput, existing *store.GroupPool) (*store.GroupPool, []int64, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return nil, nil, errors.New("分组名称不能为空")
	}
	if input.BindAddress == "" {
		input.BindAddress = "0.0.0.0"
	}
	if net.ParseIP(input.BindAddress) == nil && input.BindAddress != "localhost" {
		return nil, nil, errors.New("监听地址无效")
	}
	if input.Protocol == "" {
		input.Protocol = config.InboundProtocolMixed
	}
	protocol, err := config.NormalizeInboundProtocol(input.Protocol)
	if err != nil {
		return nil, nil, err
	}
	switch strings.ToLower(strings.TrimSpace(input.DispatchMode)) {
	case "random":
		input.DispatchMode = "random"
	case "lowest_latency":
		input.DispatchMode = "lowest_latency"
	default:
		input.DispatchMode = "fixed"
	}
	if input.FailureWindowSeconds <= 0 {
		input.FailureWindowSeconds = 300
	}
	if input.FailureThreshold <= 0 {
		input.FailureThreshold = 3
	}
	if input.HealthCheckSeconds <= 0 {
		input.HealthCheckSeconds = 60
	}
	autoPort := input.BindPort == 0
	if autoPort {
		input.BindPort, err = s.allocateGroupPort(ctx)
		if err != nil {
			return nil, nil, err
		}
	} else if err := s.validateGroupPort(ctx, input.BindAddress, input.BindPort, existing); err != nil {
		return nil, nil, err
	}
	selectableNodeIDs, err := s.selectableGroupNodeIDs(ctx)
	if err != nil {
		return nil, nil, err
	}
	keptNodeIDs := make([]int64, 0, len(input.ExplicitNodeIDs))
	removedNodeIDs := make([]int64, 0)
	seenNodeIDs := make(map[int64]struct{}, len(input.ExplicitNodeIDs))
	for _, nodeID := range input.ExplicitNodeIDs {
		if nodeID <= 0 {
			continue
		}
		if _, duplicate := seenNodeIDs[nodeID]; duplicate {
			continue
		}
		seenNodeIDs[nodeID] = struct{}{}
		if _, ok := selectableNodeIDs[nodeID]; !ok {
			removedNodeIDs = append(removedNodeIDs, nodeID)
			continue
		}
		keptNodeIDs = append(keptNodeIDs, nodeID)
	}
	excludedInput := []int64(nil)
	if input.ExcludedNodeIDs != nil {
		excludedInput = *input.ExcludedNodeIDs
	} else if existing != nil {
		excludedInput = existing.ExcludedNodeIDs
	}
	keptSet := make(map[int64]struct{}, len(keptNodeIDs))
	for _, nodeID := range keptNodeIDs {
		keptSet[nodeID] = struct{}{}
	}
	excludedNodeIDs := make([]int64, 0, len(excludedInput))
	seenExcluded := make(map[int64]struct{}, len(excludedInput))
	for _, nodeID := range excludedInput {
		if nodeID <= 0 {
			continue
		}
		if _, manuallyAdded := keptSet[nodeID]; manuallyAdded {
			continue
		}
		if _, duplicate := seenExcluded[nodeID]; duplicate {
			continue
		}
		seenExcluded[nodeID] = struct{}{}
		excludedNodeIDs = append(excludedNodeIDs, nodeID)
	}
	regions := make([]string, 0, len(input.Regions))
	seenRegions := make(map[string]struct{})
	for _, value := range input.Regions {
		region := strings.ToLower(strings.TrimSpace(value))
		if region == "" {
			continue
		}
		if _, ok := seenRegions[region]; !ok {
			seenRegions[region] = struct{}{}
			regions = append(regions, region)
		}
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	} else if existing != nil {
		enabled = existing.Enabled
	}
	subscriptionEnabled := true
	if input.SubscriptionEnabled != nil {
		subscriptionEnabled = *input.SubscriptionEnabled
	} else if existing != nil {
		subscriptionEnabled = existing.SubscriptionEnabled
	}
	if input.SubscriptionMode != "members" {
		input.SubscriptionMode = "entry"
	}
	subscriptionToken := ""
	if existing != nil {
		subscriptionToken = existing.SubscriptionToken
	}
	if subscriptionToken == "" {
		subscriptionToken, err = s.generateSessionToken()
		if err != nil {
			return nil, nil, fmt.Errorf("生成订阅 Token: %w", err)
		}
	}
	group := &store.GroupPool{Name: input.Name, BindAddress: input.BindAddress, BindPort: input.BindPort,
		Protocol: protocol, Username: input.Username, Password: input.Password, DispatchMode: input.DispatchMode,
		Regions: regions, ExplicitNodeIDs: keptNodeIDs, ExcludedNodeIDs: excludedNodeIDs, FailureWindowSeconds: input.FailureWindowSeconds,
		FailureThreshold: input.FailureThreshold, HealthCheckSeconds: input.HealthCheckSeconds, Enabled: enabled,
		SubscriptionEnabled: subscriptionEnabled, SubscriptionToken: subscriptionToken,
		SubscriptionMode: input.SubscriptionMode, ExternalHost: strings.TrimSpace(input.ExternalHost)}
	if existing != nil {
		group.ID, group.CurrentActiveNodeID, group.CreatedAt, group.NodeStates = existing.ID, existing.CurrentActiveNodeID, existing.CreatedAt, existing.NodeStates
	}
	return group, removedNodeIDs, nil
}

func (s *Server) allocateGroupPort(ctx context.Context) (uint16, error) {
	groups, err := s.store.ListGroupPools(ctx)
	if err != nil {
		return 0, err
	}
	used := make(map[uint16]struct{}, len(groups)+2)
	for _, group := range groups {
		used[group.BindPort] = struct{}{}
	}
	s.cfgMu.RLock()
	cfg := s.cfgSrc
	s.cfgMu.RUnlock()
	if cfg != nil {
		cfg.RLock()
		used[cfg.Listener.Port] = struct{}{}
		for _, node := range cfg.Nodes {
			used[node.Port] = struct{}{}
		}
		cfg.RUnlock()
	}
	for port := 10000; port <= 19999; port++ {
		candidate := uint16(port)
		if _, ok := used[candidate]; ok {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
		if err == nil {
			listener.Close()
			return candidate, nil
		}
	}
	return 0, errors.New("10000–19999 端口段没有可用端口")
}

func (s *Server) validateGroupPort(ctx context.Context, address string, port uint16, existing *store.GroupPool) error {
	groups, err := s.store.ListGroupPools(ctx)
	if err != nil {
		return err
	}
	for _, group := range groups {
		if existing != nil && group.ID == existing.ID {
			continue
		}
		if group.BindPort == port {
			return fmt.Errorf("端口 %d 已被分组“%s”使用", port, group.Name)
		}
	}
	s.cfgMu.RLock()
	cfg := s.cfgSrc
	s.cfgMu.RUnlock()
	if cfg != nil {
		cfg.RLock()
		if cfg.Listener.Port == port {
			cfg.RUnlock()
			return fmt.Errorf("端口 %d 与全局代理入口冲突", port)
		}
		for _, node := range cfg.Nodes {
			if node.Port == port {
				cfg.RUnlock()
				return fmt.Errorf("端口 %d 与节点“%s”冲突", port, node.Name)
			}
		}
		cfg.RUnlock()
	}
	if existing == nil || existing.BindPort != port || existing.BindAddress != address {
		listener, err := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(int(port))))
		if err != nil {
			return fmt.Errorf("端口 %d 当前不可用: %w", port, err)
		}
		listener.Close()
	}
	return nil
}

func (s *Server) reloadAfterGroupMutation(ctx context.Context) string {
	if s.nodeMgr == nil {
		return "重载管理器不可用"
	}
	if err := s.nodeMgr.TriggerReload(ctx); err != nil {
		return err.Error()
	}
	return ""
}

func (s *Server) applyGroupRuntimeMutation(ctx context.Context, before, after *store.GroupPool) string {
	if runtimeManager, ok := s.nodeMgr.(GroupRuntimeManager); ok {
		if err := runtimeManager.ApplyGroupRuntime(ctx, before, after); err != nil {
			return err.Error()
		}
		return ""
	}
	return s.reloadAfterGroupMutation(ctx)
}

func cloneGroupPool(groupPool *store.GroupPool) *store.GroupPool {
	if groupPool == nil {
		return nil
	}
	cloned := *groupPool
	cloned.Regions = append([]string(nil), groupPool.Regions...)
	cloned.ExplicitNodeIDs = append([]int64(nil), groupPool.ExplicitNodeIDs...)
	cloned.ExcludedNodeIDs = append([]int64(nil), groupPool.ExcludedNodeIDs...)
	cloned.NodeStates = append([]store.GroupNodeState(nil), groupPool.NodeStates...)
	return &cloned
}

func (s *Server) groupOperationLock(groupID int64) *sync.Mutex {
	lock, _ := s.groupOperationLocks.LoadOrStore(groupID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func containsInt64(values []int64, target int64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func removeInt64(values []int64, target int64) []int64 {
	result := values[:0]
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}

// handleGeoipStatus reports the on-disk state of the GeoIP database.
func (s *Server) handleGeoipStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	dbPath := s.geoipDatabasePath()
	writeJSON(w, geoipResponse{
		Enabled:  s.geoipEnabled(),
		Database: geoip.DatabaseStatus(dbPath),
	})
}

// handleGeoipDownload downloads the GeoIP database if it is missing.
func (s *Server) handleGeoipDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	dbPath := s.geoipDatabasePath()
	if dbPath == "" {
		writeAPIError(w, http.StatusBadRequest, "GeoIP 数据库路径未配置")
		return
	}

	if err := geoip.EnsureDatabase(dbPath); err != nil {
		s.logger.Printf("GeoIP database download failed: %v", err)
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("下载 IP 库失败: %v", err))
		return
	}
	writeJSON(w, geoipResponse{
		Enabled:  s.geoipEnabled(),
		Database: geoip.DatabaseStatus(dbPath),
		Message:  "IP 库下载完成",
	})
}

// handleGeoipUpdate forces a re-download (update) of the GeoIP database,
// overwriting the existing file.
func (s *Server) handleGeoipUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	dbPath := s.geoipDatabasePath()
	if dbPath == "" {
		writeAPIError(w, http.StatusBadRequest, "GeoIP 数据库路径未配置")
		return
	}

	if err := geoip.DownloadDatabase(dbPath); err != nil {
		s.logger.Printf("GeoIP database update failed: %v", err)
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("更新 IP 库失败: %v", err))
		return
	}
	writeJSON(w, geoipResponse{
		Enabled:    s.geoipEnabled(),
		Database:   geoip.DatabaseStatus(dbPath),
		Message:    "IP 库更新完成，请重载配置以生效",
		ReloadHint: true,
	})
}

// Session management functions

// generateSessionToken creates a cryptographically secure random token.
func (s *Server) generateSessionToken() (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("failed to generate session token: %w", err)
	}
	return hex.EncodeToString(tokenBytes), nil
}

// createSession creates a new session with expiration.
func (s *Server) createSession() (*Session, error) {
	token, err := s.generateSessionToken()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	session := &Session{
		Token:     token,
		CreatedAt: now,
		ExpiresAt: now.Add(s.sessionTTL),
	}

	// Persist to Store if available
	if s.store != nil {
		storeSession := &store.Session{
			Token:     session.Token,
			CreatedAt: session.CreatedAt,
			ExpiresAt: session.ExpiresAt,
		}
		if err := s.store.CreateSession(context.Background(), storeSession); err != nil {
			s.logger.Printf("Failed to persist session to store: %v", err)
		}
	}

	// Also keep in memory for fast lookups
	s.sessionMu.Lock()
	s.sessions[token] = session
	s.sessionMu.Unlock()

	return session, nil
}

// validateSession checks if a session token is valid and not expired.
func (s *Server) validateSession(token string) bool {
	// Check in-memory cache first
	s.sessionMu.RLock()
	session, exists := s.sessions[token]
	s.sessionMu.RUnlock()

	if exists {
		if time.Now().After(session.ExpiresAt) {
			s.sessionMu.Lock()
			delete(s.sessions, token)
			s.sessionMu.Unlock()
			// Also delete from store
			if s.store != nil {
				_ = s.store.DeleteSession(context.Background(), token)
			}
			return false
		}
		return true
	}

	// Fallback: check Store (e.g., after restart)
	if s.store != nil {
		storeSess, err := s.store.GetSession(context.Background(), token)
		if err != nil || storeSess == nil {
			return false
		}
		if time.Now().After(storeSess.ExpiresAt) {
			_ = s.store.DeleteSession(context.Background(), token)
			return false
		}
		// Restore to in-memory cache
		s.sessionMu.Lock()
		s.sessions[token] = &Session{
			Token:     storeSess.Token,
			CreatedAt: storeSess.CreatedAt,
			ExpiresAt: storeSess.ExpiresAt,
		}
		s.sessionMu.Unlock()
		return true
	}

	return false
}

// cleanupExpiredSessions periodically removes expired sessions.
func (s *Server) cleanupExpiredSessions() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			now := time.Now()
			s.sessionMu.Lock()
			for token, session := range s.sessions {
				if now.After(session.ExpiresAt) {
					delete(s.sessions, token)
				}
			}
			s.sessionMu.Unlock()

			// Also cleanup in Store
			if s.store != nil {
				_ = s.store.CleanupExpiredSessions(context.Background())
			}
		}
	}
}

// secureCompareStrings performs constant-time string comparison to prevent timing attacks.
func secureCompareStrings(a, b string) bool {
	aBytes := []byte(a)
	bBytes := []byte(b)

	// If lengths differ, still perform a dummy comparison to maintain constant time
	if len(aBytes) != len(bBytes) {
		dummy := make([]byte, 32)
		subtle.ConstantTimeCompare(dummy, dummy)
		return false
	}

	return subtle.ConstantTimeCompare(aBytes, bBytes) == 1
}
