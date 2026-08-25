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
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/nodecodec"
	"easy_proxies/internal/outbound/pool"
	"easy_proxies/internal/store"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
)

// Ensure Manager implements monitor.NodeManager.
var _ monitor.NodeManager = (*Manager)(nil)
var _ monitor.GroupRuntimeManager = (*Manager)(nil)

const (
	defaultDrainTimeout       = 10 * time.Second
	defaultHealthCheckTimeout = 30 * time.Second
	healthCheckPollInterval   = 500 * time.Millisecond
	// periodicHealthInterval is configured via cfg.Management.HealthCheckInterval
	periodicHealthTimeout = 10 * time.Second
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
	mu sync.RWMutex

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

	// lastAppliedMode and lastAppliedBasePort track the mode/BasePort from the
	// last successful Start/Reload. Used by TriggerReload to detect changes,
	// since m.cfg may have been mutated by updateAllSettings before reload.
	lastAppliedMode     string
	lastAppliedBasePort uint16

	groupSlotsMu sync.Mutex
	groupSlots   map[int64]*groupRuntimeSlot
}

type groupRuntimeSlot struct {
	gate  chan struct{}
	box   *box.Box
	mu    sync.RWMutex
	state monitor.GroupRuntimeStatus
}

// New creates a BoxManager with the given config.
func New(cfg *config.Config, monitorCfg monitor.Config, opts ...Option) *Manager {
	m := &Manager{
		cfg:        cfg,
		monitorCfg: monitorCfg,
		groupSlots: make(map[int64]*groupRuntimeSlot),
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

	m.mu.Lock()
	m.currentBox = instance
	m.lastAppliedMode = cfg.Mode
	m.lastAppliedBasePort = cfg.MultiPort.BasePort
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
		m.monitorMgr.StartPeriodicHealthCheck(interval, periodicHealthTimeout)
		m.healthCheckStarted = true
	}
	m.mu.Unlock()

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
	if newCfg == nil {
		return errors.New("new config is nil")
	}

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
		m.logger.Infof("reload skipped: runtime configuration unchanged")
		return nil
	}
	drainTimeout := m.drainTimeout
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
		instance, err = m.createBaseBox(ctx, newCfg)
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

	// Sweep stale monitor entries (disabled/removed nodes) now that the new box
	// has successfully registered all active nodes with the current generation.
	if m.monitorMgr != nil {
		m.monitorMgr.SweepStaleNodes()
	}

	m.applyConfigSettings(newCfg)

	m.mu.Lock()
	m.currentBox = instance
	m.cfg = newCfg
	m.idle = false // Clear idle state on successful reload
	m.lastAppliedMode = newCfg.Mode
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
		m.monitorMgr.SetHealthCheckInterval(effectiveHealthCheckInterval(newCfg))
		go m.monitorMgr.RequestProbeAllOnce(periodicHealthTimeout)
	}
	if groupReloadErr != nil {
		m.logger.Warnf("base reload completed with group runtime errors: %v", groupReloadErr)
	} else {
		m.logger.Infof("reload completed successfully with %d nodes", len(newCfg.Nodes))
	}
	return groupReloadErr
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
		if err := m.applyGroupRuntime(ctx, oldGroups[id], newGroups[id], true); err != nil {
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

func groupMemberNodes(cfg *config.Config, groupCfg config.GroupPoolConfig) []config.NodeConfig {
	if cfg == nil || !groupCfg.Enabled {
		return nil
	}
	regions := make(map[string]struct{}, len(groupCfg.Regions))
	for _, region := range groupCfg.Regions {
		regions[strings.ToLower(strings.TrimSpace(region))] = struct{}{}
	}
	explicit := make(map[int64]struct{}, len(groupCfg.ExplicitNodeIDs))
	for _, nodeID := range groupCfg.ExplicitNodeIDs {
		explicit[nodeID] = struct{}{}
	}
	excluded := make(map[int64]struct{}, len(groupCfg.ExcludedNodeIDs))
	for _, nodeID := range groupCfg.ExcludedNodeIDs {
		excluded[nodeID] = struct{}{}
	}
	result := make([]config.NodeConfig, 0)
	for _, node := range cfg.Nodes {
		if _, skip := excluded[node.ID]; skip {
			continue
		}
		_, manuallyIncluded := explicit[node.ID]
		_, regionIncluded := regions[strings.ToLower(strings.TrimSpace(node.Region))]
		if manuallyIncluded || regionIncluded {
			result = append(result, node)
		}
	}
	return result
}

func storeGroupFromConfig(groupCfg config.GroupPoolConfig) *store.GroupPool {
	groupPool := &store.GroupPool{ID: groupCfg.ID, Name: groupCfg.Name, BindAddress: groupCfg.BindAddress,
		BindPort: groupCfg.BindPort, Protocol: groupCfg.Protocol, Username: groupCfg.Username, Password: groupCfg.Password,
		DispatchMode: groupCfg.DispatchMode, Regions: append([]string(nil), groupCfg.Regions...),
		ExplicitNodeIDs: append([]int64(nil), groupCfg.ExplicitNodeIDs...), ExcludedNodeIDs: append([]int64(nil), groupCfg.ExcludedNodeIDs...),
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
	slot := &groupRuntimeSlot{gate: make(chan struct{}, 1), state: monitor.GroupRuntimeStatus{Status: "stopped"}}
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
	slot.mu.Unlock()
	return nil
}

func (m *Manager) ApplyGroupRuntime(ctx context.Context, before, after *store.GroupPool) error {
	return m.applyGroupRuntime(ctx, before, after, false)
}

func (m *Manager) applyGroupRuntime(ctx context.Context, before, after *store.GroupPool, force bool) error {
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

	if !force && groupRuntimeEqual(before, after) {
		m.replaceCachedGroup(after)
		return nil
	}

	slot.mu.Lock()
	oldBox := slot.box
	slot.state = monitor.GroupRuntimeStatus{Status: "reconfiguring"}
	slot.mu.Unlock()

	if after == nil || !after.Enabled {
		if oldBox != nil {
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
	m.mu.RLock()
	baseCtx := m.baseCtx
	m.mu.RUnlock()
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	newBox, err := m.createGroupBox(baseCtx, candidateCfg, groupID)
	if err != nil {
		if force {
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
		if oldBox != nil {
			m.setGroupRuntimeStatus(groupID, "ready", "")
		} else {
			m.setGroupRuntimeStatus(groupID, "error", err.Error())
		}
		return err
	}
	if oldBox != nil {
		_ = oldBox.Close()
	}
	if err = newBox.Start(); err != nil {
		_ = newBox.Close()
		if force {
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
	slot.mu.Unlock()
	m.replaceCachedGroup(after)
	return nil
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
	if status.Status != "ready" {
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
		groups = append(groups, groupConfigsFromStore([]store.GroupPool{*groupPool})[0])
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
		groups = append(groups, groupConfigsFromStore([]store.GroupPool{*groupPool})[0])
	}
	m.cfg.Groups = groups
}

func groupRuntimeEqual(before, after *store.GroupPool) bool {
	if before == nil || after == nil {
		return before == after
	}
	return before.ID == after.ID && before.BindAddress == after.BindAddress && before.BindPort == after.BindPort &&
		before.Protocol == after.Protocol && before.Username == after.Username && before.Password == after.Password &&
		before.DispatchMode == after.DispatchMode && reflect.DeepEqual(before.Regions, after.Regions) &&
		reflect.DeepEqual(before.ExplicitNodeIDs, after.ExplicitNodeIDs) && reflect.DeepEqual(before.ExcludedNodeIDs, after.ExcludedNodeIDs) &&
		before.FailureWindowSeconds == after.FailureWindowSeconds && before.FailureThreshold == after.FailureThreshold &&
		before.HealthCheckSeconds == after.HealthCheckSeconds && before.Enabled == after.Enabled
}

// createBaseBox builds the application-wide instance without group listeners.
func (m *Manager) createBaseBox(ctx context.Context, cfg *config.Config) (*box.Box, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	if m.monitorMgr == nil {
		return nil, errors.New("monitor manager not initialized")
	}

	opts, err := builder.BuildBase(cfg)
	if err != nil {
		return nil, fmt.Errorf("build sing-box options: %w", err)
	}
	return m.createBoxFromOptions(ctx, opts)
}

func (m *Manager) createGroupBox(ctx context.Context, cfg *config.Config, groupID int64) (*box.Box, error) {
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	opts, err := builder.BuildGroup(cfg, groupID)
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
			Name:            n.Name,
			URI:             n.URI,
			Port:            port,
			Username:        n.Username,
			Password:        n.Password,
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
	return monitor.ManagedNodeConfig{Name: node.Name, URI: node.URI, Port: node.Port,
		Username: node.Username, Password: node.Password, Source: node.Source,
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
	oldMode := m.lastAppliedMode
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
					IdentityHash: n.IdentityHash, CanonicalJSON: n.CanonicalJSON,
				})
			}
			m.logger.Infof("merged nodes for reload: %d inline + store nodes = %d total", len(inlineURIs), len(newCfg.Nodes))
		}
		groups, err := m.store.ListGroupPools(ctx)
		if err != nil {
			m.logger.Warnf("failed to list group pools during reload: %v", err)
		} else {
			newCfg.Groups = groupConfigsFromStore(groups)
		}
	}

	// If no enabled nodes available after merging, enter idle state:
	// stop the running box gracefully so disabled nodes are no longer served.
	if len(newCfg.Nodes) == 0 {
		return m.enterIdle(newCfg)
	}

	// Detect mode or base port changes — if either changed, discard old port
	// assignments so all nodes get fresh ports from the new BasePort.
	modeChanged := newCfg.Mode != oldMode
	basePortChanged := newCfg.MultiPort.BasePort != oldBasePort
	if modeChanged || basePortChanged {
		m.logger.Infof("mode/base-port changed (mode: %s→%s, base: %d→%d), reassigning all ports",
			oldMode, newCfg.Mode, oldBasePort, newCfg.MultiPort.BasePort)
		portMap = nil // Discard old port map
		for idx := range newCfg.Nodes {
			newCfg.Nodes[idx].Port = 0 // Clear all ports for reassignment
		}
	}

	return m.ReloadWithPortMap(newCfg, portMap)
}

func groupConfigsFromStore(groups []store.GroupPool) []config.GroupPoolConfig {
	result := make([]config.GroupPoolConfig, 0, len(groups))
	for _, group := range groups {
		converted := config.GroupPoolConfig{ID: group.ID, Name: group.Name, BindAddress: group.BindAddress,
			BindPort: group.BindPort, Protocol: group.Protocol, Username: group.Username, Password: group.Password,
			DispatchMode: group.DispatchMode, Regions: group.Regions, ExplicitNodeIDs: group.ExplicitNodeIDs,
			FailureWindow:    time.Duration(group.FailureWindowSeconds) * time.Second,
			FailureThreshold: group.FailureThreshold, HealthCheckInterval: time.Duration(group.HealthCheckSeconds) * time.Second,
			CurrentActiveNodeID: group.CurrentActiveNodeID, Enabled: group.Enabled,
			SubscriptionEnabled: group.SubscriptionEnabled, SubscriptionToken: group.SubscriptionToken,
			SubscriptionMode: group.SubscriptionMode, ExternalHost: group.ExternalHost,
			CreatedAt: group.CreatedAt, UpdatedAt: group.UpdatedAt}
		for _, state := range group.NodeStates {
			converted.NodeStates = append(converted.NodeStates, config.GroupNodeStateConfig{NodeID: state.NodeID,
				FailureHistory: state.FailureHistory, Evicted: state.Evicted, LastError: state.LastError, EvictedAt: state.EvictedAt})
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
	// Build set of used ports
	usedPorts := make(map[uint16]bool)
	if cfg.Mode == "hybrid" {
		usedPorts[cfg.Listener.Port] = true
	}
	for _, node := range cfg.Nodes {
		usedPorts[node.Port] = true
	}

	// Find and reassign the conflicting node
	for idx := range cfg.Nodes {
		if cfg.Nodes[idx].Port == conflictPort {
			// Find next available port
			newPort := conflictPort + 1
			address := cfg.MultiPort.Address
			if address == "" {
				address = "0.0.0.0"
			}
			for usedPorts[newPort] || !isPortAvailable(address, newPort) {
				newPort++
				if newPort > 65535 {
					log.Printf("❌ No available port found for node %q", cfg.Nodes[idx].Name)
					return false
				}
			}
			log.Printf("⚠️  Port %d in use, reassigning node %q to port %d", conflictPort, cfg.Nodes[idx].Name, newPort)
			cfg.Nodes[idx].Port = newPort
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

	// Handle multi-port mode specifics
	if m.cfg.Mode == "multi-port" {
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
