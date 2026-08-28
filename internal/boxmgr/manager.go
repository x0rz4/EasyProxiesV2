package boxmgr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"easy_proxies/internal/builder"
	"easy_proxies/internal/config"
	"easy_proxies/internal/group"
	"easy_proxies/internal/groupmember"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/nodecodec"
	"easy_proxies/internal/outbound/pool"
	"easy_proxies/internal/runtimetag"
	"easy_proxies/internal/store"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
)

// Ensure Manager implements monitor.NodeManager.
var _ monitor.NodeManager = (*Manager)(nil)
var _ monitor.GroupRuntimeManager = (*Manager)(nil)

const (
	defaultDrainTimeout       = 10 * time.Second
	defaultHealthCheckTimeout = 30 * time.Second
	healthCheckPollInterval   = 500 * time.Millisecond
)

// Logger defines logging interface for the manager.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Option configures the Manager.
type Option func(*Manager)

// WithLogger sets a custom logger.
func WithLogger(l Logger) Option {
	return func(m *Manager) { m.logger = l }
}

// WithStore sets the data store.
func WithStore(s store.Store) Option {
	return func(m *Manager) { m.store = s }
}

// ConfigUpdateListener is notified when the active config changes (e.g., after reload).
type ConfigUpdateListener interface {
	OnConfigUpdate(cfg *config.Config)
}

// Manager owns the lifecycle of the active sing-box instance.
type Manager struct {
	mu       sync.RWMutex
	reloadMu sync.Mutex

	currentBox    *box.Box
	monitorMgr    *monitor.Manager
	monitorServer *monitor.Server
	cfg           *config.Config
	monitorCfg    monitor.Config
	store         store.Store

	drainTimeout      time.Duration
	minAvailableNodes int
	logger            Logger

	baseCtx            context.Context
	healthCheckStarted bool
	configListeners    []ConfigUpdateListener
	idle               bool // true when manager was started but stopped due to 0 enabled nodes

	// lastAppliedBasePort tracks the BasePort from the last successful
	// Start/Reload so an enabled multi-port topology can be reassigned when it changes.
	lastAppliedBasePort uint16

	groupSlotsMu sync.Mutex
	groupSlots   map[int64]*groupRuntimeSlot
	landingMu    sync.Mutex

	runtimeGeneration uint64
	runtimeNodes      map[string]runtimeNode
}

type runtimeNode struct {
	tag  string
	node config.NodeConfig
}

type groupRuntimeSlot struct {
	gate               chan struct{}
	box                *box.Box
	mu                 sync.RWMutex
	state              monitor.GroupRuntimeStatus
	appliedFingerprint string
	members            map[string]runtimeNode
	retiring           map[string]*groupRetirement
}

type groupRetirement struct {
	cancel  chan struct{}
	node    runtimeNode
	member  pool.RetiredMember
	version uint64
	once    sync.Once
}

func (r *groupRetirement) stop() { r.once.Do(func() { close(r.cancel) }) }

// New creates a BoxManager with the given config.
func New(cfg *config.Config, monitorCfg monitor.Config, opts ...Option) *Manager {
	m := &Manager{
		cfg:               cfg,
		monitorCfg:        monitorCfg,
		groupSlots:        make(map[int64]*groupRuntimeSlot),
		runtimeGeneration: runtimetag.InitialVersion,
		runtimeNodes:      make(map[string]runtimeNode),
	}
	m.applyConfigSettings(cfg)
	for _, opt := range opts {
		opt(m)
	}
	if m.logger == nil {
		m.logger = defaultLogger{}
	}
	if m.drainTimeout <= 0 {
		m.drainTimeout = defaultDrainTimeout
	}
	return m
}

// Start creates and starts the initial sing-box instance.
func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.ensureMonitor(ctx); err != nil {
		return err
	}

	m.mu.Lock()
	if m.cfg == nil {
		m.mu.Unlock()
		return errors.New("box manager requires config")
	}
	if m.currentBox != nil {
		m.mu.Unlock()
		return errors.New("sing-box already running")
	}
	m.applyConfigSettings(m.cfg)
	m.baseCtx = ctx
	cfg := m.cfg
	m.mu.Unlock()
	m.applyCachedNodeLocations(ctx, cfg)

	// Try to start, with automatic port conflict resolution
	var instance *box.Box
	maxRetries := 10
	for retry := 0; retry < maxRetries; retry++ {
		var err error
		instance, err = m.createBaseBox(ctx, cfg)
		if err != nil {
			return err
		}
		if err = instance.Start(); err != nil {
			_ = instance.Close()
			// Check if it's a port conflict error
			if conflictPort := extractPortFromBindError(err); conflictPort > 0 {
				m.logger.Warnf("port %d is in use, reassigning and retrying...", conflictPort)
				if reassigned := reassignConflictingPort(cfg, conflictPort); reassigned {
					pool.ResetAllRuntimeState() // no group boxes have started yet
					continue
				}
			}
			return fmt.Errorf("start sing-box: %w", err)
		}
		break // Success
	}

	// The first instance supplies per-node dialers. Only uncached or previously
	// failed nodes are queried through their own tunnel. If classification
	// changes topology metadata, rebuild the base once before any group starts.
	if m.detectMissingNodeLocations(ctx, cfg) {
		if err := instance.Close(); err != nil {
			m.logger.Warnf("close provisional base after landing IP detection: %v", err)
		}
		m.monitorMgr.BeginReload()
		var err error
		instance, err = m.createBaseBox(ctx, cfg)
		if err != nil {
			return fmt.Errorf("rebuild base with landing locations: %w", err)
		}
		if err = instance.Start(); err != nil {
			_ = instance.Close()
			return fmt.Errorf("start base with landing locations: %w", err)
		}
		m.monitorMgr.SweepStaleNodes()
	}

	m.mu.Lock()
	m.currentBox = instance
	m.lastAppliedBasePort = cfg.MultiPort.BasePort
	m.runtimeGeneration = runtimetag.InitialVersion
	m.runtimeNodes = runtimeNodesForConfig(cfg, m.runtimeGeneration, instance)
	m.mu.Unlock()

	for index := range cfg.Groups {
		groupCfg := cfg.Groups[index]
		if !groupCfg.Enabled || groupCfg.ID == 0 {
			m.setGroupRuntimeStatus(groupCfg.ID, "stopped", "")
			continue
		}
		if err := m.startInitialGroup(ctx, groupCfg.ID); err != nil {
			m.setGroupRuntimeStatus(groupCfg.ID, "error", err.Error())
			m.logger.Errorf("start group %d: %v", groupCfg.ID, err)
		}
	}

	// Start periodic health check after nodes are registered
	m.mu.Lock()
	if m.monitorMgr != nil && !m.healthCheckStarted {
		interval := effectiveHealthCheckInterval(cfg)
		m.monitorMgr.StartPeriodicHealthCheck(interval)
		m.healthCheckStarted = true
	}
	m.mu.Unlock()
	if m.monitorMgr != nil {
		m.monitorMgr.RequestStartupProbeAllOnce()
	}

	// Wait for initial health check if min nodes configured
	if cfg.SubscriptionRefresh.MinAvailableNodes > 0 {
		timeout := cfg.SubscriptionRefresh.HealthCheckTimeout
		if timeout <= 0 {
			timeout = defaultHealthCheckTimeout
		}
		if err := m.waitForHealthCheck(timeout); err != nil {
			m.logger.Warnf("initial health check warning: %v", err)
			// Don't fail startup, just warn
		}
	}

	m.logger.Infof("sing-box instance started with %d nodes", len(cfg.Nodes))
	return nil
}

// Reload gracefully switches to a new configuration.
// For multi-port mode, we must stop the old instance first to release ports.
// Supports transitioning from idle state (0 nodes → has nodes).
func (m *Manager) Reload(newCfg *config.Config) error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	return m.reload(newCfg)
}

func (m *Manager) reload(newCfg *config.Config) error {
	if newCfg == nil {
		return errors.New("new config is nil")
	}

	m.mu.RLock()
	locationCtx := m.baseCtx
	m.mu.RUnlock()
	if locationCtx == nil {
		locationCtx = context.Background()
	}
	m.applyCachedNodeLocations(locationCtx, newCfg)

	m.mu.Lock()
	if m.currentBox == nil && !m.idle {
		m.mu.Unlock()
		return errors.New("manager not started")
	}
	ctx := m.baseCtx
	oldBox := m.currentBox
	oldCfg := m.cfg
	if oldBox != nil && runtimeConfigEqual(oldCfg, newCfg) {
		m.mu.Unlock()
		// A failed/missing cache can be retried against the current runtime. A
		// newly discovered country then flows through the regular controlled
		// reload so affected group membership is recalculated.
		if m.detectMissingNodeLocations(locationCtx, newCfg) {
			return m.reload(newCfg)
		}
		if err := m.retryDegradedGroupRuntimes(ctx, newCfg); err != nil {
			return err
		}
		m.logger.Infof("reload skipped: runtime configuration unchanged")
		return nil
	}
	if oldBox != nil && incrementalReloadEligible(oldCfg, newCfg) {
		m.mu.Unlock()
		if err := m.reloadNodesIncrementally(ctx, oldBox, oldCfg, newCfg); err != nil {
			return err
		}
		groupErr := m.syncGroupRuntimesAfterBaseReload(ctx, oldCfg, newCfg)
		if m.detectMissingNodeLocations(locationCtx, newCfg) {
			return m.reload(newCfg)
		}
		return groupErr
	}
	drainTimeout := m.drainTimeout
	reloadGeneration := m.runtimeGeneration + 1
	m.currentBox = nil // Mark as reloading
	m.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	m.logger.Infof("reloading with %d nodes", len(newCfg.Nodes))

	// The old instance owns the listener ports, so let active connections drain
	// before closing it and binding the replacement instance.
	if oldBox != nil {
		m.waitForConnectionsToDrain(drainTimeout)
		m.logger.Infof("stopping old instance to release ports...")
		if err := oldBox.Close(); err != nil {
			m.logger.Warnf("error closing old instance: %v", err)
		}
		// Give OS time to release ports
		time.Sleep(500 * time.Millisecond)
	}
	// Begin a new reload generation. Nodes re-registered during createBox will
	// be marked with the new generation; stale (disabled/removed) nodes will be
	// swept after the new box is successfully started.
	if m.monitorMgr != nil {
		m.monitorMgr.BeginReload()
	}

	// Create and start new box instance with automatic port conflict resolution
	var instance *box.Box
	maxRetries := 10
	for retry := 0; retry < maxRetries; retry++ {
		var err error
		instance, err = m.createBaseBoxVersion(ctx, newCfg, reloadGeneration)
		if err != nil {
			m.rollbackToOldConfig(ctx, oldCfg)
			return fmt.Errorf("create new box: %w", err)
		}
		if err = instance.Start(); err != nil {
			_ = instance.Close()
			// Check if it's a port conflict error
			if conflictPort := extractPortFromBindError(err); conflictPort > 0 {
				m.logger.Warnf("port %d is in use, reassigning and retrying...", conflictPort)
				if reassigned := reassignConflictingPort(newCfg, conflictPort); reassigned {
					continue
				}
			}
			m.rollbackToOldConfig(ctx, oldCfg)
			return fmt.Errorf("start new box: %w", err)
		}
		break // Success
	}

	// New subscription nodes only have dialers after the provisional base has
	// started. Resolve their landing locations now, then rebuild this base once;
	// group runtimes are synchronized only after the authoritative metadata is
	// present, so they never observe a URI-host-derived country.
	if m.detectMissingNodeLocations(ctx, newCfg) {
		if err := instance.Close(); err != nil {
			m.logger.Warnf("close provisional reloaded base after landing IP detection: %v", err)
		}
		if m.monitorMgr != nil {
			m.monitorMgr.BeginReload()
		}
		var err error
		instance, err = m.createBaseBoxVersion(ctx, newCfg, reloadGeneration)
		if err != nil {
			m.rollbackToOldConfig(ctx, oldCfg)
			return fmt.Errorf("rebuild base with landing locations: %w", err)
		}
		if err = instance.Start(); err != nil {
			_ = instance.Close()
			m.rollbackToOldConfig(ctx, oldCfg)
			return fmt.Errorf("start base with landing locations: %w", err)
		}
	}

	// Sweep stale monitor entries (disabled/removed nodes) now that the new box
	// has successfully registered all active nodes with the current generation.
	if m.monitorMgr != nil {
		m.monitorMgr.SweepStaleNodes()
	}

	m.applyConfigSettings(newCfg)

	m.mu.Lock()
	m.currentBox = instance
	m.cfg = newCfg
	m.runtimeGeneration = reloadGeneration
	m.runtimeNodes = runtimeNodesForConfig(newCfg, reloadGeneration, instance)
	m.idle = false // Clear idle state on successful reload
	m.lastAppliedBasePort = newCfg.MultiPort.BasePort
	// Update monitor server's config reference so settings API reads the latest config
	if m.monitorServer != nil {
		m.monitorServer.SetConfig(newCfg)
	}
	// Notify config update listeners (e.g., subscription manager)
	listeners := make([]ConfigUpdateListener, len(m.configListeners))
	copy(listeners, m.configListeners)
	m.mu.Unlock()
	groupReloadErr := m.syncGroupRuntimesAfterBaseReload(ctx, oldCfg, newCfg)

	for _, l := range listeners {
		l.OnConfigUpdate(newCfg)
	}

	// Reload 成功后立即触发 1 次全量探测（内部去重，避免多次 Reload 造成突发）
	if m.monitorMgr != nil {
		m.monitorMgr.UpdateProbePolicy(monitor.ProbePolicy{
			Concurrency: newCfg.Management.ProbeConcurrency, StartupTimeout: newCfg.Management.StartupProbeTimeout,
			RoutineTimeout: newCfg.Management.RoutineProbeTimeout, DialTimeout: newCfg.Management.ProbeDialTimeout,
			ResponseTimeout: newCfg.Management.ProbeResponseTimeout, RoutineRetries: newCfg.RoutineProbeRetryCount(),
		})
		if err := m.monitorMgr.SetProbeTarget(newCfg.Management.ProbeTarget); err != nil {
			m.logger.Warnf("apply reloaded probe target: %v", err)
		}
		m.monitorMgr.SetHealthCheckInterval(effectiveHealthCheckInterval(newCfg))
		m.monitorMgr.RequestStartupProbeAllOnce()
	}
	if groupReloadErr != nil {
		m.logger.Warnf("base reload completed with group runtime errors: %v", groupReloadErr)
	} else {
		m.logger.Infof("reload completed successfully with %d nodes", len(newCfg.Nodes))
	}
	return groupReloadErr
}

func (m *Manager) retryDegradedGroupRuntimes(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	var result error
	for _, groupCfg := range cfg.Groups {
		if !groupCfg.Enabled || groupCfg.ID == 0 || m.GroupRuntimeStatus(groupCfg.ID).Status != "degraded" {
			continue
		}
		groupPool := storeGroupFromConfig(groupCfg)
		if err := m.applyGroupRuntime(ctx, groupPool, groupPool, applyModeForceNoRollback); err != nil {
			result = errors.Join(result, fmt.Errorf("retry degraded group %d: %w", groupCfg.ID, err))
		}
	}
	return result
}

func (m *Manager) syncGroupRuntimesAfterBaseReload(ctx context.Context, oldCfg, newCfg *config.Config) error {
	oldGroups := make(map[int64]*store.GroupPool)
	if oldCfg != nil {
		for index := range oldCfg.Groups {
			groupPool := storeGroupFromConfig(oldCfg.Groups[index])
			oldGroups[groupPool.ID] = groupPool
		}
	}
	newGroups := make(map[int64]*store.GroupPool)
	if newCfg != nil {
		for index := range newCfg.Groups {
			groupPool := storeGroupFromConfig(newCfg.Groups[index])
			newGroups[groupPool.ID] = groupPool
		}
	}
	ids := make(map[int64]struct{}, len(oldGroups)+len(newGroups))
	for id := range oldGroups {
		ids[id] = struct{}{}
	}
	for id := range newGroups {
		ids[id] = struct{}{}
	}
	var result error
	for id := range ids {
		if groupRuntimeTopologyEqual(oldCfg, newCfg, id) {
			continue
		}
		if err := m.applyGroupRuntime(ctx, oldGroups[id], newGroups[id], applyModeForceNoRollback); err != nil {
			result = errors.Join(result, fmt.Errorf("group %d: %w", id, err))
		}
	}
	return result
}

func groupRuntimeTopologyEqual(oldCfg, newCfg *config.Config, groupID int64) bool {
	oldGroup, oldOK := groupConfigByID(oldCfg, groupID)
	newGroup, newOK := groupConfigByID(newCfg, groupID)
	if oldOK != newOK {
		return false
	}
	if !oldOK {
		return true
	}
	if !groupRuntimeEqual(storeGroupFromConfig(oldGroup), storeGroupFromConfig(newGroup)) {
		return false
	}
	return reflect.DeepEqual(groupMemberNodes(oldCfg, oldGroup), groupMemberNodes(newCfg, newGroup))
}

func groupConfigByID(cfg *config.Config, groupID int64) (config.GroupPoolConfig, bool) {
	if cfg != nil {
		for _, groupCfg := range cfg.Groups {
			if groupCfg.ID == groupID {
				return groupCfg, true
			}
		}
	}
	return config.GroupPoolConfig{}, false
}

// groupMemberNodes reports the group's members as the running box will see them.
// The judgement itself belongs to internal/groupmember so that this comparison
// and the builder can never disagree about who is in the group.
func groupMemberNodes(cfg *config.Config, groupCfg config.GroupPoolConfig) []config.NodeConfig {
	if cfg == nil || !groupCfg.Enabled {
		return nil
	}
	return groupmember.Nodes(cfg, groupCfg, groupmember.WithTagNames(cfg.TagNames))
}

// groupMemberRuntimeShape reports the group's members reduced to what its running
// box is built from. Tags are stripped because they never reach the box — the
// builder deliberately keeps them out of the member metadata — so a node merely
// gaining a tag it was already qualified by must not count as a change.
func groupMemberRuntimeShape(cfg *config.Config, groupCfg config.GroupPoolConfig) []config.NodeConfig {
	members := groupMemberNodes(cfg, groupCfg)
	if members == nil {
		return nil
	}
	shape := make([]config.NodeConfig, len(members))
	for index, member := range members {
		member.Tags = nil
		shape[index] = member
	}
	return shape
}

func groupAppliedFingerprint(cfg *config.Config, groupCfg config.GroupPoolConfig) string {
	shape := struct {
		Enabled        bool
		BindAddress    string
		BindPort       uint16
		Protocol       string
		Username       string
		Password       string
		DispatchMode   string
		FailureWindow  time.Duration
		FailureLimit   int
		HealthInterval time.Duration
		Preferred      int64
		SkipCertVerify bool
		Members        []config.NodeConfig
	}{groupCfg.Enabled, groupCfg.BindAddress, groupCfg.BindPort, groupCfg.Protocol, groupCfg.Username, groupCfg.Password,
		groupCfg.DispatchMode, groupCfg.FailureWindow, groupCfg.FailureThreshold, groupCfg.HealthCheckInterval,
		groupCfg.CurrentActiveNodeID, cfg != nil && cfg.SkipCertVerify, groupMemberRuntimeShape(cfg, groupCfg)}
	encoded, _ := yaml.Marshal(shape)
	return string(encoded)
}

func groupListenerEqual(before, after *store.GroupPool) bool {
	if before == nil || after == nil {
		return false
	}
	return before.ID == after.ID && before.Enabled && after.Enabled && before.BindAddress == after.BindAddress &&
		before.BindPort == after.BindPort && before.Protocol == after.Protocol && before.Username == after.Username && before.Password == after.Password
}

func groupRuntimeNodesForConfig(cfg *config.Config, groupCfg config.GroupPoolConfig, generation uint64, instance *box.Box) map[string]runtimeNode {
	result := make(map[string]runtimeNode)
	for _, node := range groupMemberNodes(cfg, groupCfg) {
		tag, err := runtimetag.Format(node.NodeKey(), generation)
		if err != nil {
			continue
		}
		if instance != nil {
			if _, found := instance.Outbound().Outbound(tag); !found {
				continue
			}
		}
		result[runtimeNodeKey(node)] = runtimeNode{tag: tag, node: node}
	}
	return result
}

func storeGroupFromConfig(groupCfg config.GroupPoolConfig) *store.GroupPool {
	groupPool := &store.GroupPool{ID: groupCfg.ID, Name: groupCfg.Name, BindAddress: groupCfg.BindAddress,
		BindPort: groupCfg.BindPort, Protocol: groupCfg.Protocol, Username: groupCfg.Username, Password: groupCfg.Password,
		DispatchMode: groupCfg.DispatchMode, Regions: append([]string(nil), groupCfg.Regions...),
		ExplicitNodeIDs: append([]int64(nil), groupCfg.ExplicitNodeIDs...), ExcludedNodeIDs: append([]int64(nil), groupCfg.ExcludedNodeIDs...),
		TagWhitelist: append([]int64(nil), groupCfg.TagWhitelist...), TagBlacklist: append([]int64(nil), groupCfg.TagBlacklist...),
		TagFilterMatch:       groupCfg.TagFilterMatch,
		FailureWindowSeconds: int(groupCfg.FailureWindow / time.Second), FailureThreshold: groupCfg.FailureThreshold,
		HealthCheckSeconds: int(groupCfg.HealthCheckInterval / time.Second), CurrentActiveNodeID: groupCfg.CurrentActiveNodeID,
		Enabled: groupCfg.Enabled, SubscriptionEnabled: groupCfg.SubscriptionEnabled, SubscriptionToken: groupCfg.SubscriptionToken,
		SubscriptionMode: groupCfg.SubscriptionMode, ExternalHost: groupCfg.ExternalHost,
		CreatedAt: groupCfg.CreatedAt, UpdatedAt: groupCfg.UpdatedAt}
	for _, state := range groupCfg.NodeStates {
		groupPool.NodeStates = append(groupPool.NodeStates, store.GroupNodeState{GroupID: groupCfg.ID, NodeID: state.NodeID,
			FailureHistory: append([]int64(nil), state.FailureHistory...), Evicted: state.Evicted,
			LastError: state.LastError, EvictedAt: state.EvictedAt})
	}
	return groupPool
}

func runtimeConfigEqual(a, b *config.Config) bool {
	if a == nil || b == nil {
		return a == b
	}
	aYAML, aErr := yaml.Marshal(a)
	bYAML, bErr := yaml.Marshal(b)
	return aErr == nil && bErr == nil && bytes.Equal(aYAML, bYAML) && reflect.DeepEqual(a.Groups, b.Groups)
}

func incrementalReloadEligible(oldCfg, newCfg *config.Config) bool {
	if oldCfg == nil || newCfg == nil || len(oldCfg.Nodes) == 0 || len(newCfg.Nodes) == 0 || oldCfg.MultiPort.Enabled || newCfg.MultiPort.Enabled ||
		oldCfg.GeoIP.Enabled || newCfg.GeoIP.Enabled {
		return false
	}
	oldShape, newShape := oldCfg.Clone(), newCfg.Clone()
	oldShape.Nodes, newShape.Nodes = nil, nil
	return runtimeConfigEqual(oldShape, newShape)
}

func runtimeNodeKey(node config.NodeConfig) string {
	if node.ID != 0 {
		return fmt.Sprintf("id:%d", node.ID)
	}
	return "key:" + node.NodeKey()
}

func runtimeNodesForConfig(cfg *config.Config, generation uint64, instance *box.Box) map[string]runtimeNode {
	result := make(map[string]runtimeNode, len(cfg.Nodes))
	for _, node := range cfg.Nodes {
		tag, err := runtimetag.Format(node.NodeKey(), generation)
		if err == nil {
			if instance != nil {
				if _, found := instance.Outbound().Outbound(tag); !found {
					continue
				}
			}
			result[runtimeNodeKey(node)] = runtimeNode{tag: tag, node: node}
		}
	}
	return result
}

func runtimeMemberMeta(cfg *config.Config, node config.NodeConfig) pool.MemberMeta {
	meta := pool.MemberMeta{NodeID: node.ID, Name: node.Name, URI: node.URI, Mode: cfg.EntryMode(), Region: strings.ToLower(node.Region), Country: node.Country}
	if cfg.MultiPort.Enabled {
		meta.ListenAddress, meta.Port = cfg.MultiPort.Address, node.Port
	} else if cfg.Listener.Enabled {
		meta.ListenAddress, meta.Port = cfg.Listener.Address, cfg.Listener.Port
	}
	if meta.Region == "" {
		meta.Region, meta.Country = "other", "Unknown"
	}
	return meta
}

func (m *Manager) reloadNodesIncrementally(ctx context.Context, instance *box.Box, oldCfg, newCfg *config.Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	previous := make(map[string]runtimeNode, len(m.runtimeNodes))
	for key, value := range m.runtimeNodes {
		previous[key] = value
	}
	nextGeneration := m.runtimeGeneration + 1
	drainTimeout := m.drainTimeout
	m.mu.RUnlock()

	poolOutbound, found := instance.Outbound().Outbound(pool.Tag)
	if !found {
		return errors.New("incremental reload: shared pool not found")
	}
	runtimePool, ok := poolOutbound.(pool.RuntimePool)
	if !ok {
		return errors.New("incremental reload: shared pool has no runtime control interface")
	}

	nextRuntime := make(map[string]runtimeNode, len(newCfg.Nodes))
	specs := make([]pool.RuntimeMemberSpec, 0, len(newCfg.Nodes))
	newTags := make([]string, 0)
	createdTags := make([]string, 0)
	usedTags := make(map[string]struct{}, len(newCfg.Nodes))
	for _, node := range newCfg.Nodes {
		key := runtimeNodeKey(node)
		current, retained := previous[key]
		if retained && current.node.URI == node.URI && oldCfg.SkipCertVerify == newCfg.SkipCertVerify {
			current.node = node
			nextRuntime[key] = current
			specs = append(specs, pool.RuntimeMemberSpec{Tag: current.tag, Meta: runtimeMemberMeta(newCfg, node)})
			usedTags[current.tag] = struct{}{}
			continue
		}
		tag, err := runtimetag.Format(node.NodeKey(), nextGeneration)
		if err != nil {
			m.cleanupCreatedMembers(instance, createdTags)
			return fmt.Errorf("incremental reload node %q tag: %w", node.Name, err)
		}
		if _, collision := usedTags[tag]; collision {
			m.cleanupCreatedMembers(instance, createdTags)
			return fmt.Errorf("incremental reload runtime tag collision: %s", tag)
		}
		built, err := builder.BuildNodeOutbound(tag, node, newCfg.SkipCertVerify)
		if err != nil {
			m.cleanupCreatedMembers(instance, createdTags)
			return fmt.Errorf("build dynamic node %q: %w", node.Name, err)
		}
		if err := runtimePool.CreateMember(tag, built.Type, built.Options); err != nil {
			m.cleanupCreatedMembers(instance, createdTags)
			return fmt.Errorf("create dynamic node %q: %w", node.Name, err)
		}
		createdTags = append(createdTags, tag)
		newTags = append(newTags, tag)
		usedTags[tag] = struct{}{}
		nextRuntime[key] = runtimeNode{tag: tag, node: node}
		specs = append(specs, pool.RuntimeMemberSpec{Tag: tag, Meta: runtimeMemberMeta(newCfg, node)})
	}

	version, retired, err := runtimePool.ReconcileMembers(specs)
	if err != nil {
		m.cleanupCreatedMembers(instance, createdTags)
		return fmt.Errorf("publish incremental pool topology: %w", err)
	}
	m.applyConfigSettings(newCfg)
	m.mu.Lock()
	if m.currentBox != instance {
		m.mu.Unlock()
		return errors.New("incremental reload lost active box ownership after publication")
	}
	m.cfg = newCfg
	m.runtimeGeneration = nextGeneration
	m.runtimeNodes = nextRuntime
	if m.monitorServer != nil {
		m.monitorServer.SetConfig(newCfg)
	}
	listeners := append([]ConfigUpdateListener(nil), m.configListeners...)
	m.mu.Unlock()
	for _, listener := range listeners {
		listener.OnConfigUpdate(newCfg)
	}
	if m.monitorMgr != nil {
		m.monitorMgr.RequestProbeTagsOnce(newTags)
	}
	for _, retiredMember := range retired {
		if m.monitorMgr != nil {
			m.monitorMgr.UnregisterRuntimeTag(retiredMember.Tag())
		}
		go m.drainRetiredMember(instance, retiredMember, version, drainTimeout)
	}
	m.logger.Infof("incremental reload published pool snapshot v%d with %d nodes (%d new versions)", version, len(specs), len(newTags))
	return nil
}

func (m *Manager) cleanupCreatedMembers(instance *box.Box, tags []string) {
	for index := len(tags) - 1; index >= 0; index-- {
		created, _ := instance.Outbound().Outbound(tags[index])
		if err := removeOutboundSafely(instance, tags[index]); err != nil {
			if created != nil {
				_ = common.Close(created)
			}
			m.logger.Warnf("rollback dynamic outbound %s: %v", tags[index], err)
		}
	}
}

func removeOutboundSafely(instance *box.Box, tag string) (err error) {
	if instance == nil {
		return errors.New("runtime box is nil")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("sing-box Remove(%s) panicked after manager mutation: %v", tag, recovered)
		}
	}()
	return instance.Outbound().Remove(tag)
}

func (m *Manager) drainRetiredMember(instance *box.Box, member pool.RetiredMember, snapshotVersion uint64, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for !member.Drained() && (timeout <= 0 || time.Now().Before(deadline)) {
		time.Sleep(100 * time.Millisecond)
	}
	for !member.Drained() {
		refs, operations, active := member.Counts()
		m.logger.Warnf("retired outbound %s from snapshot v%d still draining (snapshot_refs=%d operations=%d active=%d); retaining it", member.Tag(), snapshotVersion, refs, operations, active)
		time.Sleep(5 * time.Second)
	}
	if err := removeOutboundSafely(instance, member.Tag()); err != nil {
		// Remove may already have deleted the manager lookup. Never retry it;
		// close through the saved ref retained by RetiredMember.
		closeErr := member.Close()
		m.logger.Errorf("remove retired outbound %s failed after manager mutation: %v (saved close: %v)", member.Tag(), err, closeErr)
	}
	member.ReleaseState()
}

func effectiveHealthCheckInterval(cfg *config.Config) time.Duration {
	if cfg == nil {
		return 2 * time.Hour
	}
	interval := cfg.Management.HealthCheckInterval
	if interval <= 0 {
		interval = 2 * time.Hour
	}
	return interval
}

func (m *Manager) waitForConnectionsToDrain(timeout time.Duration) {
	active := pool.ActiveConnections()
	if active == 0 || timeout <= 0 {
		return
	}
	m.logger.Infof("waiting up to %s for %d active connections to drain", timeout, active)
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for active > 0 && time.Now().Before(deadline) {
		<-ticker.C
		active = pool.ActiveConnections()
	}
	if active > 0 {
		m.logger.Warnf("drain timeout reached with %d active connections", active)
	} else {
		m.logger.Infof("active connections drained")
	}
}

// AddConfigListener registers a listener to be notified when config changes after reload.
func (m *Manager) AddConfigListener(l ConfigUpdateListener) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configListeners = append(m.configListeners, l)
}

// rollbackToOldConfig attempts to restart with the previous configuration.
func (m *Manager) rollbackToOldConfig(ctx context.Context, oldCfg *config.Config) {
	if oldCfg == nil {
		return
	}
	m.logger.Warnf("attempting rollback to previous config...")
	instance, err := m.createBaseBox(ctx, oldCfg)
	if err != nil {
		m.logger.Errorf("rollback failed to create box: %v", err)
		return
	}
	if err := instance.Start(); err != nil {
		_ = instance.Close()
		m.logger.Errorf("rollback failed to start box: %v", err)
		return
	}
	m.mu.Lock()
	m.currentBox = instance
	m.cfg = oldCfg
	m.mu.Unlock()
	m.logger.Infof("rollback successful")
}

// Close terminates the active instance and auxiliary components.
func (m *Manager) Close() error {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	groupErr := m.closeAllGroupRuntimes("stopped", "")

	m.mu.Lock()
	defer m.mu.Unlock()

	err := groupErr
	if m.currentBox != nil {
		if closeErr := m.currentBox.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		m.currentBox = nil
	}
	if m.monitorServer != nil {
		m.monitorServer.Shutdown(context.Background())
		m.monitorServer = nil
	}
	if m.monitorMgr != nil {
		m.monitorMgr.Stop()
		m.monitorMgr = nil
		m.healthCheckStarted = false
	}
	m.baseCtx = nil
	pool.ResetAllRuntimeState()
	return err
}

// MonitorManager returns the shared monitor manager.
func (m *Manager) MonitorManager() *monitor.Manager {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.monitorMgr
}

// MonitorServer returns the monitor HTTP server.
func (m *Manager) MonitorServer() *monitor.Server {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.monitorServer
}

func (m *Manager) groupSlot(groupID int64) *groupRuntimeSlot {
	m.groupSlotsMu.Lock()
	defer m.groupSlotsMu.Unlock()
	if slot := m.groupSlots[groupID]; slot != nil {
		return slot
	}
	slot := &groupRuntimeSlot{gate: make(chan struct{}, 1), state: monitor.GroupRuntimeStatus{Status: "stopped"},
		members: make(map[string]runtimeNode), retiring: make(map[string]*groupRetirement)}
	slot.gate <- struct{}{}
	m.groupSlots[groupID] = slot
	return slot
}

func acquireGroupSlot(ctx context.Context, slot *groupRuntimeSlot) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-slot.gate:
		return nil
	}
}

func releaseGroupSlot(slot *groupRuntimeSlot) { slot.gate <- struct{}{} }

func (m *Manager) setGroupRuntimeStatus(groupID int64, status, runtimeError string) {
	if groupID == 0 {
		return
	}
	slot := m.groupSlot(groupID)
	slot.mu.Lock()
	slot.state = monitor.GroupRuntimeStatus{Status: status, Error: runtimeError}
	slot.mu.Unlock()
}

func (m *Manager) GroupRuntimeStatus(groupID int64) monitor.GroupRuntimeStatus {
	slot := m.groupSlot(groupID)
	slot.mu.RLock()
	defer slot.mu.RUnlock()
	return slot.state
}

func (m *Manager) startInitialGroup(ctx context.Context, groupID int64) error {
	slot := m.groupSlot(groupID)
	if err := acquireGroupSlot(ctx, slot); err != nil {
		return err
	}
	defer releaseGroupSlot(slot)
	m.setGroupRuntimeStatus(groupID, "starting", "")
	m.mu.RLock()
	cfg := m.cfg.Clone()
	baseCtx := m.baseCtx
	m.mu.RUnlock()
	if baseCtx == nil {
		baseCtx = ctx
	}
	instance, err := m.createGroupBox(baseCtx, cfg, groupID)
	if err == nil {
		err = instance.Start()
	}
	if err != nil {
		if instance != nil {
			_ = instance.Close()
		}
		return err
	}
	slot.mu.Lock()
	slot.box = instance
	slot.state = monitor.GroupRuntimeStatus{Status: "ready"}
	if groupCfg, ok := groupConfigByID(cfg, groupID); ok {
		slot.members = groupRuntimeNodesForConfig(cfg, groupCfg, m.runtimeGeneration, instance)
		slot.appliedFingerprint = groupAppliedFingerprint(cfg, groupCfg)
	}
	slot.mu.Unlock()
	return nil
}

// applyMode says how applyGroupRuntime should treat a rebuild request. The
// distinction that matters is not "rebuild or not" but what happens when the
// rebuild fails, which is why this is three values rather than a bool.
type applyMode uint8

const (
	// applyModeDelta skips the rebuild entirely when the group definition is
	// unchanged. It is the default for an operator editing one group.
	applyModeDelta applyMode = iota
	// applyModeForceNoRollback rebuilds unconditionally and leaves the group
	// stopped on failure. It is for the path that follows a base reload: the base
	// box has already moved on, so the old group box could not be recreated
	// against it even if we tried.
	applyModeForceNoRollback
	// applyModeForceWithRollback rebuilds unconditionally but restores the
	// previous box on failure. It is for rebuilds driven by node facts — tags,
	// regions — where the group definition itself is untouched and still valid,
	// so falling back to the running listener is strictly better than dropping it.
	applyModeForceWithRollback
)

func (mode applyMode) forced() bool { return mode != applyModeDelta }

func (m *Manager) ApplyGroupRuntime(ctx context.Context, before, after *store.GroupPool) error {
	return m.applyGroupRuntime(ctx, before, after, applyModeDelta)
}

func (m *Manager) applyGroupRuntime(ctx context.Context, before, after *store.GroupPool, mode applyMode) error {
	groupID := int64(0)
	if after != nil {
		groupID = after.ID
	} else if before != nil {
		groupID = before.ID
	}
	if groupID == 0 {
		return errors.New("invalid group ID")
	}
	slot := m.groupSlot(groupID)
	if err := acquireGroupSlot(ctx, slot); err != nil {
		return err
	}
	defer releaseGroupSlot(slot)

	slot.mu.Lock()
	oldBox := slot.box
	slot.state = monitor.GroupRuntimeStatus{Status: "reconfiguring"}
	slot.mu.Unlock()

	if after == nil || !after.Enabled {
		if oldBox != nil {
			cancelGroupRetirements(slot)
			if err := oldBox.Close(); err != nil {
				m.mu.RLock()
				baseCtx := m.baseCtx
				m.mu.RUnlock()
				rollbackErr := m.restoreGroupBox(baseCtx, slot, before)
				if rollbackErr != nil {
					m.setGroupRuntimeStatus(groupID, "error", rollbackErr.Error())
					return fmt.Errorf("stop group runtime: %w; rollback runtime: %v", err, rollbackErr)
				}
				return fmt.Errorf("stop group runtime: %w", err)
			}
		}
		slot.mu.Lock()
		slot.box = nil
		slot.state = monitor.GroupRuntimeStatus{Status: "stopped"}
		slot.mu.Unlock()
		m.replaceCachedGroup(after)
		return nil
	}

	candidateCfg := m.configWithGroup(after)
	candidateGroup, found := groupConfigByID(candidateCfg, groupID)
	if !found {
		m.setGroupRuntimeStatus(groupID, "error", "group configuration is missing")
		return errors.New("group configuration is missing")
	}
	fingerprint := groupAppliedFingerprint(candidateCfg, candidateGroup)
	slot.mu.RLock()
	alreadyApplied := slot.appliedFingerprint == fingerprint
	slot.mu.RUnlock()
	if !mode.forced() && alreadyApplied {
		m.setGroupRuntimeStatus(groupID, "ready", "")
		m.replaceCachedGroup(after)
		return nil
	}
	if oldBox != nil && groupListenerEqual(before, after) {
		if err := m.applyGroupRuntimeIncremental(candidateCfg, candidateGroup, slot, oldBox, fingerprint); err != nil {
			if mode.forced() {
				m.setGroupRuntimeStatus(groupID, "degraded", err.Error())
			} else {
				m.setGroupRuntimeStatus(groupID, "ready", "")
			}
			return err
		}
		m.replaceCachedGroup(after)
		return nil
	}
	m.mu.RLock()
	baseCtx := m.baseCtx
	m.mu.RUnlock()
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	newBox, err := m.createGroupBox(baseCtx, candidateCfg, groupID)
	if err != nil {
		if mode == applyModeForceNoRollback {
			if oldBox != nil {
				_ = oldBox.Close()
			}
			slot.mu.Lock()
			slot.box = nil
			slot.state = monitor.GroupRuntimeStatus{Status: "error", Error: err.Error()}
			slot.mu.Unlock()
			m.replaceCachedGroup(after)
			return err
		}
		// The old box was never closed, so it is still serving: leave it alone.
		if oldBox != nil {
			m.setGroupRuntimeStatus(groupID, "ready", "")
		} else {
			m.setGroupRuntimeStatus(groupID, "error", err.Error())
		}
		return err
	}
	if oldBox != nil {
		cancelGroupRetirements(slot)
		_ = oldBox.Close()
	}
	if err = newBox.Start(); err != nil {
		_ = newBox.Close()
		if mode == applyModeForceNoRollback {
			slot.mu.Lock()
			slot.box = nil
			slot.state = monitor.GroupRuntimeStatus{Status: "error", Error: err.Error()}
			slot.mu.Unlock()
			m.replaceCachedGroup(after)
			return fmt.Errorf("start new group runtime: %w", err)
		}
		rollbackErr := m.restoreGroupBox(baseCtx, slot, before)
		if rollbackErr != nil {
			m.setGroupRuntimeStatus(groupID, "error", rollbackErr.Error())
			return fmt.Errorf("start new group runtime: %w; rollback runtime: %v", err, rollbackErr)
		}
		return fmt.Errorf("start new group runtime: %w", err)
	}
	slot.mu.Lock()
	slot.box = newBox
	slot.state = monitor.GroupRuntimeStatus{Status: "ready"}
	slot.members = groupRuntimeNodesForConfig(candidateCfg, candidateGroup, m.runtimeGeneration, newBox)
	slot.appliedFingerprint = fingerprint
	slot.mu.Unlock()
	m.replaceCachedGroup(after)
	return nil
}

func cancelGroupRetirements(slot *groupRuntimeSlot) {
	if slot == nil {
		return
	}
	slot.mu.Lock()
	for tag, ticket := range slot.retiring {
		ticket.stop()
		delete(slot.retiring, tag)
	}
	slot.mu.Unlock()
}

func (m *Manager) applyGroupRuntimeIncremental(cfg *config.Config, groupCfg config.GroupPoolConfig, slot *groupRuntimeSlot, instance *box.Box, fingerprint string) (resultErr error) {
	poolTag := fmt.Sprintf("group-pool-%d", groupCfg.ID)
	poolOutbound, found := instance.Outbound().Outbound(poolTag)
	if !found {
		return fmt.Errorf("group runtime pool %s not found", poolTag)
	}
	runtimePool, ok := poolOutbound.(pool.RuntimePool)
	if !ok {
		return fmt.Errorf("group runtime pool %s has no transaction interface", poolTag)
	}
	desiredNodes := groupMemberNodes(cfg, groupCfg)
	if len(desiredNodes) == 0 {
		return errors.New("group update has no matching nodes; retaining current members")
	}
	m.mu.RLock()
	generation := m.runtimeGeneration
	m.mu.RUnlock()
	slot.mu.Lock()
	previous := make(map[string]runtimeNode, len(slot.members))
	for key, item := range slot.members {
		previous[key] = item
	}
	retiring := make(map[string]*groupRetirement, len(slot.retiring))
	for tag, ticket := range slot.retiring {
		retiring[tag] = ticket
	}
	slot.mu.Unlock()

	next := make(map[string]runtimeNode, len(desiredNodes))
	specs := make([]pool.RuntimeMemberSpec, 0, len(desiredNodes))
	createdTags := make([]string, 0)
	usedTags := make(map[string]struct{}, len(desiredNodes))
	cancelledTickets := make([]*groupRetirement, 0)
	committed := false
	defer func() {
		if committed {
			return
		}
		for _, old := range cancelledTickets {
			replacement := &groupRetirement{cancel: make(chan struct{}), node: old.node, member: old.member, version: old.version}
			slot.mu.Lock()
			if slot.retiring[old.member.Tag()] == nil {
				slot.retiring[old.member.Tag()] = replacement
				go m.drainRetiredGroupMember(instance, groupCfg.ID, slot, replacement, old.member, old.version)
			}
			slot.mu.Unlock()
		}
	}()
	for _, node := range desiredNodes {
		key := runtimeNodeKey(node)
		current, retained := previous[key]
		if retained && current.node.URI == node.URI {
			current.node = node
			next[key] = current
			specs = append(specs, pool.RuntimeMemberSpec{Tag: current.tag, Meta: runtimeMemberMeta(cfg, node)})
			usedTags[current.tag] = struct{}{}
			continue
		}
		tag, err := runtimetag.Format(node.NodeKey(), generation)
		if err != nil {
			m.cleanupCreatedMembers(instance, createdTags)
			return fmt.Errorf("group node %q tag: %w", node.Name, err)
		}
		if _, collision := usedTags[tag]; collision {
			m.cleanupCreatedMembers(instance, createdTags)
			return fmt.Errorf("group runtime tag collision: %s", tag)
		}
		if ticket := retiring[tag]; ticket != nil && ticket.node.node.URI == node.URI {
			slot.mu.Lock()
			if slot.retiring[tag] == ticket {
				ticket.stop()
				if _, stillRegistered := instance.Outbound().Outbound(tag); stillRegistered {
					cancelledTickets = append(cancelledTickets, ticket)
					delete(slot.retiring, tag)
					slot.mu.Unlock()
					next[key] = runtimeNode{tag: tag, node: node}
					specs = append(specs, pool.RuntimeMemberSpec{Tag: tag, Meta: runtimeMemberMeta(cfg, node)})
					usedTags[tag] = struct{}{}
					continue
				}
			}
			slot.mu.Unlock()
		}
		built, err := builder.BuildNodeOutbound(tag, node, cfg.SkipCertVerify)
		if err != nil {
			m.cleanupCreatedMembers(instance, createdTags)
			return fmt.Errorf("build group node %q: %w", node.Name, err)
		}
		if err := runtimePool.CreateMember(tag, built.Type, built.Options); err != nil {
			m.cleanupCreatedMembers(instance, createdTags)
			return fmt.Errorf("create group node %q: %w", node.Name, err)
		}
		createdTags = append(createdTags, tag)
		next[key] = runtimeNode{tag: tag, node: node}
		specs = append(specs, pool.RuntimeMemberSpec{Tag: tag, Meta: runtimeMemberMeta(cfg, node)})
		usedTags[tag] = struct{}{}
	}
	initial := make(map[string]group.GroupInitialState, len(specs))
	stateByNodeID := make(map[int64]config.GroupNodeStateConfig, len(groupCfg.NodeStates))
	for _, state := range groupCfg.NodeStates {
		stateByNodeID[state.NodeID] = state
	}
	for _, spec := range specs {
		state := stateByNodeID[spec.Meta.NodeID]
		initial[spec.Tag] = group.GroupInitialState{NodeID: spec.Meta.NodeID,
			FailureHistory: append([]int64(nil), state.FailureHistory...), Evicted: state.Evicted,
			LastError: state.LastError, EvictedAt: state.EvictedAt}
	}
	prepared, err := runtimePool.PrepareUpdate(pool.RuntimeUpdate{Members: specs, Mode: groupCfg.DispatchMode,
		FailureWindow: groupCfg.FailureWindow, FailureThreshold: groupCfg.FailureThreshold,
		HealthCheckInterval: groupCfg.HealthCheckInterval, PreferredNodeID: groupCfg.CurrentActiveNodeID,
		InitialGroupState: initial})
	if err != nil {
		m.cleanupCreatedMembers(instance, createdTags)
		return fmt.Errorf("prepare group runtime: %w", err)
	}
	version, retiredMembers, err := prepared.Commit()
	if err != nil {
		prepared.Rollback()
		m.cleanupCreatedMembers(instance, createdTags)
		return fmt.Errorf("commit group runtime: %w", err)
	}
	committed = true
	slot.mu.Lock()
	slot.members = next
	slot.appliedFingerprint = fingerprint
	slot.state = monitor.GroupRuntimeStatus{Status: "ready"}
	for _, retired := range retiredMembers {
		ticket := &groupRetirement{cancel: make(chan struct{}), member: retired, version: version}
		for _, item := range previous {
			if item.tag == retired.Tag() {
				ticket.node = item
				break
			}
		}
		if old := slot.retiring[retired.Tag()]; old != nil {
			old.stop()
		}
		slot.retiring[retired.Tag()] = ticket
		go m.drainRetiredGroupMember(instance, groupCfg.ID, slot, ticket, retired, version)
	}
	slot.mu.Unlock()
	m.logger.Infof("group %q published pool snapshot v%d with %d members", groupCfg.Name, version, len(specs))
	return nil
}

func (m *Manager) drainRetiredGroupMember(instance *box.Box, groupID int64, slot *groupRuntimeSlot, ticket *groupRetirement, member pool.RetiredMember, version uint64) {
	timeout := m.drainTimeout
	deadline := time.Now().Add(timeout)
	for !member.Drained() && (timeout <= 0 || time.Now().Before(deadline)) {
		select {
		case <-ticket.cancel:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	for !member.Drained() {
		refs, operations, active := member.Counts()
		m.logger.Warnf("group %d outbound %s from snapshot v%d still draining (snapshot_refs=%d operations=%d active=%d); retaining it",
			groupID, member.Tag(), version, refs, operations, active)
		select {
		case <-ticket.cancel:
			return
		case <-time.After(5 * time.Second):
		}
	}
	slot.mu.Lock()
	if slot.retiring[member.Tag()] != ticket {
		slot.mu.Unlock()
		return
	}
	select {
	case <-ticket.cancel:
		delete(slot.retiring, member.Tag())
		slot.mu.Unlock()
		return
	default:
	}
	// Keep the retirement claim while Remove mutates sing-box's manager. A
	// concurrent re-add either cancels before this point or waits until removal
	// has completed and then creates a fresh outbound; it can never lose its new
	// object to a stale drain goroutine.
	err := removeOutboundSafely(instance, member.Tag())
	delete(slot.retiring, member.Tag())
	slot.mu.Unlock()
	if err != nil {
		closeErr := member.Close()
		m.logger.Errorf("remove retired group outbound %s failed after manager mutation: %v (saved close: %v)", member.Tag(), err, closeErr)
	}
	member.ReleaseState()
}

func (m *Manager) restoreGroupBox(ctx context.Context, slot *groupRuntimeSlot, previous *store.GroupPool) error {
	if previous == nil || !previous.Enabled {
		slot.mu.Lock()
		slot.box = nil
		slot.state = monitor.GroupRuntimeStatus{Status: "stopped"}
		slot.mu.Unlock()
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rollbackCfg := m.configWithGroup(previous)
	instance, err := m.createGroupBox(ctx, rollbackCfg, previous.ID)
	if err == nil {
		err = instance.Start()
	}
	if err != nil {
		if instance != nil {
			_ = instance.Close()
		}
		return err
	}
	slot.mu.Lock()
	slot.box = instance
	slot.state = monitor.GroupRuntimeStatus{Status: "ready"}
	slot.mu.Unlock()
	return nil
}

func (m *Manager) ActivateGroupMember(ctx context.Context, groupID, nodeID int64) error {
	slot := m.groupSlot(groupID)
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := acquireGroupSlot(waitCtx, slot); err != nil {
		return errors.New("分组运行时尚未就绪")
	}
	defer releaseGroupSlot(slot)
	status := m.GroupRuntimeStatus(groupID)
	if status.Status != "ready" && status.Status != "degraded" {
		return errors.New("分组运行时尚未就绪")
	}
	if err := group.ActivateMember(groupID, nodeID); err != nil {
		if strings.Contains(err.Error(), "runtime not found") {
			return errors.New("分组运行时尚未就绪")
		}
		return err
	}
	return nil
}

func (m *Manager) configWithGroup(groupPool *store.GroupPool) *config.Config {
	m.mu.RLock()
	cfg := m.cfg.Clone()
	m.mu.RUnlock()
	groups := make([]config.GroupPoolConfig, 0, len(cfg.Groups)+1)
	for _, existing := range cfg.Groups {
		if groupPool == nil || existing.ID != groupPool.ID {
			groups = append(groups, existing)
		}
	}
	if groupPool != nil {
		groups = append(groups, GroupConfigsFromStore([]store.GroupPool{*groupPool})[0])
	}
	cfg.Groups = groups
	return cfg
}

func (m *Manager) replaceCachedGroup(groupPool *store.GroupPool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg == nil {
		return
	}
	groups := make([]config.GroupPoolConfig, 0, len(m.cfg.Groups)+1)
	for _, existing := range m.cfg.Groups {
		if groupPool == nil || existing.ID != groupPool.ID {
			groups = append(groups, existing)
		}
	}
	if groupPool != nil {
		groups = append(groups, GroupConfigsFromStore([]store.GroupPool{*groupPool})[0])
	}
	m.cfg.Groups = groups
}

// loadTagNames snapshots the tag ID → name mapping group membership is resolved
// through. An ID missing from the result is treated as a tag no node can carry,
// so a deleted tag narrows the groups referencing it instead of widening them.
func (m *Manager) loadTagNames(ctx context.Context) (map[int64]string, error) {
	if m.store == nil {
		return nil, nil
	}
	tags, err := m.store.ListTags(ctx)
	if err != nil {
		return nil, err
	}
	tagNames := make(map[int64]string, len(tags))
	for _, tag := range tags {
		tagNames[tag.ID] = tag.Name
	}
	return tagNames, nil
}

// ApplyGroupMembershipChanges reacts to node facts changing — tags being
// recomputed, a landing region being classified — by updating only the group
// pool snapshots whose member set actually moved.
//
// The base box is never reloaded. A full reload rebuilds every listener in the
// process, which drops live connections on groups that did not change, and node
// facts change far more often than group definitions do. The cached config is
// refreshed even when no group is affected, so the next reload still sees the
// current tags.
func (m *Manager) ApplyGroupMembershipChanges(ctx context.Context, changedNodeIDs []int64) error {
	if len(changedNodeIDs) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if m.store == nil {
		return nil
	}
	changed := make(map[int64]struct{}, len(changedNodeIDs))
	ids := make([]int64, 0, len(changedNodeIDs))
	for _, nodeID := range changedNodeIDs {
		if nodeID <= 0 {
			continue
		}
		if _, seen := changed[nodeID]; seen {
			continue
		}
		changed[nodeID] = struct{}{}
		ids = append(ids, nodeID)
	}
	if len(ids) == 0 {
		return nil
	}
	// Read the store before taking the lock: the fresh facts are what the new
	// membership is judged against, and the query must not block reloads.
	nodes, err := m.store.ListNodes(ctx, store.NodeFilter{NodeIDs: ids})
	if err != nil {
		return fmt.Errorf("list changed nodes: %w", err)
	}
	tagNames, err := m.loadTagNames(ctx)
	if err != nil {
		return fmt.Errorf("list tags: %w", err)
	}
	storedByID := make(map[int64]store.Node, len(nodes))
	for _, node := range nodes {
		storedByID[node.ID] = node
	}

	m.mu.Lock()
	if m.cfg == nil {
		m.mu.Unlock()
		return nil
	}
	before := m.cfg.Clone()
	for idx := range m.cfg.Nodes {
		nodeID := m.cfg.Nodes[idx].ID
		if _, affected := changed[nodeID]; !affected {
			continue
		}
		// A node the store no longer reports is left as it is: it is on its way out
		// through a config reload, and clearing its facts here would shuffle groups
		// on the way.
		stored, found := storedByID[nodeID]
		if !found {
			continue
		}
		m.cfg.Nodes[idx].Tags = append([]string(nil), stored.Tags...)
		// The landing classification is a membership fact too, and this is the path
		// a region change arrives through as well.
		m.cfg.Nodes[idx].Region = stored.Region
		m.cfg.Nodes[idx].Country = stored.Country
	}
	m.cfg.TagNames = tagNames
	after := m.cfg.Clone()
	m.mu.Unlock()

	var result error
	for index := range after.Groups {
		groupCfg := after.Groups[index]
		if !groupCfg.Enabled || groupCfg.ID == 0 {
			continue
		}
		previous, found := groupConfigByID(before, groupCfg.ID)
		if found && reflect.DeepEqual(groupMemberRuntimeShape(before, previous), groupMemberRuntimeShape(after, groupCfg)) {
			continue
		}
		groupPool := storeGroupFromConfig(groupCfg)
		// The group definition itself did not change, so the running listener is
		// still a valid fallback: update forcibly while retaining it on failure.
		if err := m.applyGroupRuntime(ctx, groupPool, groupPool, applyModeForceWithRollback); err != nil {
			result = errors.Join(result, fmt.Errorf("group %d: %w", groupCfg.ID, err))
			continue
		}
		m.logger.Infof("group %q updated in place after node membership change", groupCfg.Name)
	}
	return result
}

// groupRuntimeEqual reports whether two revisions of a group definition describe
// the same listener. Every field that can change who is in the group has to be
// here: editing only the tag whitelist would otherwise be a silent no-op.
func groupRuntimeEqual(before, after *store.GroupPool) bool {
	if before == nil || after == nil {
		return before == after
	}
	return before.ID == after.ID && before.BindAddress == after.BindAddress && before.BindPort == after.BindPort &&
		before.Protocol == after.Protocol && before.Username == after.Username && before.Password == after.Password &&
		before.DispatchMode == after.DispatchMode && reflect.DeepEqual(before.Regions, after.Regions) &&
		reflect.DeepEqual(before.ExplicitNodeIDs, after.ExplicitNodeIDs) && reflect.DeepEqual(before.ExcludedNodeIDs, after.ExcludedNodeIDs) &&
		reflect.DeepEqual(before.TagWhitelist, after.TagWhitelist) && reflect.DeepEqual(before.TagBlacklist, after.TagBlacklist) &&
		before.TagFilterMatch == after.TagFilterMatch &&
		before.FailureWindowSeconds == after.FailureWindowSeconds && before.FailureThreshold == after.FailureThreshold &&
		before.HealthCheckSeconds == after.HealthCheckSeconds && before.Enabled == after.Enabled
}

// createBaseBox builds the application-wide instance without group listeners.
func (m *Manager) createBaseBox(ctx context.Context, cfg *config.Config) (*box.Box, error) {
	m.mu.RLock()
	generation := m.runtimeGeneration
	m.mu.RUnlock()
	if generation == 0 {
		generation = runtimetag.InitialVersion
	}
	return m.createBaseBoxVersion(ctx, cfg, generation)
}

func (m *Manager) createBaseBoxVersion(ctx context.Context, cfg *config.Config, generation uint64) (*box.Box, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	if m.monitorMgr == nil {
		return nil, errors.New("monitor manager not initialized")
	}

	opts, err := builder.BuildBaseVersion(cfg, generation)
	if err != nil {
		return nil, fmt.Errorf("build sing-box options: %w", err)
	}
	return m.createBoxFromOptions(ctx, opts)
}

func (m *Manager) createGroupBox(ctx context.Context, cfg *config.Config, groupID int64) (*box.Box, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	m.mu.RLock()
	generation := m.runtimeGeneration
	m.mu.RUnlock()
	if generation == 0 {
		generation = runtimetag.InitialVersion
	}
	opts, err := builder.BuildGroupVersion(cfg, groupID, generation)
	if err != nil {
		return nil, err
	}
	return m.createBoxFromOptions(ctx, opts)
}

func (m *Manager) createBoxFromOptions(ctx context.Context, opts option.Options) (*box.Box, error) {

	inboundRegistry := include.InboundRegistry()
	outboundRegistry := include.OutboundRegistry()
	pool.Register(outboundRegistry)
	endpointRegistry := include.EndpointRegistry()
	dnsRegistry := include.DNSTransportRegistry()
	serviceRegistry := include.ServiceRegistry()

	boxCtx := box.Context(ctx, inboundRegistry, outboundRegistry, endpointRegistry, dnsRegistry, serviceRegistry)
	boxCtx = monitor.ContextWith(boxCtx, m.monitorMgr)

	instance, err := box.New(box.Options{Context: boxCtx, Options: opts})
	if err != nil {
		return nil, fmt.Errorf("create sing-box instance: %w", err)
	}
	return instance, nil
}

// gracefulSwitch swaps the current box with a new one.
func (m *Manager) gracefulSwitch(newBox *box.Box) error {
	if newBox == nil {
		return errors.New("new box is nil")
	}

	m.mu.Lock()
	old := m.currentBox
	m.currentBox = newBox
	drainTimeout := m.drainTimeout
	m.mu.Unlock()

	if old != nil {
		go m.drainOldBox(old, drainTimeout)
	}

	m.logger.Infof("switched to new instance, draining old for %s", drainTimeout)
	return nil
}

// drainOldBox waits for drain timeout then closes the old box.
func (m *Manager) drainOldBox(oldBox *box.Box, timeout time.Duration) {
	if oldBox == nil {
		return
	}
	if timeout > 0 {
		time.Sleep(timeout)
	}
	if err := oldBox.Close(); err != nil {
		m.logger.Errorf("failed to close old instance: %v", err)
		return
	}
	m.logger.Infof("old instance closed after %s drain", timeout)
}

// waitForHealthCheck polls until enough nodes are available or timeout.
func (m *Manager) waitForHealthCheck(timeout time.Duration) error {
	if m.monitorMgr == nil || m.minAvailableNodes <= 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = defaultHealthCheckTimeout
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(healthCheckPollInterval)
	defer ticker.Stop()

	for {
		available, total := m.availableNodeCount()
		if available >= m.minAvailableNodes {
			m.logger.Infof("health check passed: %d/%d nodes available", available, total)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout: %d/%d nodes available (need >= %d)", available, total, m.minAvailableNodes)
		}
		<-ticker.C
	}
}

// availableNodeCount returns (available, total) node counts.
func (m *Manager) availableNodeCount() (int, int) {
	if m.monitorMgr == nil {
		return 0, 0
	}
	snapshots := m.monitorMgr.Snapshot()
	total := len(snapshots)
	available := 0
	for _, snap := range snapshots {
		if snap.InitialCheckDone && snap.Available {
			available++
		}
	}
	return available, total
}

// ensureMonitor initializes monitor manager and server if needed.
func (m *Manager) ensureMonitor(ctx context.Context) error {
	m.mu.Lock()
	if m.monitorMgr != nil {
		m.mu.Unlock()
		return nil
	}

	monitorMgr, err := monitor.NewManager(m.monitorCfg)
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("init monitor manager: %w", err)
	}
	monitorMgr.SetLogger(monitorLoggerAdapter{logger: m.logger})
	m.monitorMgr = monitorMgr

	var serverToStart *monitor.Server
	if m.monitorCfg.Enabled {
		if m.monitorServer == nil {
			serverToStart = monitor.NewServer(m.monitorCfg, monitorMgr, log.Default())
			serverToStart.SetConfig(m.cfg)
			serverToStart.SetStore(m.store)
			m.monitorServer = serverToStart
		}
		// Set NodeManager for config CRUD endpoints
		if m.monitorServer != nil {
			m.monitorServer.SetNodeManager(m)
		}
		// Note: StartPeriodicHealthCheck is called after nodes are registered in Start()
	}
	m.mu.Unlock()

	if serverToStart != nil {
		serverToStart.Start(ctx)
	}
	return nil
}

// applyConfigSettings extracts runtime settings from config.
func (m *Manager) applyConfigSettings(cfg *config.Config) {
	if cfg == nil {
		return
	}
	if cfg.SubscriptionRefresh.DrainTimeout > 0 {
		m.drainTimeout = cfg.SubscriptionRefresh.DrainTimeout
	} else if m.drainTimeout == 0 {
		m.drainTimeout = defaultDrainTimeout
	}
	m.minAvailableNodes = cfg.SubscriptionRefresh.MinAvailableNodes
}

// defaultLogger is the fallback logger using standard log.
type defaultLogger struct{}

func (defaultLogger) Infof(format string, args ...any) {
	log.Printf("[boxmgr] "+format, args...)
}

func (defaultLogger) Warnf(format string, args ...any) {
	log.Printf("[boxmgr] WARN: "+format, args...)
}

func (defaultLogger) Errorf(format string, args ...any) {
	log.Printf("[boxmgr] ERROR: "+format, args...)
}

// monitorLoggerAdapter adapts Logger to monitor.Logger interface.
type monitorLoggerAdapter struct {
	logger Logger
}

func (a monitorLoggerAdapter) Info(args ...any) {
	if a.logger != nil {
		a.logger.Infof("%s", fmt.Sprint(args...))
	}
}

func (a monitorLoggerAdapter) Warn(args ...any) {
	if a.logger != nil {
		a.logger.Warnf("%s", fmt.Sprint(args...))
	}
}

// --- NodeManager interface implementation ---

var errConfigUnavailable = errors.New("config is not initialized")

// ListConfigNodes returns a copy of all configured nodes.
// If a Store is available, it merges the disabled status from the store
// and also includes disabled nodes that are not in the active config.
// Port numbers are taken from the active config (m.cfg.Nodes) since they
// are dynamically assigned by NormalizeWithPortMap and may not be in the Store.
func (m *Manager) ListConfigNodes(ctx context.Context, subscriptionID *int64) ([]monitor.ManagedNodeConfig, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.cfg == nil {
		return nil, errConfigUnavailable
	}

	// If no store, just return active nodes
	if m.store == nil {
		result := make([]monitor.ManagedNodeConfig, 0, len(m.cfg.Nodes))
		for _, node := range m.cfg.Nodes {
			result = append(result, managedNodeConfig(node, nil))
		}
		return result, nil
	}

	// Build a lookup from URI → runtime port from the active config.
	// These ports are dynamically assigned by NormalizeWithPortMap and
	// reflect the actual listening ports in the current sing-box instance.
	runtimePorts := make(map[string]uint16, len(m.cfg.Nodes))
	for _, n := range m.cfg.Nodes {
		if n.Port > 0 {
			runtimePorts[n.NodeKey()] = n.Port
		}
	}

	// Fetch all nodes from store (including disabled ones)
	storeNodes, err := m.store.ListManagedNodes(ctx, subscriptionID)
	if err != nil {
		// Fallback to config nodes if store fails
		m.logger.Warnf("failed to list nodes from store: %v, falling back to config", err)
		result := make([]monitor.ManagedNodeConfig, 0, len(m.cfg.Nodes))
		for _, node := range m.cfg.Nodes {
			result = append(result, managedNodeConfig(node, nil))
		}
		return result, nil
	}

	// Build result from store nodes (preserves disabled status)
	// Merge runtime port assignments from active config
	result := make([]monitor.ManagedNodeConfig, 0, len(storeNodes))
	for _, n := range storeNodes {
		port := n.Port
		// Prefer runtime port from active config (dynamically assigned)
		identityKey := n.IdentityHash
		if identityKey == "" {
			if identity, identityErr := nodecodec.ParseURI(n.URI); identityErr == nil {
				identityKey = identity.Hash
			} else {
				identityKey = n.URI
			}
		}
		if runtimePort, ok := runtimePorts[identityKey]; ok && runtimePort > 0 {
			port = runtimePort
		}
		result = append(result, monitor.ManagedNodeConfig{
			ID:              n.ID,
			Name:            n.Name,
			URI:             n.URI,
			Port:            port,
			Username:        n.Username,
			Password:        n.Password,
			Region:          n.Region,
			Country:         n.Country,
			Tags:            n.Tags,
			Source:          config.NodeSource(n.Source),
			Disabled:        !n.Enabled,
			SubscriptionIDs: n.SubscriptionIDs,
		})
	}

	return result, nil
}

func managedNodeConfig(node config.NodeConfig, subscriptionIDs []int64) monitor.ManagedNodeConfig {
	if subscriptionIDs == nil {
		subscriptionIDs = []int64{}
	}
	return monitor.ManagedNodeConfig{ID: node.ID, Name: node.Name, URI: node.URI, Port: node.Port,
		Username: node.Username, Password: node.Password, Region: node.Region, Country: node.Country,
		Tags: node.Tags, Source: node.Source,
		Disabled: node.Disabled, SubscriptionIDs: subscriptionIDs}
}

// CreateNode adds a new node and persists it to the Store.
// Nodes added via the WebUI are always marked as "manual" source.
func (m *Manager) CreateNode(ctx context.Context, node config.NodeConfig) (config.NodeConfig, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return config.NodeConfig{}, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cfg == nil {
		return config.NodeConfig{}, errConfigUnavailable
	}

	normalized, err := m.prepareNodeLocked(node, "")
	if err != nil {
		return config.NodeConfig{}, err
	}

	normalized.Source = config.NodeSourceManual

	// Persist to Store if available
	if m.store != nil {
		identity, identityErr := nodecodec.ParseURI(normalized.URI)
		if identityErr != nil {
			return config.NodeConfig{}, fmt.Errorf("%w: %v", monitor.ErrInvalidNode, identityErr)
		}
		if existing, lookupErr := m.store.GetNodeByIdentity(ctx, identity.Hash); lookupErr != nil {
			return config.NodeConfig{}, fmt.Errorf("lookup node identity: %w", lookupErr)
		} else if existing != nil {
			return config.NodeConfig{}, &monitor.NodeDuplicateError{ExistingID: existing.ID, ExistingName: existing.Name}
		}
		storeNode := &store.Node{
			URI:      normalized.URI,
			Name:     normalized.Name,
			Source:   string(normalized.Source),
			Port:     normalized.Port,
			Username: normalized.Username,
			Password: normalized.Password,
			Enabled:  true,
		}
		if err := m.store.CreateNode(ctx, storeNode); err != nil {
			if existing, lookupErr := m.store.GetNodeByIdentity(ctx, identity.Hash); lookupErr == nil && existing != nil {
				return config.NodeConfig{}, &monitor.NodeDuplicateError{ExistingID: existing.ID, ExistingName: existing.Name}
			}
			return config.NodeConfig{}, fmt.Errorf("save to store: %w", err)
		}
	}

	m.cfg.Nodes = append(m.cfg.Nodes, normalized)
	return normalized, nil
}

// UpdateNode updates an existing node by name and persists to the Store.
func (m *Manager) UpdateNode(ctx context.Context, name string, node config.NodeConfig) (config.NodeConfig, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return config.NodeConfig{}, err
		}
	}

	name = strings.TrimSpace(name)
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cfg == nil {
		return config.NodeConfig{}, errConfigUnavailable
	}

	idx := m.nodeIndexLocked(name)
	if idx == -1 {
		return config.NodeConfig{}, monitor.ErrNodeNotFound
	}

	normalized, err := m.prepareNodeLocked(node, name)
	if err != nil {
		return config.NodeConfig{}, err
	}

	// Preserve the original source
	normalized.Source = m.cfg.Nodes[idx].Source

	// Persist to Store if available
	if m.store != nil {
		existing, err := m.store.GetNodeByName(ctx, name)
		if err != nil {
			return config.NodeConfig{}, fmt.Errorf("lookup in store: %w", err)
		}
		if existing != nil {
			identity, identityErr := nodecodec.ParseURI(normalized.URI)
			if identityErr != nil {
				return config.NodeConfig{}, fmt.Errorf("%w: %v", monitor.ErrInvalidNode, identityErr)
			}
			if duplicate, lookupErr := m.store.GetNodeByIdentity(ctx, identity.Hash); lookupErr != nil {
				return config.NodeConfig{}, fmt.Errorf("lookup node identity: %w", lookupErr)
			} else if duplicate != nil && duplicate.ID != existing.ID {
				return config.NodeConfig{}, &monitor.NodeDuplicateError{ExistingID: duplicate.ID, ExistingName: duplicate.Name}
			}
			existing.URI = normalized.URI
			existing.Name = normalized.Name
			existing.Port = normalized.Port
			existing.Username = normalized.Username
			existing.Password = normalized.Password
			if err := m.store.UpdateNode(ctx, existing); err != nil {
				return config.NodeConfig{}, fmt.Errorf("update in store: %w", err)
			}
		}
	}

	m.cfg.Nodes[idx] = normalized
	return normalized, nil
}

// SetNodeEnabled enables or disables a node by name.
// This only updates the store; a reload is needed for changes to take effect.
func (m *Manager) SetNodeEnabled(ctx context.Context, name string, enabled bool) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	name = strings.TrimSpace(name)
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cfg == nil {
		return errConfigUnavailable
	}

	// Update in Store
	if m.store != nil {
		existing, err := m.store.GetNodeByName(ctx, name)
		if err != nil {
			return fmt.Errorf("lookup in store: %w", err)
		}
		if existing == nil {
			return monitor.ErrNodeNotFound
		}
		existing.Enabled = enabled
		if err := m.store.UpdateNode(ctx, existing); err != nil {
			return fmt.Errorf("update in store: %w", err)
		}
	} else {
		// No store — just check the node exists in config
		idx := m.nodeIndexLocked(name)
		if idx == -1 {
			return monitor.ErrNodeNotFound
		}
	}

	// If disabling, remove from active config nodes
	if !enabled {
		idx := m.nodeIndexLocked(name)
		if idx != -1 {
			m.cfg.Nodes = append(m.cfg.Nodes[:idx], m.cfg.Nodes[idx+1:]...)
		}
	}

	return nil
}

// DeleteNode removes a node by name and deletes it from the Store.
func (m *Manager) DeleteNode(ctx context.Context, name string) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	name = strings.TrimSpace(name)
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cfg == nil {
		return errConfigUnavailable
	}

	idx := m.nodeIndexLocked(name)
	if idx == -1 {
		return monitor.ErrNodeNotFound
	}

	// Delete from Store if available
	if m.store != nil {
		existing, err := m.store.GetNodeByName(ctx, name)
		if err != nil {
			return fmt.Errorf("lookup in store: %w", err)
		}
		if existing != nil {
			if err := m.store.DeleteNode(ctx, existing.ID); err != nil {
				return fmt.Errorf("delete from store: %w", err)
			}
		}
	}

	m.cfg.Nodes = append(m.cfg.Nodes[:idx], m.cfg.Nodes[idx+1:]...)
	return nil
}

// TriggerReload reloads the sing-box instance by re-reading config from disk
// and loading nodes from the SQLite Store.
func (m *Manager) TriggerReload(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	m.mu.RLock()
	portMap := m.cfg.BuildPortMap() // Preserve existing port assignments
	oldBasePort := m.lastAppliedBasePort
	cfgPath := ""
	if m.cfg != nil {
		cfgPath = m.cfg.FilePath()
	}
	m.mu.RUnlock()

	// Re-read config from disk using LoadForReload (only gets inline nodes + settings)
	var newCfg *config.Config
	if cfgPath != "" {
		var err error
		newCfg, err = config.LoadForReload(cfgPath)
		if err != nil {
			m.logger.Warnf("failed to reload config from disk: %v, falling back to in-memory copy", err)
			m.mu.RLock()
			newCfg = m.copyConfigLocked()
			m.mu.RUnlock()
		} else {
			m.logger.Infof("reloaded config from disk: %s", cfgPath)
		}
	} else {
		m.mu.RLock()
		newCfg = m.copyConfigLocked()
		m.mu.RUnlock()
	}

	if newCfg == nil {
		return errConfigUnavailable
	}

	// Merge inline nodes (from config.yaml) with store nodes (subscription + manual).
	// Inline nodes take priority; store nodes are added if their URI is not already present.
	if m.store != nil {
		storeNodes, err := m.store.ListNodes(ctx, store.NodeFilter{})
		if err != nil {
			m.logger.Warnf("failed to list nodes from store during reload: %v", err)
		} else if len(storeNodes) > 0 {
			// Build set of URIs already present from inline nodes
			inlineURIs := make(map[string]int, len(newCfg.Nodes))
			for idx, n := range newCfg.Nodes {
				inlineURIs[n.NodeKey()] = idx
			}

			// Merge store nodes, skipping duplicates and disabled nodes
			for _, n := range storeNodes {
				if !n.Enabled {
					continue
				}
				identityKey := n.IdentityHash
				if identityKey == "" {
					identityKey = n.URI
				}
				if idx, exists := inlineURIs[identityKey]; exists {
					// Inline connection settings take priority, while SQLite owns the
					// stable identity and discovered region metadata used by groups.
					newCfg.Nodes[idx].ID = n.ID
					newCfg.Nodes[idx].Tags = append([]string(nil), n.Tags...)
					if newCfg.Nodes[idx].Region == "" {
						newCfg.Nodes[idx].Region = n.Region
					}
					if newCfg.Nodes[idx].Country == "" {
						newCfg.Nodes[idx].Country = n.Country
					}
					continue
				}
				newCfg.Nodes = append(newCfg.Nodes, config.NodeConfig{
					ID:           n.ID,
					Name:         n.Name,
					URI:          n.URI,
					Port:         n.Port,
					Username:     n.Username,
					Password:     n.Password,
					Source:       config.NodeSource(n.Source),
					Region:       n.Region,
					Country:      n.Country,
					Tags:         append([]string(nil), n.Tags...),
					IdentityHash: n.IdentityHash, CanonicalJSON: n.CanonicalJSON,
				})
			}
			m.logger.Infof("merged nodes for reload: %d inline + store nodes = %d total", len(inlineURIs), len(newCfg.Nodes))
		}
		groups, err := m.store.ListGroupPools(ctx)
		if err != nil {
			m.logger.Warnf("failed to list group pools during reload: %v", err)
		} else {
			newCfg.Groups = GroupConfigsFromStore(groups)
		}
		// Tag names join a group's tag IDs to the tag names a node carries, so the
		// snapshot has to be refreshed together with the nodes it applies to.
		if tagNames, err := m.loadTagNames(ctx); err != nil {
			m.logger.Warnf("failed to list tags during reload: %v", err)
		} else {
			newCfg.TagNames = tagNames
		}
	}

	// If no enabled nodes available after merging, enter idle state:
	// stop the running box gracefully so disabled nodes are no longer served.
	if len(newCfg.Nodes) == 0 {
		return m.enterIdle(newCfg)
	}

	// When an active multi-port base changes, discard old assignments so all
	// node listeners are rebuilt from the new base. Entry switch changes alone
	// preserve prior assignments for a later re-enable.
	basePortChanged := newCfg.MultiPort.BasePort != oldBasePort
	if newCfg.MultiPort.Enabled && basePortChanged {
		m.logger.Infof("multi-port base changed (%d→%d), reassigning all node ports",
			oldBasePort, newCfg.MultiPort.BasePort)
		portMap = nil // Discard old port map
		for idx := range newCfg.Nodes {
			newCfg.Nodes[idx].Port = 0 // Clear all ports for reassignment
		}
	}

	return m.ReloadWithPortMap(newCfg, portMap)
}

// GroupConfigsFromStore converts the persisted group definitions into a
// detached runtime snapshot. SQLite is the source of truth for group
// membership, so callers rebuilding the base topology should use this helper
// instead of carrying a potentially stale cached Groups slice forward.
func GroupConfigsFromStore(groups []store.GroupPool) []config.GroupPoolConfig {
	result := make([]config.GroupPoolConfig, 0, len(groups))
	for _, group := range groups {
		converted := config.GroupPoolConfig{ID: group.ID, Name: group.Name, BindAddress: group.BindAddress,
			BindPort: group.BindPort, Protocol: group.Protocol, Username: group.Username, Password: group.Password,
			DispatchMode: group.DispatchMode, Regions: append([]string(nil), group.Regions...),
			ExplicitNodeIDs:  append([]int64(nil), group.ExplicitNodeIDs...),
			ExcludedNodeIDs:  append([]int64(nil), group.ExcludedNodeIDs...),
			TagWhitelist:     append([]int64(nil), group.TagWhitelist...),
			TagBlacklist:     append([]int64(nil), group.TagBlacklist...),
			TagFilterMatch:   group.TagFilterMatch,
			FailureWindow:    time.Duration(group.FailureWindowSeconds) * time.Second,
			FailureThreshold: group.FailureThreshold, HealthCheckInterval: time.Duration(group.HealthCheckSeconds) * time.Second,
			CurrentActiveNodeID: group.CurrentActiveNodeID, Enabled: group.Enabled,
			SubscriptionEnabled: group.SubscriptionEnabled, SubscriptionToken: group.SubscriptionToken,
			SubscriptionMode: group.SubscriptionMode, ExternalHost: group.ExternalHost,
			CreatedAt: group.CreatedAt, UpdatedAt: group.UpdatedAt}
		for _, state := range group.NodeStates {
			converted.NodeStates = append(converted.NodeStates, config.GroupNodeStateConfig{NodeID: state.NodeID,
				FailureHistory: append([]int64(nil), state.FailureHistory...), Evicted: state.Evicted,
				LastError: state.LastError, EvictedAt: state.EvictedAt})
		}
		result = append(result, converted)
	}
	return result
}

// ReloadWithPortMap gracefully switches to a new configuration, preserving port assignments.
func (m *Manager) ReloadWithPortMap(newCfg *config.Config, portMap map[string]uint16) error {
	if newCfg == nil {
		return errors.New("new config is nil")
	}
	if len(newCfg.Nodes) == 0 {
		return m.enterIdle(newCfg)
	}

	// Always normalize config (apply defaults, assign ports, etc.).
	// If portMap is provided, existing nodes keep their ports; otherwise all ports are reassigned.
	if portMap == nil {
		portMap = make(map[string]uint16)
	}
	if err := newCfg.NormalizeWithPortMap(portMap); err != nil {
		return fmt.Errorf("normalize config with port map: %w", err)
	}

	return m.Reload(newCfg)
}

// enterIdle stops the running sing-box instance when there are 0 enabled nodes.
// The manager enters an idle state and can be resumed by TriggerReload when
// nodes are re-enabled.
func (m *Manager) enterIdle(newCfg *config.Config) error {
	m.mu.Lock()
	oldBox := m.currentBox
	wasIdle := m.idle
	m.currentBox = nil
	m.idle = true
	m.cfg = newCfg
	ctx := m.baseCtx
	// Update monitor server's config reference
	if m.monitorServer != nil {
		m.monitorServer.SetConfig(newCfg)
	}
	listeners := make([]ConfigUpdateListener, len(m.configListeners))
	copy(listeners, m.configListeners)
	m.mu.Unlock()

	if wasIdle {
		m.logger.Infof("already idle, updated config (still 0 enabled nodes)")
		return nil
	}

	// Stop the running instance
	if oldBox != nil {
		m.logger.Infof("stopping instance (all nodes disabled)...")
		if err := oldBox.Close(); err != nil {
			m.logger.Warnf("error closing instance during idle transition: %v", err)
		}
	}
	m.closeAllGroupRuntimes("stopped", "没有已启用节点")

	// Clean up monitor and shared state
	if m.monitorMgr != nil {
		m.monitorMgr.BeginReload()
		m.monitorMgr.SweepStaleNodes()
	}

	_ = ctx // baseCtx preserved for future resume

	for _, l := range listeners {
		l.OnConfigUpdate(newCfg)
	}

	m.logger.Infof("entered idle state (0 enabled nodes); re-enable nodes and reload to resume")
	return nil
}

func (m *Manager) closeAllGroupRuntimes(status, runtimeError string) error {
	m.groupSlotsMu.Lock()
	slots := make([]*groupRuntimeSlot, 0, len(m.groupSlots))
	for _, slot := range m.groupSlots {
		slots = append(slots, slot)
	}
	m.groupSlotsMu.Unlock()
	var result error
	for _, slot := range slots {
		if err := acquireGroupSlot(context.Background(), slot); err != nil {
			continue
		}
		slot.mu.Lock()
		if slot.box != nil {
			if err := slot.box.Close(); err != nil {
				result = errors.Join(result, err)
			}
			slot.box = nil
		}
		slot.state = monitor.GroupRuntimeStatus{Status: status, Error: runtimeError}
		slot.mu.Unlock()
		releaseGroupSlot(slot)
	}
	return result
}

// CurrentPortMap returns the current port mapping from the active configuration.
func (m *Manager) CurrentPortMap() map[string]uint16 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cfg == nil {
		return nil
	}
	return m.cfg.BuildPortMap()
}

// --- Helper functions ---

// portBindErrorRegex matches "listen tcp4 0.0.0.0:24282: bind: address already in use"
var portBindErrorRegex = regexp.MustCompile(`listen tcp[46]? [^:]+:(\d+): bind: address already in use`)

// extractPortFromBindError extracts the port number from a bind error message.
func extractPortFromBindError(err error) uint16 {
	if err == nil {
		return 0
	}
	matches := portBindErrorRegex.FindStringSubmatch(err.Error())
	if len(matches) < 2 {
		return 0
	}
	var port int
	fmt.Sscanf(matches[1], "%d", &port)
	if port > 0 && port <= 65535 {
		return uint16(port)
	}
	return 0
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

// reassignConflictingPort finds the node using the conflicting port and assigns a new port.
func reassignConflictingPort(cfg *config.Config, conflictPort uint16) bool {
	if cfg == nil || !cfg.MultiPort.Enabled {
		return false
	}
	// Build set of used ports
	usedPorts := make(map[uint16]bool)
	if cfg.Listener.Enabled && cfg.MultiPort.Enabled {
		usedPorts[cfg.Listener.Port] = true
	}
	for _, node := range cfg.Nodes {
		usedPorts[node.Port] = true
	}

	// Find and reassign the conflicting node
	for idx := range cfg.Nodes {
		if cfg.Nodes[idx].Port == conflictPort {
			// Find next available port
			newPort := int(conflictPort) + 1
			address := cfg.MultiPort.Address
			if address == "" {
				address = "0.0.0.0"
			}
			for newPort <= 65535 && (usedPorts[uint16(newPort)] || !isPortAvailable(address, uint16(newPort))) {
				newPort++
			}
			if newPort > 65535 {
				log.Printf("❌ No available port found for node %q", cfg.Nodes[idx].Name)
				return false
			}
			log.Printf("⚠️  Port %d in use, reassigning node %q to port %d", conflictPort, cfg.Nodes[idx].Name, newPort)
			cfg.Nodes[idx].Port = uint16(newPort)
			return true
		}
	}
	return false
}

func cloneNodes(nodes []config.NodeConfig) []config.NodeConfig {
	if len(nodes) == 0 {
		return []config.NodeConfig{} // Return empty slice, not nil, for proper JSON serialization
	}
	out := make([]config.NodeConfig, len(nodes))
	copy(out, nodes)
	return out
}

func (m *Manager) copyConfigLocked() *config.Config {
	if m.cfg == nil {
		return nil
	}
	return m.cfg.Clone()
}

func (m *Manager) nodeIndexLocked(name string) int {
	for idx, node := range m.cfg.Nodes {
		if node.Name == name {
			return idx
		}
	}
	return -1
}

func (m *Manager) portInUseLocked(port uint16, currentName string) bool {
	if port == 0 {
		return false
	}
	for _, node := range m.cfg.Nodes {
		if node.Name == currentName {
			continue
		}
		if node.Port == port {
			return true
		}
	}
	return false
}

func (m *Manager) nextAvailablePortLocked() uint16 {
	base := m.cfg.MultiPort.BasePort
	if base == 0 {
		base = 24000
	}
	used := make(map[uint16]struct{}, len(m.cfg.Nodes))
	for _, node := range m.cfg.Nodes {
		if node.Port > 0 {
			used[node.Port] = struct{}{}
		}
	}
	port := base
	for i := 0; i < 1<<16; i++ {
		if _, ok := used[port]; !ok && port != 0 {
			return port
		}
		port++
		if port == 0 {
			port = 1
		}
	}
	return base
}

func (m *Manager) prepareNodeLocked(node config.NodeConfig, currentName string) (config.NodeConfig, error) {
	node.Name = strings.TrimSpace(node.Name)
	node.URI = strings.TrimSpace(node.URI)

	if node.URI == "" {
		return config.NodeConfig{}, fmt.Errorf("%w: URI 不能为空", monitor.ErrInvalidNode)
	}

	// Extract name from URI fragment (#name) if not provided
	if node.Name == "" {
		if currentName != "" {
			node.Name = currentName
		} else if idx := strings.LastIndex(node.URI, "#"); idx != -1 && idx < len(node.URI)-1 {
			// Extract and URL-decode the fragment
			fragment := node.URI[idx+1:]
			if decoded, err := url.QueryUnescape(fragment); err == nil && decoded != "" {
				node.Name = decoded
			}
		}
		// Fallback to auto-generated name
		if node.Name == "" {
			node.Name = fmt.Sprintf("node-%d", len(m.cfg.Nodes)+1)
		}
	}

	// Check for name conflict (excluding current node when updating)
	if idx := m.nodeIndexLocked(node.Name); idx != -1 {
		if currentName == "" || m.cfg.Nodes[idx].Name != currentName {
			return config.NodeConfig{}, fmt.Errorf("%w: 节点 %s 已存在", monitor.ErrNodeConflict, node.Name)
		}
	}

	// Per-node ports and default credentials only apply while multi-port is enabled.
	if m.cfg.MultiPort.Enabled {
		if node.Port == 0 {
			node.Port = m.nextAvailablePortLocked()
		} else if m.portInUseLocked(node.Port, currentName) {
			return config.NodeConfig{}, fmt.Errorf("%w: 端口 %d 已被占用", monitor.ErrNodeConflict, node.Port)
		}
		if node.Username == "" {
			node.Username = m.cfg.MultiPort.Username
			node.Password = m.cfg.MultiPort.Password
		}
	}

	return node, nil
}
