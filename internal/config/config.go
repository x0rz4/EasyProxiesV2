package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// Config describes the high level settings for the proxy pool server.
type Config struct {
	mu sync.RWMutex `yaml:"-"` // protects all fields for concurrent access

	Mode                string                    `yaml:"mode"`
	Listener            ListenerConfig            `yaml:"listener"`
	MultiPort           MultiPortConfig           `yaml:"multi_port"`
	Pool                PoolConfig                `yaml:"pool"`
	Groups              []GroupPoolConfig         `yaml:"-"` // Runtime-only; persisted in SQLite.
	Management          ManagementConfig          `yaml:"management"`
	SubscriptionRefresh SubscriptionRefreshConfig `yaml:"subscription_refresh"`
	GeoIP               GeoIPConfig               `yaml:"geoip"`
	Nodes               []NodeConfig              `yaml:"nodes"`
	NodesFile           string                    `yaml:"nodes_file"`    // 节点文件路径，每行一个 URI
	Subscriptions       []string                  `yaml:"subscriptions"` // 订阅链接列表
	ExternalIP          string                    `yaml:"external_ip"`   // 外部 IP 地址，用于导出时替换 0.0.0.0
	LogLevel            string                    `yaml:"log_level"`
	SkipCertVerify      bool                      `yaml:"skip_cert_verify"` // 全局跳过 SSL 证书验证
	DatabasePath        string                    `yaml:"database_path"`    // SQLite 数据库路径，默认 data/data.db

	filePath string `yaml:"-"` // 配置文件路径，用于保存
}

// GeoIPConfig controls GeoIP-based region routing.
type GeoIPConfig struct {
	Enabled            bool          `yaml:"enabled"`              // 是否启用 GeoIP 地域分区
	DatabasePath       string        `yaml:"database_path"`        // GeoLite2-Country.mmdb 文件路径
	Listen             string        `yaml:"listen"`               // GeoIP 路由监听地址，默认使用 listener 配置
	Port               uint16        `yaml:"port"`                 // GeoIP 路由监听端口，默认 2323
	AutoUpdateEnabled  bool          `yaml:"auto_update_enabled"`  // 是否启用自动更新数据库
	AutoUpdateInterval time.Duration `yaml:"auto_update_interval"` // 自动更新间隔，默认 24 小时
}

// ListenerConfig defines how the proxy should listen for clients.
type ListenerConfig struct {
	Address  string `yaml:"address"`
	Port     uint16 `yaml:"port"`
	Protocol string `yaml:"protocol"` // http, socks5, mixed
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// PoolConfig configures scheduling + failure handling.
type PoolConfig struct {
	Mode              string        `yaml:"mode"`
	FailureThreshold  int           `yaml:"failure_threshold"`
	BlacklistDuration time.Duration `yaml:"blacklist_duration"`
}

// GroupPoolConfig describes an independent listener backed by a dynamic subset
// of the globally configured nodes. Definitions are persisted in SQLite.
type GroupPoolConfig struct {
	ID                  int64                  `yaml:"-" json:"id"`
	Name                string                 `yaml:"-" json:"name"`
	BindAddress         string                 `yaml:"-" json:"bind_address"`
	BindPort            uint16                 `yaml:"-" json:"bind_port"`
	Protocol            string                 `yaml:"-" json:"protocol"`
	Username            string                 `yaml:"-" json:"username,omitempty"`
	Password            string                 `yaml:"-" json:"password,omitempty"`
	DispatchMode        string                 `yaml:"-" json:"dispatch_mode"`
	Regions             []string               `yaml:"-" json:"regions"`
	ExplicitNodeIDs     []int64                `yaml:"-" json:"explicit_node_ids"`
	ExcludedNodeIDs     []int64                `yaml:"-" json:"excluded_node_ids"`
	FailureWindow       time.Duration          `yaml:"-" json:"-"`
	FailureThreshold    int                    `yaml:"-" json:"failure_threshold"`
	HealthCheckInterval time.Duration          `yaml:"-" json:"-"`
	CurrentActiveNodeID int64                  `yaml:"-" json:"current_active_node_id,omitempty"`
	Enabled             bool                   `yaml:"-" json:"enabled"`
	SubscriptionEnabled bool                   `yaml:"-" json:"subscription_enabled"`
	SubscriptionToken   string                 `yaml:"-" json:"subscription_token,omitempty"`
	SubscriptionMode    string                 `yaml:"-" json:"subscription_mode"`
	ExternalHost        string                 `yaml:"-" json:"external_host,omitempty"`
	CreatedAt           time.Time              `yaml:"-" json:"created_at,omitempty"`
	UpdatedAt           time.Time              `yaml:"-" json:"updated_at,omitempty"`
	NodeStates          []GroupNodeStateConfig `yaml:"-" json:"-"`
}

// GroupNodeStateConfig restores eviction state across reloads and restarts.
type GroupNodeStateConfig struct {
	NodeID         int64     `json:"node_id"`
	FailureHistory []int64   `json:"failure_history,omitempty"`
	Evicted        bool      `json:"evicted"`
	LastError      string    `json:"last_error,omitempty"`
	EvictedAt      time.Time `json:"evicted_at,omitempty"`
}

// MultiPortConfig defines address/credential defaults for multi-port mode.
type MultiPortConfig struct {
	Address  string `yaml:"address"`
	BasePort uint16 `yaml:"base_port"`
	Protocol string `yaml:"protocol"` // http, socks5, mixed
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// ManagementConfig controls the monitoring HTTP endpoint.
type ManagementConfig struct {
	Enabled             *bool         `yaml:"enabled"`
	Listen              string        `yaml:"listen"`
	ProbeTarget         string        `yaml:"probe_target"`
	Password            string        `yaml:"password"` // WebUI 访问密码，为空则不需要密码
	HealthCheckInterval time.Duration `yaml:"health_check_interval"`
}

// SubscriptionRefreshConfig controls subscription auto-refresh and reload settings.
type SubscriptionRefreshConfig struct {
	Enabled            bool          `yaml:"enabled"`              // 是否启用定时刷新
	Interval           time.Duration `yaml:"interval"`             // 刷新间隔，默认 1 小时
	Timeout            time.Duration `yaml:"timeout"`              // 获取订阅的超时时间
	HealthCheckTimeout time.Duration `yaml:"health_check_timeout"` // 新节点健康检查超时
	DrainTimeout       time.Duration `yaml:"drain_timeout"`        // 旧实例排空超时时间
	MinAvailableNodes  int           `yaml:"min_available_nodes"`  // 最少可用节点数，低于此值不切换
}

// NodeSource indicates where a node configuration originated from.
type NodeSource string

const (
	NodeSourceInline       NodeSource = "inline"       // Defined directly in config.yaml nodes array
	NodeSourceFile         NodeSource = "nodes_file"   // Loaded from external nodes file
	NodeSourceSubscription NodeSource = "subscription" // Fetched from subscription URL
	NodeSourceManual       NodeSource = "manual"       // Added manually via WebUI
)

const (
	InboundProtocolHTTP   = "http"
	InboundProtocolSOCKS5 = "socks5"
	InboundProtocolMixed  = "mixed"
)

var supportedProxyURISchemes = []string{
	"vmess://",
	"vless://",
	"trojan://",
	"ss://",
	"ssr://",
	"hysteria://",
	"hysteria2://",
	"hy2://",
	"anytls://",
	"http://",
	"socks5://",
}

// NormalizeInboundProtocol normalizes inbound protocol aliases and validates the value.
func NormalizeInboundProtocol(value string) (string, error) {
	protocol := strings.ToLower(strings.TrimSpace(value))
	switch protocol {
	case "socks":
		protocol = InboundProtocolSOCKS5
	}
	switch protocol {
	case InboundProtocolHTTP, InboundProtocolSOCKS5, InboundProtocolMixed:
		return protocol, nil
	default:
		return "", fmt.Errorf("不支持的监听协议: %q（仅支持 http/socks5/mixed）", value)
	}
}

// NodeConfig describes a single upstream proxy endpoint expressed as URI.
type NodeConfig struct {
	ID       int64      `yaml:"-" json:"id,omitempty"`
	Name     string     `yaml:"name" json:"name"`
	URI      string     `yaml:"uri" json:"uri"`
	Port     uint16     `yaml:"port,omitempty" json:"port,omitempty"`
	Username string     `yaml:"username,omitempty" json:"username,omitempty"`
	Password string     `yaml:"password,omitempty" json:"password,omitempty"`
	Source   NodeSource `yaml:"-" json:"source,omitempty"`   // Runtime only, not persisted in YAML
	Disabled bool       `yaml:"-" json:"disabled,omitempty"` // Runtime only, not persisted in YAML; true = node is disabled
	Region   string     `yaml:"-" json:"region,omitempty"`
	Country  string     `yaml:"-" json:"country,omitempty"`
}

// NodeKey returns a unique identifier for the node based on its URI.
// This is used to preserve port assignments across reloads.
func (n *NodeConfig) NodeKey() string {
	return n.URI
}

// Load reads YAML config from disk and applies defaults/validation.
// This is used for the initial startup and will fetch subscription URLs.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	cfg.filePath = path

	// Resolve relative paths to be relative to the config file directory
	configDir := filepath.Dir(path)
	if cfg.NodesFile != "" && !filepath.IsAbs(cfg.NodesFile) {
		cfg.NodesFile = filepath.Join(configDir, cfg.NodesFile)
	}
	if cfg.GeoIP.DatabasePath != "" && !filepath.IsAbs(cfg.GeoIP.DatabasePath) {
		cfg.GeoIP.DatabasePath = filepath.Join(configDir, cfg.GeoIP.DatabasePath)
	}

	if err := cfg.normalizeInternal(false); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadForReload reads YAML config from disk for a reload operation.
// Unlike Load, it does NOT re-fetch subscription URLs.
// Only inline nodes from config.yaml are loaded; subscription and manual
// nodes are loaded from the SQLite Store by the caller.
func LoadForReload(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	cfg.filePath = path

	// Resolve relative paths to be relative to the config file directory
	configDir := filepath.Dir(path)
	if cfg.NodesFile != "" && !filepath.IsAbs(cfg.NodesFile) {
		cfg.NodesFile = filepath.Join(configDir, cfg.NodesFile)
	}
	if cfg.GeoIP.DatabasePath != "" && !filepath.IsAbs(cfg.GeoIP.DatabasePath) {
		cfg.GeoIP.DatabasePath = filepath.Join(configDir, cfg.GeoIP.DatabasePath)
	}

	if err := cfg.normalizeInternal(true); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) normalize() error {
	return c.normalizeInternal(false)
}

// applyDefaults sets default values for all config fields.
// This is the single source of truth for defaults, used by both
// normalizeInternal and NormalizeWithPortMap to avoid code duplication.
func (c *Config) applyDefaults() error {
	if c.Mode == "" {
		c.Mode = "pool"
	}
	// Normalize mode name: support both multi-port and multi_port
	if c.Mode == "multi_port" {
		c.Mode = "multi-port"
	}
	switch c.Mode {
	case "pool", "multi-port", "hybrid":
	default:
		return fmt.Errorf("unsupported mode %q (use 'pool', 'multi-port', or 'hybrid')", c.Mode)
	}
	if c.Listener.Address == "" {
		c.Listener.Address = "0.0.0.0"
	}
	if c.Listener.Port == 0 {
		c.Listener.Port = 2323
	}
	if c.Listener.Protocol == "" {
		c.Listener.Protocol = InboundProtocolHTTP
	}
	listenerProtocol, err := NormalizeInboundProtocol(c.Listener.Protocol)
	if err != nil {
		return err
	}
	c.Listener.Protocol = listenerProtocol
	if c.Pool.Mode == "" {
		c.Pool.Mode = "sequential"
	}
	if c.Pool.FailureThreshold <= 0 {
		c.Pool.FailureThreshold = 3
	}
	if c.Pool.BlacklistDuration <= 0 {
		c.Pool.BlacklistDuration = 24 * time.Hour
	}
	for idx := range c.Groups {
		g := &c.Groups[idx]
		if g.BindAddress == "" {
			g.BindAddress = "0.0.0.0"
		}
		if g.Protocol == "" {
			g.Protocol = InboundProtocolMixed
		}
		groupProtocol, err := NormalizeInboundProtocol(g.Protocol)
		if err != nil {
			return fmt.Errorf("group %q: %w", g.Name, err)
		}
		g.Protocol = groupProtocol
		switch strings.ToLower(strings.TrimSpace(g.DispatchMode)) {
		case "random":
			g.DispatchMode = "random"
		case "lowest_latency":
			g.DispatchMode = "lowest_latency"
		default:
			g.DispatchMode = "fixed"
		}
		if g.FailureWindow <= 0 {
			g.FailureWindow = 5 * time.Minute
		}
		if g.FailureThreshold <= 0 {
			g.FailureThreshold = 3
		}
		if g.HealthCheckInterval <= 0 {
			g.HealthCheckInterval = time.Minute
		}
		if g.SubscriptionMode != "members" {
			g.SubscriptionMode = "entry"
		}
	}
	if c.MultiPort.Address == "" {
		c.MultiPort.Address = "0.0.0.0"
	}
	if c.MultiPort.BasePort == 0 {
		c.MultiPort.BasePort = 28000
	}
	if c.MultiPort.Protocol == "" {
		c.MultiPort.Protocol = InboundProtocolHTTP
	}
	multiPortProtocol, err := NormalizeInboundProtocol(c.MultiPort.Protocol)
	if err != nil {
		return err
	}
	c.MultiPort.Protocol = multiPortProtocol
	if c.Management.Listen == "" {
		c.Management.Listen = "127.0.0.1:9090"
	}
	if c.Management.ProbeTarget == "" {
		c.Management.ProbeTarget = "www.apple.com:80"
	}
	if c.Management.Enabled == nil {
		defaultEnabled := true
		c.Management.Enabled = &defaultEnabled
	}
	if c.Management.HealthCheckInterval <= 0 {
		c.Management.HealthCheckInterval = 2 * time.Hour
	}

	// Subscription refresh defaults
	if c.SubscriptionRefresh.Interval <= 0 {
		c.SubscriptionRefresh.Interval = 1 * time.Hour
	}
	if c.SubscriptionRefresh.Timeout <= 0 {
		c.SubscriptionRefresh.Timeout = 30 * time.Second
	}
	if c.SubscriptionRefresh.HealthCheckTimeout <= 0 {
		c.SubscriptionRefresh.HealthCheckTimeout = 60 * time.Second
	}
	if c.SubscriptionRefresh.DrainTimeout <= 0 {
		c.SubscriptionRefresh.DrainTimeout = 30 * time.Second
	}
	if c.SubscriptionRefresh.MinAvailableNodes <= 0 {
		c.SubscriptionRefresh.MinAvailableNodes = 1
	}

	if c.LogLevel == "" {
		c.LogLevel = "info"
	}

	return nil
}

// normalizeInternal applies defaults, loads external nodes, and validates config.
// If skipSubscriptionFetch is true (reload/refresh scenario), only inline nodes
// from config.yaml are loaded. Subscription and manual nodes are managed by the
// SQLite Store and loaded by the caller (app.go / boxmgr).
func (c *Config) normalizeInternal(skipSubscriptionFetch bool) error {
	if err := c.applyDefaults(); err != nil {
		return err
	}

	// Mark inline nodes with source
	for idx := range c.Nodes {
		c.Nodes[idx].Source = NodeSourceInline
	}

	if skipSubscriptionFetch {
		// ---- Reload mode ----
		// Nodes will be loaded from SQLite Store by the caller (app.go / boxmgr).
		// Only inline nodes from config.yaml are included here.
		log.Printf("[config] reload mode: %d inline nodes from config.yaml", len(c.Nodes))
	} else {
		// ---- Initial load mode ----

		// Load nodes from file if specified (but NOT if subscriptions exist - subscription takes priority)
		if c.NodesFile != "" && len(c.Subscriptions) == 0 {
			fileNodes, err := loadNodesFromFile(c.NodesFile)
			if err != nil {
				return fmt.Errorf("load nodes from file %q: %w", c.NodesFile, err)
			}
			for idx := range fileNodes {
				fileNodes[idx].Source = NodeSourceFile
			}
			c.Nodes = append(c.Nodes, fileNodes...)
		}

		// Load nodes from subscriptions (fetched into memory, persisted to Store by app.go)
		if len(c.Subscriptions) > 0 {
			var subNodes []NodeConfig
			subTimeout := c.SubscriptionRefresh.Timeout
			for _, subURL := range c.Subscriptions {
				nodes, err := loadNodesFromSubscription(subURL, subTimeout)
				if err != nil {
					log.Printf("⚠️ Failed to load subscription %q: %v (skipping)", subURL, err)
					continue
				}
				log.Printf("✅ Loaded %d nodes from subscription", len(nodes))
				subNodes = append(subNodes, nodes...)
			}
			for idx := range subNodes {
				subNodes[idx].Source = NodeSourceSubscription
			}
			c.Nodes = append(c.Nodes, subNodes...)
		}
	}

	// Note: Manual nodes are loaded from SQLite Store by app.go, not from files.

	if len(c.Nodes) == 0 {
		// Not fatal — Store may have nodes from a previous run.
		log.Printf("⚠️ No nodes in config (inline/subscription/file). Will check Store for existing nodes.")
	}
	portCursor := c.MultiPort.BasePort
	for idx := range c.Nodes {
		c.Nodes[idx].Name = strings.TrimSpace(c.Nodes[idx].Name)
		c.Nodes[idx].URI = strings.TrimSpace(c.Nodes[idx].URI)

		if c.Nodes[idx].URI == "" {
			return fmt.Errorf("node %d is missing uri", idx)
		}

		// Auto-extract name from URI fragment (#name) if not provided
		if c.Nodes[idx].Name == "" {
			if parsed, err := url.Parse(c.Nodes[idx].URI); err == nil && parsed.Fragment != "" {
				// URL decode the fragment to handle encoded characters
				if decoded, err := url.QueryUnescape(parsed.Fragment); err == nil {
					c.Nodes[idx].Name = decoded
				} else {
					c.Nodes[idx].Name = parsed.Fragment
				}
			}
		}

		// Fallback to default name if still empty
		if c.Nodes[idx].Name == "" {
			c.Nodes[idx].Name = fmt.Sprintf("node-%d", idx)
		}

		// Auto-assign port in multi-port/hybrid mode, skip occupied ports
		if c.Nodes[idx].Port == 0 && (c.Mode == "multi-port" || c.Mode == "hybrid") {
			for !isPortAvailable(c.MultiPort.Address, portCursor) {
				log.Printf("⚠️  Port %d is in use, trying next port", portCursor)
				portCursor++
				if portCursor > 65535 {
					return fmt.Errorf("no available ports found starting from %d", c.MultiPort.BasePort)
				}
			}
			c.Nodes[idx].Port = portCursor
			portCursor++
		} else if c.Nodes[idx].Port == 0 {
			c.Nodes[idx].Port = portCursor
			portCursor++
		}

		if c.Mode == "multi-port" || c.Mode == "hybrid" {
			if c.Nodes[idx].Username == "" {
				c.Nodes[idx].Username = c.MultiPort.Username
				c.Nodes[idx].Password = c.MultiPort.Password
			}
		}
	}
	if c.DatabasePath == "" {
		c.DatabasePath = "data/data.db"
	}
	// Resolve database path relative to config file directory
	if c.filePath != "" && !filepath.IsAbs(c.DatabasePath) {
		c.DatabasePath = filepath.Join(filepath.Dir(c.filePath), c.DatabasePath)
	}

	// Auto-fix port conflicts in hybrid mode (pool port vs multi-port)
	if c.Mode == "hybrid" {
		poolPort := c.Listener.Port
		usedPorts := make(map[uint16]bool)
		usedPorts[poolPort] = true
		for idx := range c.Nodes {
			usedPorts[c.Nodes[idx].Port] = true
		}
		for idx := range c.Nodes {
			if c.Nodes[idx].Port == poolPort {
				// Find next available port
				newPort := c.Nodes[idx].Port + 1
				for usedPorts[newPort] || !isPortAvailable(c.MultiPort.Address, newPort) {
					newPort++
					if newPort > 65535 {
						return fmt.Errorf("no available port for node %q after conflict with pool port %d", c.Nodes[idx].Name, poolPort)
					}
				}
				log.Printf("⚠️  Node %q port %d conflicts with pool port, reassigned to %d", c.Nodes[idx].Name, poolPort, newPort)
				usedPorts[newPort] = true
				c.Nodes[idx].Port = newPort
			}
		}
	}

	return nil
}

// BuildPortMap creates a mapping from node URI to port for existing nodes.
// This is used to preserve port assignments when reloading configuration.
func (c *Config) BuildPortMap() map[string]uint16 {
	portMap := make(map[string]uint16)
	for _, node := range c.Nodes {
		if node.Port > 0 {
			portMap[node.NodeKey()] = node.Port
		}
	}
	return portMap
}

// NormalizeWithPortMap applies defaults and validation, preserving port assignments
// for nodes that exist in the provided port map.
func (c *Config) NormalizeWithPortMap(portMap map[string]uint16) error {
	if err := c.applyDefaults(); err != nil {
		return err
	}

	if len(c.Nodes) == 0 {
		return errors.New("config.nodes cannot be empty (no inline, subscription, or manual nodes available)")
	}

	// Build set of ports already assigned from portMap
	usedPorts := make(map[uint16]bool)
	if c.Mode == "hybrid" {
		usedPorts[c.Listener.Port] = true
	}

	// First pass: assign ports from portMap for existing nodes, and track all pre-existing ports
	for idx := range c.Nodes {
		c.Nodes[idx].Name = strings.TrimSpace(c.Nodes[idx].Name)
		c.Nodes[idx].URI = strings.TrimSpace(c.Nodes[idx].URI)
		if c.Nodes[idx].URI == "" {
			return fmt.Errorf("node %d is missing uri", idx)
		}

		// Extract name from URI fragment if not provided
		if c.Nodes[idx].Name == "" {
			if parsed, err := url.Parse(c.Nodes[idx].URI); err == nil && parsed.Fragment != "" {
				if decoded, err := url.QueryUnescape(parsed.Fragment); err == nil {
					c.Nodes[idx].Name = decoded
				} else {
					c.Nodes[idx].Name = parsed.Fragment
				}
			}
		}
		if c.Nodes[idx].Name == "" {
			c.Nodes[idx].Name = fmt.Sprintf("node-%d", idx)
		}

		// Check if this node has a preserved port from portMap
		if c.Mode == "multi-port" || c.Mode == "hybrid" {
			nodeKey := c.Nodes[idx].NodeKey()
			if existingPort, ok := portMap[nodeKey]; ok && existingPort > 0 {
				c.Nodes[idx].Port = existingPort
				usedPorts[existingPort] = true
				log.Printf("✅ Preserved port %d for node %q", existingPort, c.Nodes[idx].Name)
			} else if c.Nodes[idx].Port > 0 {
				// Track pre-existing ports (e.g. from store) to avoid conflicts
				usedPorts[c.Nodes[idx].Port] = true
			}
		}
	}

	// Second pass: assign new ports for nodes without preserved ports
	portCursor := c.MultiPort.BasePort
	for idx := range c.Nodes {
		if c.Nodes[idx].Port == 0 && (c.Mode == "multi-port" || c.Mode == "hybrid") {
			// Find next available port that's not used
			for usedPorts[portCursor] || !isPortAvailable(c.MultiPort.Address, portCursor) {
				portCursor++
				if portCursor > 65535 {
					return fmt.Errorf("no available ports found starting from %d", c.MultiPort.BasePort)
				}
			}
			c.Nodes[idx].Port = portCursor
			usedPorts[portCursor] = true
			log.Printf("📌 Assigned new port %d for node %q", portCursor, c.Nodes[idx].Name)
			portCursor++
		} else if c.Nodes[idx].Port == 0 {
			c.Nodes[idx].Port = portCursor
			portCursor++
		}

		// Apply default credentials
		if c.Mode == "multi-port" || c.Mode == "hybrid" {
			if c.Nodes[idx].Username == "" {
				c.Nodes[idx].Username = c.MultiPort.Username
				c.Nodes[idx].Password = c.MultiPort.Password
			}
		}
	}

	return nil
}

// ManagementEnabled reports whether the monitoring endpoint should run.
func (c *Config) ManagementEnabled() bool {
	if c.Management.Enabled == nil {
		return true
	}
	return *c.Management.Enabled
}

// loadNodesFromFile reads a nodes file where each line is a proxy URI
// Lines starting with # are comments, empty lines are ignored
func loadNodesFromFile(path string) ([]NodeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseNodesFromContent(string(data))
}

// loadNodesFromSubscription fetches and parses nodes from a subscription URL
// Supports multiple formats: base64 encoded, plain text, clash yaml, etc.
func loadNodesFromSubscription(subURL string, timeout time.Duration) ([]NodeConfig, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{
		Timeout: timeout,
	}

	req, err := http.NewRequest("GET", subURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Set common headers to avoid being blocked
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch subscription: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subscription returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	content := string(body)

	// Try to detect and parse different formats
	return parseSubscriptionContent(content)
}

// parseSubscriptionContent tries to parse subscription content in various formats (optimized)
func parseSubscriptionContent(content string) ([]NodeConfig, error) {
	content = strings.TrimSpace(content)

	// Quick check for YAML format (check first 200 chars for "proxies:")
	sampleSize := 200
	if len(content) < sampleSize {
		sampleSize = len(content)
	}
	if strings.Contains(content[:sampleSize], "proxies:") {
		return parseClashYAML(content)
	}

	// Check if it's base64 encoded (common for v2ray subscriptions)
	if isBase64(content) {
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			// Try URL-safe base64
			decoded, err = base64.RawStdEncoding.DecodeString(content)
			if err != nil {
				// Not base64, try as plain text
				return parseNodesFromContent(content)
			}
		}
		content = string(decoded)
	}

	// Parse as plain text (one URI per line)
	return parseNodesFromContent(content)
}

// ParseSubscriptionContent parses Clash YAML, Base64 and plain URI
// subscription payloads. It is shared by startup loading and refreshes so the
// accepted formats remain consistent.
func ParseSubscriptionContent(content string) ([]NodeConfig, error) {
	return parseSubscriptionContent(content)
}

// ParseImportContent parses pasted or imported proxy content in any supported
// format and returns normalized NodeConfig entries. Accepted formats:
//   - Clash YAML document (contains a top-level `proxies:` key)
//   - Clash YAML list items, one or more per line (e.g. `- { name: ..., type: ss, ... }`)
//   - Base64-encoded payload (v2ray subscriptions) wrapping a URI list or Clash YAML
//   - Plain proxy URI list (one URI per line, `#` comments allowed)
//
// Clash YAML entries are converted to standard proxy URIs via the same path used
// for subscriptions, so import and subscription accept identical node formats.
func ParseImportContent(content string) ([]NodeConfig, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("import content is empty")
	}

	// 1. Full Clash YAML document (top-level `proxies:` key).
	if hasProxiesKey(content) {
		return requireImportedNodes(parseClashYAML(content))
	}

	// 2. Inline Clash YAML list items ("- { ... }" or "- name: ...").
	if looksLikeInlineYAML(content) {
		return requireImportedNodes(parseClashYAML(wrapAsClashProxiesDoc(content)))
	}

	// 3. Base64-encoded payload (common for v2ray subscriptions).
	if decoded, ok := tryBase64Decode(content); ok {
		decoded = strings.TrimSpace(decoded)
		if hasProxiesKey(decoded) {
			return requireImportedNodes(parseClashYAML(decoded))
		}
		if looksLikeInlineYAML(decoded) {
			return requireImportedNodes(parseClashYAML(wrapAsClashProxiesDoc(decoded)))
		}
		return requireImportedNodes(parseNodesFromContent(decoded))
	}

	// 4. Plain proxy URI list.
	return requireImportedNodes(parseNodesFromContent(content))
}

func requireImportedNodes(nodes []NodeConfig, err error) ([]NodeConfig, error) {
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, errors.New("未找到支持的代理 URI 或 Clash 节点")
	}
	return nodes, nil
}

// hasProxiesKey reports whether content looks like a Clash YAML document by
// checking for a top-level `proxies:` key in its leading bytes.
func hasProxiesKey(content string) bool {
	sampleSize := 200
	if len(content) < sampleSize {
		sampleSize = len(content)
	}
	return strings.Contains(content[:sampleSize], "proxies:")
}

// looksLikeInlineYAML reports whether the first non-empty, non-comment line is a
// YAML list item ("- { ... }" flow style or "- key: value" block style).
func looksLikeInlineYAML(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "-\t")
	}
	return false
}

// wrapAsClashProxiesDoc indents a list of YAML items so it parses as the value of
// a top-level `proxies:` key. This lets bare `- { ... }` lines be parsed by
// parseClashYAML, which expects a full Clash document.
func wrapAsClashProxiesDoc(content string) string {
	var b strings.Builder
	b.WriteString("proxies:\n")
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// tryBase64Decode attempts to decode content as base64 using several common
// alphabets (standard and URL-safe, with and without padding). It returns the
// decoded string and true on success. Content that already contains a URI scheme
// ("://") is treated as plaintext and not decoded.
func tryBase64Decode(content string) (string, bool) {
	content = strings.TrimSpace(content)
	if content == "" || strings.Contains(content, "://") {
		return "", false
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(content); err == nil {
			return string(decoded), true
		}
	}
	return "", false
}

// parseNodesFromContent parses nodes from plain text content (one URI per line)
func parseNodesFromContent(content string) ([]NodeConfig, error) {
	var nodes []NodeConfig
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = unwrapProxyURILine(line)

		// Check if it's a valid proxy URI
		if IsProxyURI(line) {
			node := NodeConfig{
				URI: line,
			}
			// Extract node name from URI fragment (#name) if present
			if parsed, perr := url.Parse(line); perr == nil && parsed.Fragment != "" {
				if decoded, decErr := url.QueryUnescape(parsed.Fragment); decErr == nil {
					node.Name = decoded
				} else {
					node.Name = parsed.Fragment
				}
			}
			nodes = append(nodes, node)
		}
	}

	return nodes, nil
}

// unwrapProxyURILine accepts proxy links copied from Markdown-rendered text or
// angle-bracket autolinks in addition to ordinary raw URI lines.
func unwrapProxyURILine(line string) string {
	line = strings.TrimSpace(line)
	candidates := []string{line}
	if len(line) > 2 && strings.HasPrefix(line, "<") && strings.HasSuffix(line, ">") {
		candidates = append(candidates, strings.TrimSpace(line[1:len(line)-1]))
	}
	if separator := strings.Index(line, "]("); strings.HasPrefix(line, "[") && separator > 1 && strings.HasSuffix(line, ")") {
		candidates = append(candidates, strings.TrimSpace(line[separator+2:len(line)-1]))
	}
	for _, candidate := range candidates {
		if IsProxyURI(candidate) {
			return candidate
		}
		unescaped := strings.Replace(candidate, `\://`, "://", 1)
		if IsProxyURI(unescaped) {
			return unescaped
		}
	}
	return line
}

// isBase64 checks if a string looks like base64 encoded content (optimized version)
func isBase64(s string) bool {
	// Remove whitespace
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return false
	}

	// Remove newlines for checking
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")

	// Quick check: if it contains proxy URI schemes, it's not base64
	if strings.Contains(s, "://") {
		return false
	}

	// Check character set - base64 only contains A-Za-z0-9+/=
	// This is much faster than trying to decode
	for _, c := range s {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=') {
			return false
		}
	}

	// Length must be multiple of 4 (with padding)
	return len(s)%4 == 0
}

// IsProxyURI checks if a string is a valid proxy URI
func IsProxyURI(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	for _, scheme := range supportedProxyURISchemes {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return false
}

// clashConfig represents a minimal Clash configuration for parsing proxies
type clashConfig struct {
	Proxies []clashProxy `yaml:"proxies"`
}

type clashProxy struct {
	Name              string                 `yaml:"name"`
	Type              string                 `yaml:"type"`
	Server            string                 `yaml:"server"`
	Port              int                    `yaml:"port"`
	UUID              string                 `yaml:"uuid"`
	Password          string                 `yaml:"password"`
	Cipher            string                 `yaml:"cipher"`
	AlterId           int                    `yaml:"alterId"`
	Network           string                 `yaml:"network"`
	TLS               bool                   `yaml:"tls"`
	SkipCertVerify    bool                   `yaml:"skip-cert-verify"`
	ServerName        string                 `yaml:"servername"`
	SNI               string                 `yaml:"sni"`
	Flow              string                 `yaml:"flow"`
	UDP               bool                   `yaml:"udp"`
	WSOpts            *clashWSOptions        `yaml:"ws-opts"`
	GrpcOpts          *clashGrpcOptions      `yaml:"grpc-opts"`
	RealityOpts       *clashRealityOptions   `yaml:"reality-opts"`
	ClientFingerprint string                 `yaml:"client-fingerprint"`
	Plugin            string                 `yaml:"plugin"`
	PluginOpts        map[string]interface{} `yaml:"plugin-opts"`
}

type clashWSOptions struct {
	Path    string            `yaml:"path"`
	Headers map[string]string `yaml:"headers"`
}

type clashGrpcOptions struct {
	GrpcServiceName string `yaml:"grpc-service-name"`
}

type clashRealityOptions struct {
	PublicKey string `yaml:"public-key"`
	ShortID   string `yaml:"short-id"`
}

// parseClashYAML parses Clash YAML format and converts to NodeConfig
func parseClashYAML(content string) ([]NodeConfig, error) {
	var clash clashConfig
	if err := yaml.Unmarshal([]byte(content), &clash); err != nil {
		return nil, fmt.Errorf("parse clash yaml: %w", err)
	}

	var nodes []NodeConfig
	for _, proxy := range clash.Proxies {
		uri := convertClashProxyToURI(proxy)
		if uri != "" {
			nodes = append(nodes, NodeConfig{
				Name: proxy.Name,
				URI:  uri,
			})
		}
	}

	return nodes, nil
}

// convertClashProxyToURI converts a Clash proxy config to a standard URI
func convertClashProxyToURI(p clashProxy) string {
	switch strings.ToLower(p.Type) {
	case "vmess":
		return buildVMessURI(p)
	case "vless":
		return buildVLESSURI(p)
	case "trojan":
		return buildTrojanURI(p)
	case "ss", "shadowsocks":
		return buildShadowsocksURI(p)
	case "hysteria2", "hy2":
		return buildHysteria2URI(p)
	case "anytls":
		return buildAnyTLSURI(p)
	default:
		return ""
	}
}

func buildVMessURI(p clashProxy) string {
	params := url.Values{}
	if p.Network != "" && p.Network != "tcp" {
		params.Set("type", p.Network)
	}
	if p.TLS {
		params.Set("security", "tls")
		if p.ServerName != "" {
			params.Set("sni", p.ServerName)
		} else if p.SNI != "" {
			params.Set("sni", p.SNI)
		}
	}
	if p.WSOpts != nil {
		if p.WSOpts.Path != "" {
			params.Set("path", p.WSOpts.Path)
		}
		if host, ok := p.WSOpts.Headers["Host"]; ok {
			params.Set("host", host)
		}
	}
	if p.ClientFingerprint != "" {
		params.Set("fp", p.ClientFingerprint)
	}

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	return fmt.Sprintf("vmess://%s@%s:%d%s#%s", p.UUID, p.Server, p.Port, query, url.QueryEscape(p.Name))
}

func buildVLESSURI(p clashProxy) string {
	params := url.Values{}
	params.Set("encryption", "none")

	if p.Network != "" && p.Network != "tcp" {
		params.Set("type", p.Network)
	}
	if p.Flow != "" {
		params.Set("flow", p.Flow)
	}
	if p.TLS {
		params.Set("security", "tls")
		if p.ServerName != "" {
			params.Set("sni", p.ServerName)
		} else if p.SNI != "" {
			params.Set("sni", p.SNI)
		}
	}
	if p.RealityOpts != nil {
		params.Set("security", "reality")
		if p.RealityOpts.PublicKey != "" {
			params.Set("pbk", p.RealityOpts.PublicKey)
		}
		if p.RealityOpts.ShortID != "" {
			params.Set("sid", p.RealityOpts.ShortID)
		}
		if p.ServerName != "" {
			params.Set("sni", p.ServerName)
		}
	}
	if p.WSOpts != nil {
		if p.WSOpts.Path != "" {
			params.Set("path", p.WSOpts.Path)
		}
		if host, ok := p.WSOpts.Headers["Host"]; ok {
			params.Set("host", host)
		}
	}
	if p.GrpcOpts != nil && p.GrpcOpts.GrpcServiceName != "" {
		params.Set("serviceName", p.GrpcOpts.GrpcServiceName)
	}
	if p.ClientFingerprint != "" {
		params.Set("fp", p.ClientFingerprint)
	}

	return fmt.Sprintf("vless://%s@%s:%d?%s#%s", p.UUID, p.Server, p.Port, params.Encode(), url.QueryEscape(p.Name))
}

func buildTrojanURI(p clashProxy) string {
	params := url.Values{}
	if p.Network != "" && p.Network != "tcp" {
		params.Set("type", p.Network)
	}
	if p.ServerName != "" {
		params.Set("sni", p.ServerName)
	} else if p.SNI != "" {
		params.Set("sni", p.SNI)
	}
	if p.SkipCertVerify {
		params.Set("allowInsecure", "1")
	}
	if p.WSOpts != nil {
		if p.WSOpts.Path != "" {
			params.Set("path", p.WSOpts.Path)
		}
		if host, ok := p.WSOpts.Headers["Host"]; ok {
			params.Set("host", host)
		}
	}
	if p.ClientFingerprint != "" {
		params.Set("fp", p.ClientFingerprint)
	}

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	return fmt.Sprintf("trojan://%s@%s:%d%s#%s", p.Password, p.Server, p.Port, query, url.QueryEscape(p.Name))
}

func buildShadowsocksURI(p clashProxy) string {
	// Encode method:password in base64
	userInfo := base64.StdEncoding.EncodeToString([]byte(p.Cipher + ":" + p.Password))
	return fmt.Sprintf("ss://%s@%s:%d#%s", userInfo, p.Server, p.Port, url.QueryEscape(p.Name))
}

func buildHysteria2URI(p clashProxy) string {
	params := url.Values{}
	if p.ServerName != "" {
		params.Set("sni", p.ServerName)
	} else if p.SNI != "" {
		params.Set("sni", p.SNI)
	}
	if p.SkipCertVerify {
		params.Set("insecure", "1")
	}

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	return fmt.Sprintf("hysteria2://%s@%s:%d%s#%s", p.Password, p.Server, p.Port, query, url.QueryEscape(p.Name))
}

func buildAnyTLSURI(p clashProxy) string {
	params := url.Values{}
	if p.ServerName != "" {
		params.Set("sni", p.ServerName)
	} else if p.SNI != "" {
		params.Set("sni", p.SNI)
	}
	if p.SkipCertVerify {
		params.Set("insecure", "1")
	}

	query := ""
	if len(params) > 0 {
		query = "?" + params.Encode()
	}

	return fmt.Sprintf("anytls://%s@%s:%d%s#%s", p.Password, p.Server, p.Port, query, url.QueryEscape(p.Name))
}

// RLock acquires a read lock on the config.
func (c *Config) RLock() {
	if c != nil {
		c.mu.RLock()
	}
}

// RUnlock releases the read lock on the config.
func (c *Config) RUnlock() {
	if c != nil {
		c.mu.RUnlock()
	}
}

// Lock acquires a write lock on the config.
func (c *Config) Lock() {
	if c != nil {
		c.mu.Lock()
	}
}

// Unlock releases the write lock on the config.
func (c *Config) Unlock() {
	if c != nil {
		c.mu.Unlock()
	}
}

// Clone creates a deep copy of the Config (without the mutex).
// The caller must hold at least a read lock if the config may be modified concurrently.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	cloned := *c
	cloned.mu = sync.RWMutex{} // fresh mutex for the clone

	// Deep copy slices
	if c.Nodes != nil {
		cloned.Nodes = make([]NodeConfig, len(c.Nodes))
		copy(cloned.Nodes, c.Nodes)
	}
	if c.Subscriptions != nil {
		cloned.Subscriptions = make([]string, len(c.Subscriptions))
		copy(cloned.Subscriptions, c.Subscriptions)
	}
	if c.Groups != nil {
		cloned.Groups = make([]GroupPoolConfig, len(c.Groups))
		copy(cloned.Groups, c.Groups)
		for idx := range cloned.Groups {
			cloned.Groups[idx].Regions = append([]string(nil), c.Groups[idx].Regions...)
			cloned.Groups[idx].ExplicitNodeIDs = append([]int64(nil), c.Groups[idx].ExplicitNodeIDs...)
			cloned.Groups[idx].NodeStates = append([]GroupNodeStateConfig(nil), c.Groups[idx].NodeStates...)
			for stateIdx := range cloned.Groups[idx].NodeStates {
				cloned.Groups[idx].NodeStates[stateIdx].FailureHistory = append([]int64(nil), c.Groups[idx].NodeStates[stateIdx].FailureHistory...)
			}
		}
	}
	return &cloned
}

// Snapshot returns a deep copy protected against concurrent config updates.
func (c *Config) Snapshot() *Config {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Clone()
}

// FilePath returns the config file path.
func (c *Config) FilePath() string {
	if c == nil {
		return ""
	}
	return c.filePath
}

// SetFilePath sets the config file path (used when creating config programmatically).
func (c *Config) SetFilePath(path string) {
	if c != nil {
		c.filePath = path
	}
}

// NOTE: ManualNodesFilePath, LoadManualNodes, SaveManualNodes, writeNodesToFile,
// SaveNodes, Save have been removed. All node persistence is now handled by
// the SQLite Store (internal/store package).

// SaveSettings persists all editable settings to config.yaml.
// Node data is managed by the SQLite Store, not config.yaml.
func (c *Config) SaveSettings() error {
	if c == nil {
		return errors.New("config is nil")
	}
	if c.filePath == "" {
		return errors.New("config file path is unknown")
	}

	// Build a clean config struct for serialization.
	// We read the existing YAML to preserve fields not managed by the settings API
	// (e.g., nodes, nodes_file, database_path).
	data, err := os.ReadFile(c.filePath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var saveCfg Config
	if err := yaml.Unmarshal(data, &saveCfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}

	// Sync all editable fields from runtime config
	saveCfg.Mode = c.Mode
	saveCfg.LogLevel = c.LogLevel
	saveCfg.ExternalIP = c.ExternalIP
	saveCfg.SkipCertVerify = c.SkipCertVerify

	// Listener
	saveCfg.Listener = c.Listener

	// Multi-port
	saveCfg.MultiPort = c.MultiPort

	// Pool
	saveCfg.Pool = c.Pool

	// Management
	saveCfg.Management = c.Management

	// Subscription refresh
	saveCfg.SubscriptionRefresh = c.SubscriptionRefresh

	// GeoIP
	saveCfg.GeoIP = c.GeoIP

	// Subscriptions
	saveCfg.Subscriptions = c.Subscriptions

	newData, err := yaml.Marshal(&saveCfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	// Use atomic write to prevent data loss from concurrent/interrupted writes
	if err := writeFileAtomic(c.filePath, newData, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// ValidateSettingsRequest validates the settings request fields and returns
// a descriptive error for any invalid values, instead of silently ignoring them.
func ValidateSettingsRequest(mode string, listenerPort, multiPortBasePort uint16,
	listenerProtocol, multiPortProtocol,
	poolBlacklistDuration, subRefreshInterval, subRefreshTimeout,
	subRefreshHealthCheckTimeout, subRefreshDrainTimeout,
	geoIPAutoUpdateInterval, managementHealthCheckInterval string) error {

	// Validate mode
	switch mode {
	case "pool", "multi-port", "hybrid":
	default:
		return fmt.Errorf("不支持的运行模式: %q", mode)
	}

	// Validate ports
	if listenerPort == 0 {
		return fmt.Errorf("监听端口不能为 0")
	}
	if multiPortBasePort == 0 {
		return fmt.Errorf("多端口起始端口不能为 0")
	}

	// Validate inbound protocols
	if _, err := NormalizeInboundProtocol(listenerProtocol); err != nil {
		return fmt.Errorf("监听入口协议无效: %w", err)
	}
	if _, err := NormalizeInboundProtocol(multiPortProtocol); err != nil {
		return fmt.Errorf("多端口入口协议无效: %w", err)
	}

	// Validate duration fields
	durationFields := []struct {
		name  string
		value string
	}{
		{"黑名单持续时间", poolBlacklistDuration},
		{"订阅刷新间隔", subRefreshInterval},
		{"订阅获取超时", subRefreshTimeout},
		{"健康检查超时", subRefreshHealthCheckTimeout},
		{"排空超时", subRefreshDrainTimeout},
		{"GeoIP 更新间隔", geoIPAutoUpdateInterval},
		{"周期健康检查间隔", managementHealthCheckInterval},
	}

	for _, field := range durationFields {
		if field.value != "" {
			d, err := time.ParseDuration(field.value)
			if err != nil {
				return fmt.Errorf("%s 格式无效: %q (%v)", field.name, field.value, err)
			}
			if d <= 0 {
				return fmt.Errorf("%s 必须大于 0: %q", field.name, field.value)
			}
		}
	}

	return nil
}

// isPortAvailable checks if a port is available for binding.
func isPortAvailable(address string, port uint16) bool {
	addr := fmt.Sprintf("%s:%d", address, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// writeFileAtomic writes data to a file atomically by writing to a temporary
// file first and then renaming it. This prevents data loss if the process is
// interrupted or if concurrent reads occur during the write.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	// Create temp file in same directory (required for atomic rename on same filesystem)
	tmpFile, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Clean up temp file on error
	success := false
	defer func() {
		if !success {
			tmpFile.Close()
			os.Remove(tmpPath)
		}
	}()

	// Write data to temp file
	if _, err := tmpFile.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	// Sync to disk before rename
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}

	// Close before rename (required on Windows)
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Set permissions
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, path); err != nil {
		// 当 config.yaml 通过 bind-mount 映射到宿主机时，部分宿主机文件系统/挂载模式会导致
		// 对“正在被挂载使用的目标文件”执行 rename 返回 EBUSY（device or resource busy）。
		// 这种情况下回退为“直接覆盖写入”（非原子，但可用），以保证 WebUI 能保存配置。
		if errors.Is(err, syscall.EBUSY) {
			data, rerr := os.ReadFile(tmpPath)
			if rerr != nil {
				return fmt.Errorf("rename temp file: %w", err)
			}
			if werr := os.WriteFile(path, data, perm); werr != nil {
				return fmt.Errorf("rename temp file: %w", err)
			}
			if cerr := os.Remove(tmpPath); cerr != nil {
				// best-effort cleanup
			}
			return nil
		}
		return fmt.Errorf("rename temp file: %w", err)
	}

	success = true
	return nil
}
