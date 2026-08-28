package pool

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

const (
	// Type is the outbound type name exposed to sing-box.
	Type = "pool"
	// Tag is the default outbound tag used by builder.
	Tag = "proxy-pool"

	modeSequential    = "sequential"
	modeRandom        = "random"
	modeBalance       = "balance"
	modeFixed         = "fixed"
	modeLowestLatency = "lowest_latency"
)

// Options controls pool outbound behaviour.
type Options struct {
	Mode                string
	Members             []string
	FailureThreshold    int
	BlacklistDuration   time.Duration
	Metadata            map[string]MemberMeta
	GroupID             int64
	FailureWindow       time.Duration
	HealthCheckInterval time.Duration
	PreferredMember     string
	InitialGroupState   map[string]group.GroupInitialState
	SelectorTag         string
	MonitorObserverOnly bool
}

// MemberMeta carries optional descriptive information for monitoring UI.
type MemberMeta struct {
	NodeID        int64
	Name          string
	URI           string
	Mode          string
	ListenAddress string
	Port          uint16
	Region        string // GeoIP region code: "jp", "kr", "us", "hk", "tw", "other"
	Country       string // Full country name from GeoIP
}

// Register wires the pool outbound into the registry.
func Register(registry *outbound.Registry) {
	outbound.Register[Options](registry, Type, newPool)
}

// MemberRef is a stable reference to one concrete runtime outbound. Published
// member references are immutable; their pointed-to shared runtime state may
// continue to change independently.
type MemberRef struct {
	outbound  adapter.Outbound
	tag       string
	entry     *monitor.EntryHandle
	shared    *sharedMemberState
	runtime   *memberRuntime
	meta      MemberMeta
	latencyMs atomic.Int64
}

// memberRuntime owns lifecycle counters for one concrete outbound. Multiple
// immutable MemberRef values may describe the same outbound after metadata or
// group-policy updates; keeping the counters here makes remove/re-add races
// visible to every ref and every retired ticket.
type memberRuntime struct {
	outbound     adapter.Outbound
	shared       *sharedMemberState
	snapshotRefs atomic.Int64
	operations   atomic.Int64
	retired      atomic.Bool
	releaseOnce  sync.Once
}

func newMemberRuntime(detour adapter.Outbound, shared *sharedMemberState) *memberRuntime {
	if shared != nil {
		shared.owners.Add(1)
	}
	return &memberRuntime{outbound: detour, shared: shared}
}

func (r *memberRuntime) releaseOwner(tag string) {
	if r == nil || r.shared == nil {
		return
	}
	r.releaseOnce.Do(func() {
		if r.shared.owners.Add(-1) == 0 {
			sharedStateStore.CompareAndDelete(tag, r.shared)
		}
	})
}

// Keep the existing internal name while the rest of the pool implementation
// is migrated to snapshots.
type memberState = MemberRef

// PoolSnapshot is an immutable, versioned view of the pool topology. Writers
// must publish a newly allocated Members slice instead of mutating one that is
// already visible to readers.
type PoolSnapshot struct {
	Members []*MemberRef
	Version uint64
	mode    string

	tcpMembers  []*MemberRef
	udpMembers  []*MemberRef
	allMembers  []*MemberRef
	memberIndex map[*MemberRef]int
	tcpIndex    map[*MemberRef]int
	udpIndex    map[*MemberRef]int
	accepting   atomic.Bool
	readers     atomic.Int64
	releaseOnce sync.Once
}

// RuntimeMemberSpec describes one concrete outbound in the live pool topology.
type RuntimeMemberSpec struct {
	Tag  string
	Meta MemberMeta
}

// RetiredMember owns the saved reference needed after publication. sing-box's
// Remove may return an error after deleting its lookup entry, so cleanup must
// not try to resolve the outbound again.
type RetiredMember struct {
	ref  *MemberRef
	pool *poolOutbound
}

func (m RetiredMember) Tag() string { return m.ref.tag }

func (m RetiredMember) Drained() bool {
	runtimeState := ensureMemberRuntime(m.ref)
	return runtimeState.snapshotRefs.Load() == 0 && runtimeState.operations.Load() == 0 &&
		(m.ref.shared == nil || m.ref.shared.activeCount() == 0)
}

func (m RetiredMember) Counts() (snapshotRefs, operations int64, active int32) {
	runtimeState := ensureMemberRuntime(m.ref)
	return runtimeState.snapshotRefs.Load(), runtimeState.operations.Load(), func() int32 {
		if m.ref.shared == nil {
			return 0
		}
		return m.ref.shared.activeCount()
	}()
}

func (m RetiredMember) Close() error { return common.Close(m.ref.outbound) }

// ReleaseState drops the per-runtime-tag counters after every reader,
// operation and connection has drained. Stable-node history lives in monitor's
// migrated entry and is not lost with this version-specific state.
func (m RetiredMember) ReleaseState() {
	runtimeState := ensureMemberRuntime(m.ref)
	runtimeState.releaseOwner(m.ref.tag)
	if m.pool != nil {
		m.pool.mu.Lock()
		if m.pool.runtimeByTag[m.ref.tag] == runtimeState {
			delete(m.pool.runtimeByTag, m.ref.tag)
		}
		m.pool.mu.Unlock()
	}
}

// RuntimePool is the control-plane surface used for in-place base pool reloads.
type RuntimePool interface {
	CreateMember(string, string, any) error
	ReconcileMembers([]RuntimeMemberSpec) (uint64, []RetiredMember, error)
	PrepareUpdate(RuntimeUpdate) (PreparedUpdate, error)
}

// RuntimeUpdate is a complete immutable group pool configuration. Zero policy
// values are normalized to the same defaults used at cold start.
type RuntimeUpdate struct {
	Members             []RuntimeMemberSpec
	Mode                string
	FailureWindow       time.Duration
	FailureThreshold    int
	HealthCheckInterval time.Duration
	PreferredNodeID     int64
	InitialGroupState   map[string]group.GroupInitialState
}

// PreparedUpdate contains only validated, detached input. Commit is the sole
// mutation point; Rollback is idempotent and deliberately leaves the live pool
// untouched.
type PreparedUpdate interface {
	Commit() (uint64, []RetiredMember, error)
	Rollback()
}

type poolOutbound struct {
	outbound.Adapter
	ctx                   context.Context
	logger                log.ContextLogger
	manager               adapter.OutboundManager
	router                adapter.Router
	options               Options
	mode                  string
	current               atomic.Pointer[PoolSnapshot]
	mu                    sync.Mutex
	topology              []*MemberRef
	topologyByTag         map[string]*MemberRef
	runtimeByTag          map[string]*memberRuntime
	currentMember         atomic.Pointer[MemberRef]
	rrCounter             atomic.Uint32
	randomState           atomic.Uint64
	failureThreshold      atomic.Int64
	failureWindowNanos    atomic.Int64
	monitor               *monitor.Manager
	selector              selectorOutbound
	selectorMu            sync.Mutex
	waitForInitialLatency atomic.Bool
	rebuildTimer          *time.Timer
	expiryTimer           *time.Timer
	closed                atomic.Bool
	unsubscribeHealth     func()
	unsubscribeActivation func()
	unsubscribeGroupState func()
	unregisterSchedule    func()
	unregisterRuntime     func()
	closeOnce             sync.Once
}

type selectorOutbound interface {
	adapter.Outbound
	SelectOutbound(tag string) bool
	Now() string
}

func newPool(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options Options) (adapter.Outbound, error) {
	if len(options.Members) == 0 {
		return nil, E.New("pool requires at least one member")
	}
	manager := service.FromContext[adapter.OutboundManager](ctx)
	if manager == nil {
		return nil, E.New("missing outbound manager in context")
	}
	monitorMgr := monitor.FromContext(ctx)
	normalized := normalizeOptions(options)
	// Member outbounds are reconciled dynamically. Declaring the cold-start
	// member tags here would make sing-box keep those initial tags forever in
	// its static dependByTag snapshot, while members added through Create remain
	// unprotected. Pool snapshots own member lifetimes uniformly instead.
	dependencies := poolDependencies(normalized)
	p := &poolOutbound{
		Adapter:       outbound.NewAdapter(Type, tag, []string{N.NetworkTCP, N.NetworkUDP}, dependencies),
		ctx:           ctx,
		logger:        logger,
		manager:       manager,
		router:        router,
		options:       normalized,
		mode:          normalized.Mode,
		monitor:       monitorMgr,
		topologyByTag: make(map[string]*MemberRef, len(normalized.Members)),
		runtimeByTag:  make(map[string]*memberRuntime, len(normalized.Members)),
	}
	p.randomState.Store(uint64(time.Now().UnixNano()) | 1)
	p.failureThreshold.Store(int64(normalized.FailureThreshold))
	p.failureWindowNanos.Store(int64(normalized.FailureWindow))
	if normalized.Mode == modeLowestLatency && normalized.PreferredMember == "" {
		p.waitForInitialLatency.Store(true)
	}
	p.unregisterRuntime = group.Register(normalized.GroupID, normalized.FailureWindow, normalized.FailureThreshold,
		normalized.PreferredMember, normalized.InitialGroupState)

	// Register nodes immediately if monitor is available
	if monitorMgr != nil && !normalized.MonitorObserverOnly {
		logger.Info("registering ", len(normalized.Members), " nodes to monitor")
		for _, memberTag := range normalized.Members {
			// Acquire shared state for this tag (creates if not exists)
			state := acquireSharedState(memberTag)

			meta := normalized.Metadata[memberTag]
			info := monitor.NodeInfo{
				NodeID:        meta.NodeID,
				Tag:           memberTag,
				Name:          meta.Name,
				URI:           meta.URI,
				Mode:          meta.Mode,
				ListenAddress: meta.ListenAddress,
				Port:          meta.Port,
				Region:        meta.Region,
				Country:       meta.Country,
			}
			entry := monitorMgr.MigrateRuntimeTag(meta.NodeID, info)
			if entry != nil {
				// Attach entry to shared state so all pool instances share it
				state.attachEntry(entry)
				logger.Info("registered node: ", memberTag)
				// Set probe, release, and dialer functions immediately
				entry.SetRelease(p.makeReleaseByTagFunc(memberTag))
				if probeFn := p.makeProbeByTagFunc(memberTag); probeFn != nil {
					entry.SetProbe(probeFn)
				}
				if dialerFn := p.makeDialerByTagFunc(memberTag); dialerFn != nil {
					entry.SetDialer(dialerFn)
				}
			} else {
				logger.Warn("failed to register node: ", memberTag)
			}
		}
	} else {
		logger.Warn("monitor manager is nil, skipping node registration")
	}

	return p, nil
}

// CreateMember constructs and starts a dynamic outbound inside the same
// sing-box context as the pool, then rejects unexpected dynamic dependencies.
func (p *poolOutbound) CreateMember(tag, outboundType string, options any) error {
	memberCtx := adapter.WithContext(p.ctx, &adapter.InboundContext{Outbound: tag})
	if err := p.manager.Create(memberCtx, p.router, p.logger, tag, outboundType, options); err != nil {
		return err
	}
	created, ok := p.manager.Outbound(tag)
	if !ok {
		return E.New("created pool member is not registered: ", tag)
	}
	if dependencies := created.Dependencies(); len(dependencies) != 0 {
		removeErr := p.manager.Remove(tag)
		if removeErr != nil {
			_ = common.Close(created)
		}
		return E.New("dynamic pool member ", tag, " depends on ", strings.Join(dependencies, ", "), "; cleanup: ", removeErr)
	}
	return nil
}

func poolDependencies(options Options) []string {
	if options.SelectorTag == "" {
		return nil
	}
	return []string{options.SelectorTag}
}

func normalizeOptions(options Options) Options {
	if options.FailureThreshold <= 0 {
		options.FailureThreshold = 3
	}
	if options.BlacklistDuration <= 0 {
		options.BlacklistDuration = 24 * time.Hour
	}
	if options.Metadata == nil {
		options.Metadata = make(map[string]MemberMeta)
	}
	switch strings.ToLower(options.Mode) {
	case modeRandom:
		options.Mode = modeRandom
	case modeBalance:
		options.Mode = modeBalance
	case modeFixed:
		options.Mode = modeFixed
	case modeLowestLatency:
		options.Mode = modeLowestLatency
	default:
		options.Mode = modeSequential
	}
	if options.GroupID != 0 {
		// The runtime registry treats every InitialGroupState entry as a member.
		// Intersect it with Members and fill missing entries so stale persisted
		// state can never recreate an excluded member.
		initialState := make(map[string]group.GroupInitialState, len(options.Members))
		for _, tag := range options.Members {
			if state, ok := options.InitialGroupState[tag]; ok {
				state.FailureHistory = append([]int64(nil), state.FailureHistory...)
				initialState[tag] = state
				continue
			}
			initialState[tag] = group.GroupInitialState{NodeID: options.Metadata[tag].NodeID}
		}
		options.InitialGroupState = initialState
	}
	return options
}

func (p *poolOutbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	p.mu.Lock()
	err := p.initializeMembersLocked()
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if p.monitor != nil {
		p.unsubscribeHealth = p.monitor.SubscribeHealthResults(p.handleHealthResult)
	}
	if p.monitor != nil && p.options.GroupID != 0 {
		nodeIDs := make([]int64, 0, len(p.options.Members))
		for _, tag := range p.options.Members {
			nodeIDs = append(nodeIDs, p.options.Metadata[tag].NodeID)
		}
		p.unregisterSchedule = p.monitor.RegisterGroupHealthScheduleByNodeID(p.options.GroupID, nodeIDs, p.options.HealthCheckInterval)
		p.reconcileCurrent()
		p.unsubscribeActivation = group.RegisterActivationHandler(p.options.GroupID, p.activateNodeID)
		p.unsubscribeGroupState = group.SubscribeStateChanges(p.handleGroupState)
	}
	return nil
}

func (p *poolOutbound) loadSnapshot() (*PoolSnapshot, error) {
	if snapshot := p.current.Load(); snapshot != nil {
		return snapshot, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.initializeMembersLocked(); err != nil {
		return nil, err
	}
	return p.current.Load(), nil
}

// acquireSnapshot pins the loaded snapshot until release is called. The second
// accepting check closes the Load/increment race with a concurrent publisher.
func (p *poolOutbound) acquireSnapshot() (*PoolSnapshot, func(), error) {
	for {
		snapshot, err := p.loadSnapshot()
		if err != nil {
			return nil, nil, err
		}
		snapshot.readers.Add(1)
		if snapshot.accepting.Load() {
			return snapshot, func() { p.releaseSnapshotReader(snapshot) }, nil
		}
		p.releaseSnapshotReader(snapshot)
		runtime.Gosched()
	}
}

func (p *poolOutbound) releaseSnapshotReader(snapshot *PoolSnapshot) {
	if snapshot.readers.Add(-1) == 0 && !snapshot.accepting.Load() {
		p.releaseSnapshotMembers(snapshot)
	}
}

func (p *poolOutbound) releaseSnapshotMembers(snapshot *PoolSnapshot) {
	snapshot.releaseOnce.Do(func() {
		for _, member := range snapshot.allMembers {
			member.runtime.snapshotRefs.Add(-1)
		}
	})
}

// publishSnapshotLocked builds all expensive eligibility and ordering state on
// the writer side, then replaces current with one atomic store.
func (p *poolOutbound) publishSnapshotLocked(members []*memberState) *PoolSnapshot {
	version := uint64(1)
	previous := p.current.Load()
	if previous != nil {
		version = previous.Version + 1
	}
	snapshot := &PoolSnapshot{
		Version:     version,
		mode:        p.mode,
		allMembers:  append([]*MemberRef(nil), members...),
		memberIndex: make(map[*MemberRef]int, len(members)),
		tcpIndex:    make(map[*MemberRef]int, len(members)),
		udpIndex:    make(map[*MemberRef]int, len(members)),
	}
	for _, member := range snapshot.allMembers {
		ensureMemberRuntime(member)
		member.runtime.snapshotRefs.Add(1)
		if !p.memberAvailableForSnapshot(member, time.Now()) {
			continue
		}
		snapshot.memberIndex[member] = len(snapshot.Members)
		snapshot.Members = append(snapshot.Members, member)
		// Tests and control-plane preparation may use metadata-only references.
		// A published runtime member always has an outbound; treating a nil one as
		// dual-stack here keeps snapshot construction side-effect free and lets
		// validation happen at the reconciliation boundary.
		networks := []string{N.NetworkTCP, N.NetworkUDP}
		if member.outbound != nil {
			networks = member.outbound.Network()
		}
		if common.Contains(networks, N.NetworkTCP) {
			snapshot.tcpMembers = append(snapshot.tcpMembers, member)
		}
		if common.Contains(networks, N.NetworkUDP) {
			snapshot.udpMembers = append(snapshot.udpMembers, member)
		}
	}
	if p.mode == modeLowestLatency {
		sort.SliceStable(snapshot.tcpMembers, func(i, j int) bool { return p.latencyMemberLess(snapshot.tcpMembers[i], snapshot.tcpMembers[j]) })
		sort.SliceStable(snapshot.udpMembers, func(i, j int) bool { return p.latencyMemberLess(snapshot.udpMembers[i], snapshot.udpMembers[j]) })
	}
	for index, member := range snapshot.tcpMembers {
		snapshot.tcpIndex[member] = index
	}
	for index, member := range snapshot.udpMembers {
		snapshot.udpIndex[member] = index
	}
	snapshot.accepting.Store(true)
	if previous != nil {
		previous.accepting.Store(false)
	}
	p.current.Store(snapshot)
	if previous != nil && previous.readers.Load() == 0 {
		p.releaseSnapshotMembers(previous)
	}
	if p.logger != nil {
		p.logger.Info("published pool snapshot v", snapshot.Version, " with ", len(snapshot.Members), "/", len(snapshot.allMembers), " available members")
	}
	return snapshot
}

func (p *poolOutbound) memberAvailableForSnapshot(member *MemberRef, now time.Time) bool {
	if p.options.GroupID != 0 && !group.MemberAvailable(p.options.GroupID, member.tag) {
		return false
	}
	probeConfigured := false
	if p.monitor != nil {
		_, probeConfigured = p.monitor.TargetForProbe()
		monitorSnapshot := p.monitor.SnapshotForTag(member.tag)
		if p.options.MonitorObserverOnly && p.memberMeta(member).NodeID != 0 {
			monitorSnapshot = p.monitor.SnapshotForNodeID(p.memberMeta(member).NodeID)
		}
		if monitorSnapshot != nil {
			member.latencyMs.Store(monitorSnapshot.LastLatencyMs)
		}
		if (p.options.GroupID != 0 || probeConfigured) &&
			(monitorSnapshot == nil || !monitorSnapshot.InitialCheckDone || !monitorSnapshot.Available || monitorSnapshot.Blacklisted) {
			return false
		}
	}
	return member.shared == nil || !member.shared.isBlacklisted(now)
}

func (p *poolOutbound) Close() error {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		p.mu.Lock()
		if p.rebuildTimer != nil {
			p.rebuildTimer.Stop()
		}
		if p.expiryTimer != nil {
			p.expiryTimer.Stop()
		}
		if snapshot := p.current.Load(); snapshot != nil {
			snapshot.accepting.Store(false)
			if snapshot.readers.Load() == 0 {
				p.releaseSnapshotMembers(snapshot)
			}
		}
		p.mu.Unlock()
		if p.unsubscribeActivation != nil {
			p.unsubscribeActivation()
		}
		if p.unsubscribeGroupState != nil {
			p.unsubscribeGroupState()
		}
		if p.unregisterSchedule != nil {
			p.unregisterSchedule()
		}
		if p.unsubscribeHealth != nil {
			p.unsubscribeHealth()
		}
		if p.unregisterRuntime != nil {
			p.unregisterRuntime()
		}
		p.mu.Lock()
		for tag, runtimeState := range p.runtimeByTag {
			runtimeState.releaseOwner(tag)
		}
		p.runtimeByTag = nil
		p.mu.Unlock()
	})
	return nil
}

func (p *poolOutbound) activateNodeID(nodeID int64) error {
	snapshot, err := p.loadSnapshot()
	if err != nil {
		return err
	}
	var selected *memberState
	for _, candidate := range snapshot.Members {
		if p.memberMeta(candidate).NodeID == nodeID {
			selected = candidate
			break
		}
	}
	if selected == nil {
		return errors.New("node is not a healthy running group member")
	}
	p.selectorMu.Lock()
	defer p.selectorMu.Unlock()
	if p.selector != nil && !p.selector.SelectOutbound(selected.tag) {
		return errors.New("selector rejected group member")
	}
	p.waitForInitialLatency.Store(false)
	p.currentMember.Store(selected)
	group.SetCurrentTag(p.options.GroupID, selected.tag)
	return nil
}

// initializeMembersLocked must be called with p.mu held
func (p *poolOutbound) initializeMembersLocked() error {
	if p.current.Load() != nil {
		return nil // Already initialized
	}

	members := make([]*memberState, 0, len(p.options.Members))
	for _, tag := range p.options.Members {
		detour, loaded := p.manager.Outbound(tag)
		if !loaded {
			return E.New("pool member not found: ", tag)
		}

		// Acquire shared state (creates if not exists, reuses if already created)
		state := acquireSharedState(tag)

		runtimeState := newMemberRuntime(detour, state)
		member := &memberState{
			outbound: detour,
			tag:      tag,
			shared:   state,
			runtime:  runtimeState,
			entry:    state.entryHandle(),
			meta:     p.options.Metadata[tag],
		}
		member.latencyMs.Store(-1)

		// Connect to existing monitor entry if available
		if p.monitor != nil {
			meta := p.options.Metadata[tag]
			var entry *monitor.EntryHandle
			if p.options.MonitorObserverOnly {
				entry = p.monitor.HandleForNodeID(meta.NodeID)
			} else {
				entry = p.monitor.MigrateRuntimeTag(meta.NodeID, monitor.NodeInfo{
					NodeID: meta.NodeID, Tag: tag, Name: meta.Name, URI: meta.URI, Mode: meta.Mode,
					ListenAddress: meta.ListenAddress, Port: meta.Port, Region: meta.Region, Country: meta.Country,
				})
			}
			if entry != nil {
				if !p.options.MonitorObserverOnly {
					state.attachEntry(entry)
				}
				member.entry = entry
				if !p.options.MonitorObserverOnly {
					entry.SetRelease(p.makeReleaseFunc(member))
					if probe := p.makeProbeFunc(member); probe != nil {
						entry.SetProbe(probe)
					}
					if dialer := p.makeDialerFunc(member); dialer != nil {
						entry.SetDialer(dialer)
					}
				}
			}
		}
		members = append(members, member)
		p.runtimeByTag[tag] = runtimeState
	}
	if p.options.SelectorTag != "" {
		detour, loaded := p.manager.Outbound(p.options.SelectorTag)
		if !loaded {
			return E.New("group selector not found: ", p.options.SelectorTag)
		}
		selector, ok := detour.(selectorOutbound)
		if !ok {
			return E.New("outbound is not a selector: ", p.options.SelectorTag)
		}
		p.selector = selector
	}
	p.topology = members
	p.topologyByTag = make(map[string]*MemberRef, len(members))
	for _, member := range members {
		p.topologyByTag[member.tag] = member
		if member.tag == p.options.PreferredMember {
			p.currentMember.Store(member)
		}
	}
	p.publishSnapshotLocked(members)

	return nil
}

// ReconcileMembers resolves a complete desired topology, atomically publishes
// its currently eligible candidates, and returns refs that are no longer
// reachable from the new topology. New outbounds must already exist in manager.
func (p *poolOutbound) ReconcileMembers(specs []RuntimeMemberSpec) (uint64, []RetiredMember, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reconcileMembersLocked(specs)
}

func (p *poolOutbound) reconcileMembersLocked(specs []RuntimeMemberSpec) (uint64, []RetiredMember, error) {
	if p.closed.Load() {
		return 0, nil, net.ErrClosed
	}
	oldTopology := p.topology
	oldCurrent := p.currentMember.Load()
	seen := make(map[string]struct{}, len(specs))
	resolved := make(map[string]adapter.Outbound, len(specs))
	for _, spec := range specs {
		if spec.Tag == "" {
			return 0, nil, errors.New("pool member tag is empty")
		}
		if _, exists := seen[spec.Tag]; exists {
			return 0, nil, E.New("duplicate pool member: ", spec.Tag)
		}
		seen[spec.Tag] = struct{}{}
		detour, loaded := p.manager.Outbound(spec.Tag)
		if !loaded {
			return 0, nil, E.New("pool member not found: ", spec.Tag)
		}
		resolved[spec.Tag] = detour
	}
	next := make([]*MemberRef, 0, len(specs))
	for _, spec := range specs {
		detour := resolved[spec.Tag]
		member := p.topologyByTag[spec.Tag]
		if member == nil || member.outbound != detour {
			state := acquireSharedState(spec.Tag)
			runtimeState := p.runtimeByTag[spec.Tag]
			if runtimeState == nil || runtimeState.outbound != detour {
				runtimeState = newMemberRuntime(detour, state)
				p.runtimeByTag[spec.Tag] = runtimeState
			}
			runtimeState.retired.Store(false)
			member = &MemberRef{outbound: detour, tag: spec.Tag, shared: state, runtime: runtimeState, entry: state.entryHandle(), meta: spec.Meta}
			member.latencyMs.Store(-1)
			p.attachMonitorMember(member)
		} else if member.meta != spec.Meta {
			// Metadata belongs to the immutable published ref. A metadata-only
			// update gets a new ref while retaining the same outbound and counters.
			replacement := &MemberRef{outbound: member.outbound, tag: member.tag, entry: member.entry, shared: member.shared, runtime: member.runtime, meta: spec.Meta}
			replacement.latencyMs.Store(member.latencyMs.Load())
			member = replacement
			p.attachMonitorMember(member)
		}
		next = append(next, member)
	}
	retired := make([]RetiredMember, 0)
	for _, member := range p.topology {
		if _, retained := seen[member.tag]; !retained {
			retired = append(retired, RetiredMember{ref: member, pool: p})
		}
	}
	p.topology = next
	p.topologyByTag = make(map[string]*MemberRef, len(next))
	for _, member := range next {
		p.topologyByTag[member.tag] = member
	}
	var nextCurrent *MemberRef
	if oldCurrent != nil {
		nextCurrent = p.topologyByTag[oldCurrent.tag]
		if nextCurrent == nil {
			oldNodeID := p.memberMeta(oldCurrent).NodeID
			if oldNodeID != 0 {
				for _, candidate := range next {
					if p.memberMeta(candidate).NodeID == oldNodeID {
						nextCurrent = candidate
						break
					}
				}
			}
		}
		if nextCurrent == nil && p.mode == modeFixed {
			oldIndex := -1
			for index, member := range oldTopology {
				if member == oldCurrent {
					oldIndex = index
					break
				}
			}
			if oldIndex >= 0 {
				for offset := 1; offset <= len(oldTopology); offset++ {
					if candidate := p.topologyByTag[oldTopology[(oldIndex+offset)%len(oldTopology)].tag]; candidate != nil {
						nextCurrent = candidate
						break
					}
				}
			}
		}
	}
	p.currentMember.Store(nextCurrent)
	for _, item := range retired {
		// Close the acquireOperation race before the new topology becomes
		// visible. Existing operations remain counted and are allowed to drain.
		item.ref.runtime.retired.Store(true)
	}
	snapshot := p.publishSnapshotLocked(next)
	return snapshot.Version, retired, nil
}

type preparedPoolUpdate struct {
	pool       *poolOutbound
	update     RuntimeUpdate
	rolledBack atomic.Bool
	committed  atomic.Bool
}

// PrepareUpdate validates and deep-copies a complete desired runtime without
// changing the current topology, group state, policy, or health schedule.
func (p *poolOutbound) PrepareUpdate(update RuntimeUpdate) (PreparedUpdate, error) {
	if len(update.Members) == 0 {
		return nil, errors.New("pool update requires at least one member")
	}
	copyUpdate := RuntimeUpdate{
		Members:             make([]RuntimeMemberSpec, len(update.Members)),
		Mode:                normalizeOptions(Options{Mode: update.Mode}).Mode,
		FailureWindow:       update.FailureWindow,
		FailureThreshold:    update.FailureThreshold,
		HealthCheckInterval: update.HealthCheckInterval,
		PreferredNodeID:     update.PreferredNodeID,
		InitialGroupState:   make(map[string]group.GroupInitialState, len(update.InitialGroupState)),
	}
	copy(copyUpdate.Members, update.Members)
	seen := make(map[string]struct{}, len(copyUpdate.Members))
	for index := range copyUpdate.Members {
		spec := &copyUpdate.Members[index]
		if spec.Tag == "" {
			return nil, errors.New("pool member tag is empty")
		}
		if _, exists := seen[spec.Tag]; exists {
			return nil, E.New("duplicate pool member: ", spec.Tag)
		}
		seen[spec.Tag] = struct{}{}
		detour, loaded := p.manager.Outbound(spec.Tag)
		if !loaded {
			return nil, E.New("pool member not found: ", spec.Tag)
		}
		networks := detour.Network()
		if !common.Contains(networks, N.NetworkTCP) && !common.Contains(networks, N.NetworkUDP) {
			return nil, E.New("pool member has no supported network: ", spec.Tag)
		}
	}
	for tag, state := range update.InitialGroupState {
		state.FailureHistory = append([]int64(nil), state.FailureHistory...)
		copyUpdate.InitialGroupState[tag] = state
	}
	return &preparedPoolUpdate{pool: p, update: copyUpdate}, nil
}

func (u *preparedPoolUpdate) Rollback() { u.rolledBack.Store(true) }

func (u *preparedPoolUpdate) Commit() (uint64, []RetiredMember, error) {
	if u == nil || u.pool == nil || u.rolledBack.Load() {
		return 0, nil, errors.New("pool update was rolled back")
	}
	if !u.committed.CompareAndSwap(false, true) {
		return 0, nil, errors.New("pool update already committed")
	}
	p := u.pool
	p.mu.Lock()
	if p.closed.Load() {
		p.mu.Unlock()
		return 0, nil, net.ErrClosed
	}
	oldMode := p.mode
	p.mode = u.update.Mode
	p.options.Mode = u.update.Mode
	p.options.FailureWindow = u.update.FailureWindow
	p.options.FailureThreshold = u.update.FailureThreshold
	p.options.HealthCheckInterval = u.update.HealthCheckInterval
	p.failureThreshold.Store(int64(u.update.FailureThreshold))
	p.failureWindowNanos.Store(int64(u.update.FailureWindow))
	events := group.ReconcileSilent(p.options.GroupID, group.RuntimeUpdate{
		FailureWindow: u.update.FailureWindow, FailureThreshold: u.update.FailureThreshold,
		PreferredNodeID: u.update.PreferredNodeID, Members: u.update.InitialGroupState,
	})
	version, retired, err := p.reconcileMembersLocked(u.update.Members)
	if err != nil {
		p.mode, p.options.Mode = oldMode, oldMode
		p.mu.Unlock()
		return 0, nil, err
	}
	memberTags := make([]string, 0, len(u.update.Members))
	memberNodeIDs := make([]int64, 0, len(u.update.Members))
	for _, spec := range u.update.Members {
		memberTags = append(memberTags, spec.Tag)
		memberNodeIDs = append(memberNodeIDs, spec.Meta.NodeID)
	}
	p.options.Members = memberTags
	oldSchedule := p.unregisterSchedule
	if p.monitor != nil && p.options.GroupID != 0 {
		p.unregisterSchedule = p.monitor.RegisterGroupHealthScheduleByNodeID(p.options.GroupID, memberNodeIDs, u.update.HealthCheckInterval)
	}
	p.mu.Unlock()
	if oldSchedule != nil {
		oldSchedule()
	}
	group.NotifyEvents(events)
	p.reconcileCurrent()
	return version, retired, nil
}

func (p *poolOutbound) attachMonitorMember(member *MemberRef) {
	if p.monitor == nil {
		return
	}
	meta := member.meta
	var entry *monitor.EntryHandle
	if p.options.MonitorObserverOnly {
		entry = p.monitor.HandleForNodeID(meta.NodeID)
	} else {
		entry = p.monitor.MigrateRuntimeTag(meta.NodeID, monitor.NodeInfo{NodeID: meta.NodeID, Tag: member.tag, Name: meta.Name, URI: meta.URI,
			Mode: meta.Mode, ListenAddress: meta.ListenAddress, Port: meta.Port, Region: meta.Region, Country: meta.Country})
	}
	if entry == nil {
		return
	}
	member.entry = entry
	if p.options.MonitorObserverOnly {
		return
	}
	member.shared.attachEntry(entry)
	entry.SetRelease(p.makeReleaseFunc(member))
	entry.SetProbe(p.makeProbeFunc(member))
	entry.SetDialer(p.makeDialerFunc(member))
}

func (p *poolOutbound) memberName(member *memberState) string {
	if meta := p.memberMeta(member); meta.Name != "" {
		return meta.Name
	}
	return member.tag
}

func (p *poolOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	dst := destination.String()
	snapshot, release, err := p.acquireSnapshot()
	if err != nil {
		return nil, err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	cursor, err := p.newAttemptCursor(snapshot, network)
	if err != nil {
		return nil, err
	}
	var dialErr error
	for member := cursor.next(); member != nil; member = cursor.next() {
		p.logger.Debug("→ ", dst, " ⇒ ", p.memberName(member), " [", network, "]")
		p.incActive(member)
		conn, err := p.dialSelected(ctx, member, network, destination)
		if err != nil {
			p.decActive(member)
			p.recordFailure(member, err, dst)
			dialErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		p.recordSuccess(member, dst)
		wrapped := p.wrapConnWithRelease(conn, member, dst, release)
		release = nil
		return wrapped, nil
	}
	if dialErr != nil {
		return nil, fmt.Errorf("all available proxies failed for %s: %w", dst, dialErr)
	}
	return nil, E.New("no healthy proxy available")
}

func (p *poolOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	dst := destination.String()
	snapshot, release, err := p.acquireSnapshot()
	if err != nil {
		return nil, err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	cursor, err := p.newAttemptCursor(snapshot, N.NetworkUDP)
	if err != nil {
		return nil, err
	}
	var listenErr error
	for member := cursor.next(); member != nil; member = cursor.next() {
		p.logger.Debug("→ ", dst, " ⇒ ", p.memberName(member), " [udp]")
		p.incActive(member)
		conn, err := p.listenPacketSelected(ctx, member, destination)
		if err != nil {
			p.decActive(member)
			p.recordFailure(member, err, dst)
			listenErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		p.recordSuccess(member, dst)
		wrapped := p.wrapPacketConnWithRelease(conn, member, dst, release)
		release = nil
		return wrapped, nil
	}
	if listenErr != nil {
		return nil, fmt.Errorf("all available proxies failed for %s: %w", dst, listenErr)
	}
	return nil, E.New("no healthy proxy available")
}

func (p *poolOutbound) dialSelected(ctx context.Context, member *memberState, network string, destination M.Socksaddr) (net.Conn, error) {
	if p.selector != nil {
		p.selectorMu.Lock()
		accepted := p.selector.SelectOutbound(member.tag)
		p.selectorMu.Unlock()
		if !accepted {
			return nil, E.New("selector rejected member: ", member.tag)
		}
	}
	p.waitForInitialLatency.Store(false)
	p.currentMember.Store(member)
	group.SetCurrentTag(p.options.GroupID, member.tag)
	return member.outbound.DialContext(ctx, network, destination)
}

func (p *poolOutbound) listenPacketSelected(ctx context.Context, member *memberState, destination M.Socksaddr) (net.PacketConn, error) {
	if p.selector != nil {
		p.selectorMu.Lock()
		accepted := p.selector.SelectOutbound(member.tag)
		p.selectorMu.Unlock()
		if !accepted {
			return nil, E.New("selector rejected member: ", member.tag)
		}
	}
	p.waitForInitialLatency.Store(false)
	p.currentMember.Store(member)
	group.SetCurrentTag(p.options.GroupID, member.tag)
	return member.outbound.ListenPacket(ctx, destination)
}

func (p *poolOutbound) pickMember(network string) (*memberState, error) {
	snapshot, err := p.loadSnapshot()
	if err != nil {
		return nil, err
	}
	cursor, err := p.newAttemptCursor(snapshot, network)
	if err != nil {
		return nil, err
	}
	return cursor.next(), nil
}

type attemptCursor struct {
	members []*MemberRef
	start   int
	used    int
}

func (c *attemptCursor) next() *MemberRef {
	if c.used >= len(c.members) {
		return nil
	}
	member := c.members[(c.start+c.used)%len(c.members)]
	c.used++
	return member
}

func (p *poolOutbound) newAttemptCursor(snapshot *PoolSnapshot, network string) (attemptCursor, error) {
	members := snapshot.tcpMembers
	if network == N.NetworkUDP {
		members = snapshot.udpMembers
	}
	if len(members) == 0 {
		return attemptCursor{}, E.New("no healthy proxy available")
	}
	return attemptCursor{members: members, start: p.selectionIndex(snapshot, members)}, nil
}

// availableMembers remains a control-plane helper; eligibility was already
// frozen when snapshot was published, so it never consults monitor/group state.
func (p *poolOutbound) availableMembers(snapshot *PoolSnapshot, now time.Time, network string, buf []*memberState) []*memberState {
	_ = now
	if network == N.NetworkTCP {
		return append(buf[:0], snapshot.tcpMembers...)
	}
	if network == N.NetworkUDP {
		return append(buf[:0], snapshot.udpMembers...)
	}
	return append(buf[:0], snapshot.Members...)
}

func (p *poolOutbound) selectMember(snapshot *PoolSnapshot, candidates []*memberState) *memberState {
	mode := snapshot.mode
	if mode == "" {
		mode = p.mode
	}
	if mode == modeLowestLatency && !sameCandidateSlice(candidates, snapshot.tcpMembers) && !sameCandidateSlice(candidates, snapshot.udpMembers) {
		if current := p.currentMember.Load(); current != nil {
			for _, member := range candidates {
				if member == current {
					return current
				}
			}
		}
		best := 0
		for index := 1; index < len(candidates); index++ {
			if p.latencyMemberLess(candidates[index], candidates[best]) {
				best = index
			}
		}
		return candidates[best]
	}
	return candidates[p.selectionIndex(snapshot, candidates)]
}

func sameCandidateSlice(left, right []*MemberRef) bool {
	return len(left) == len(right) && (len(left) == 0 || &left[0] == &right[0])
}

func (p *poolOutbound) selectionIndex(snapshot *PoolSnapshot, candidates []*MemberRef) int {
	mode := snapshot.mode
	if mode == "" {
		mode = p.mode
	}
	switch mode {
	case modeRandom:
		return int(p.nextRandom() % uint64(len(candidates)))
	case modeBalance:
		selected := 0
		var minActive int32
		for index, member := range candidates {
			var active int32
			if member.shared != nil {
				active = member.shared.activeCount()
			}
			if index == 0 || active < minActive {
				selected = index
				minActive = active
			}
		}
		return selected
	case modeFixed, modeLowestLatency:
		current := p.currentMember.Load()
		if current != nil {
			if index, found := snapshot.memberIndex[current]; found && index < len(candidates) && candidates[index] == current {
				return index
			}
			if index, found := snapshot.tcpIndex[current]; found && index < len(candidates) && candidates[index] == current {
				return index
			}
			if index, found := snapshot.udpIndex[current]; found && index < len(candidates) && candidates[index] == current {
				return index
			}
		}
		if mode == modeLowestLatency {
			return 0
		}
		return p.nextFixedIndex(snapshot, candidates, current)
	default:
		return int(p.rrCounter.Add(1)-1) % len(candidates)
	}
}

func (p *poolOutbound) nextRandom() uint64 {
	for {
		old := p.randomState.Load()
		next := old
		next ^= next << 13
		next ^= next >> 7
		next ^= next << 17
		if next == 0 {
			next = 0x9e3779b97f4a7c15
		}
		if p.randomState.CompareAndSwap(old, next) {
			return next
		}
	}
}

func memberByTag(candidates []*memberState, tag string) *memberState {
	if tag == "" {
		return nil
	}
	for _, candidate := range candidates {
		if candidate.tag == tag {
			return candidate
		}
	}
	return nil
}

func (p *poolOutbound) nextFixedIndex(snapshot *PoolSnapshot, candidates []*memberState, previous *MemberRef) int {
	previousIndex := -1
	for index, member := range snapshot.allMembers {
		if member == previous {
			previousIndex = index
			break
		}
	}
	if previousIndex < 0 {
		return 0
	}
	for offset := 1; offset <= len(snapshot.allMembers); offset++ {
		member := snapshot.allMembers[(previousIndex+offset)%len(snapshot.allMembers)]
		for index, candidate := range candidates {
			if candidate == member {
				return index
			}
		}
	}
	return 0
}

func (p *poolOutbound) latencyMemberLess(left, right *memberState) bool {
	leftLatency, rightLatency := left.latencyMs.Load(), right.latencyMs.Load()
	leftKnown, rightKnown := leftLatency > 0, rightLatency > 0
	if leftKnown != rightKnown {
		return leftKnown
	}
	if leftKnown && leftLatency != rightLatency {
		return leftLatency < rightLatency
	}
	leftID := p.memberMeta(left).NodeID
	rightID := p.memberMeta(right).NodeID
	if leftID != rightID {
		return leftID < rightID
	}
	return left.tag < right.tag
}

func (p *poolOutbound) memberMeta(member *MemberRef) MemberMeta {
	if member == nil {
		return MemberMeta{}
	}
	if member.meta != (MemberMeta{}) {
		return member.meta
	}
	return p.options.Metadata[member.tag]
}

func (p *poolOutbound) handleGroupState(event group.GroupStateEvent) {
	if event.GroupID != p.options.GroupID || p.closed.Load() {
		return
	}
	if event.CurrentChanged {
		p.mu.Lock()
		var current *MemberRef
		if event.CurrentNodeID != 0 {
			for _, member := range p.topology {
				if p.memberMeta(member).NodeID == event.CurrentNodeID {
					current = member
					break
				}
			}
		}
		p.currentMember.Store(current)
		p.mu.Unlock()
	}
	if event.StateChanged {
		if event.Recovered {
			p.scheduleCandidateRebuild()
		} else {
			p.rebuildCandidatesNow()
		}
	}
	if event.CurrentChanged {
		p.reconcileCurrent()
	}
}

func (p *poolOutbound) handleHealthResult(event monitor.HealthResultEvent) {
	tag := event.Tag
	if p.options.GroupID != 0 && event.NodeID != 0 {
		p.mu.Lock()
		for _, member := range p.topology {
			if p.memberMeta(member).NodeID == event.NodeID {
				tag = member.tag
				break
			}
		}
		p.mu.Unlock()
	}
	if !p.hasMember(tag) {
		return
	}
	if p.options.GroupID != 0 {
		if event.Success {
			group.RecordHealthSuccess(p.options.GroupID, tag)
		} else {
			group.RecordGroupHealthFailure(p.options.GroupID, tag, errors.New(event.Error), event.CheckedAt)
			p.scheduleExpiryRebuild(p.currentFailureWindow())
		}
	}
	p.mu.Lock()
	if member := p.topologyByTag[tag]; member != nil {
		if event.Success {
			latency := event.Latency.Milliseconds()
			if latency == 0 && event.Latency > 0 {
				latency = 1
			}
			member.latencyMs.Store(latency)
		}
	}
	p.mu.Unlock()
	if event.Success {
		p.scheduleCandidateRebuild()
	} else {
		p.rebuildCandidatesNow()
	}
	p.reconcileCurrent()
}

func (p *poolOutbound) hasMember(tag string) bool {
	p.mu.Lock()
	_, found := p.topologyByTag[tag]
	p.mu.Unlock()
	return found
}

func (p *poolOutbound) scheduleCandidateRebuild() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return
	}
	if p.rebuildTimer == nil {
		p.rebuildTimer = time.AfterFunc(100*time.Millisecond, p.rebuildCandidatesNow)
	} else {
		p.rebuildTimer.Reset(100 * time.Millisecond)
	}
}

func (p *poolOutbound) scheduleExpiryRebuild(after time.Duration) {
	if after <= 0 {
		return
	}
	// Timers may fire a fraction early relative to the wall-clock expiry. A
	// small guard prevents rebuilding once just before expiry and then leaving
	// the candidate stale forever.
	after += time.Millisecond
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return
	}
	if p.expiryTimer == nil {
		p.expiryTimer = time.AfterFunc(after, p.rebuildCandidatesNow)
	} else {
		p.expiryTimer.Reset(after)
	}
}

func (p *poolOutbound) rebuildCandidatesNow() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() || len(p.topology) == 0 {
		return
	}
	p.publishSnapshotLocked(p.topology)
}

func (p *poolOutbound) reconcileCurrent() {
	snapshot, err := p.loadSnapshot()
	if err != nil {
		p.logger.Warn("load pool snapshot for reconciliation: ", err)
		return
	}
	current := p.currentMember.Load()
	for _, candidate := range snapshot.Members {
		if candidate == current {
			p.waitForInitialLatency.Store(false)
			return
		}
	}
	if p.waitForInitialLatency.Load() && !p.allInitialChecksDone(snapshot) {
		return
	}
	p.waitForInitialLatency.Store(false)
	if len(snapshot.Members) == 0 {
		p.currentMember.Store(nil)
		group.SetCurrentTag(p.options.GroupID, "")
		return
	}
	selected := p.selectMember(snapshot, snapshot.Members)
	p.selectorMu.Lock()
	if p.selector != nil && !p.selector.SelectOutbound(selected.tag) {
		p.selectorMu.Unlock()
		p.logger.Warn("selector rejected hot replacement: ", selected.tag)
		return
	}
	p.selectorMu.Unlock()
	p.currentMember.Store(selected)
	group.SetCurrentTag(p.options.GroupID, selected.tag)
}

func (p *poolOutbound) allInitialChecksDone(snapshot *PoolSnapshot) bool {
	if p.monitor == nil {
		return true
	}
	for _, member := range snapshot.allMembers {
		monitorSnapshot := p.monitor.SnapshotForTag(member.tag)
		if p.options.MonitorObserverOnly && p.memberMeta(member).NodeID != 0 {
			monitorSnapshot = p.monitor.SnapshotForNodeID(p.memberMeta(member).NodeID)
		}
		if monitorSnapshot == nil || !monitorSnapshot.InitialCheckDone {
			return false
		}
	}
	return true
}

func (p *poolOutbound) recordFailure(member *memberState, cause error, destination string) {
	if p.options.GroupID != 0 {
		evicted := group.RecordFailure(p.options.GroupID, member.tag, cause, time.Now())
		if member.shared != nil {
			if entry := member.shared.entryHandle(); entry != nil {
				entry.RecordFailure(cause, destination)
			}
		}
		if evicted {
			p.logger.Warn("group ", p.options.GroupID, " permanently evicted ", member.tag, ": ", cause)
		} else {
			p.logger.Warn("group ", p.options.GroupID, " marked ", member.tag, " suspect: ", cause)
		}
		p.rebuildCandidatesNow()
		p.scheduleExpiryRebuild(p.currentFailureWindow())
		return
	}
	if member.shared == nil {
		p.logger.Warn("proxy ", member.tag, " failure (no shared state): ", cause)
		return
	}
	threshold := p.currentFailureThreshold()
	failures, blacklisted, until := member.shared.recordFailure(cause, threshold, p.options.BlacklistDuration, destination)
	if blacklisted {
		p.logger.Warn("proxy ", member.tag, " blacklisted for ", p.options.BlacklistDuration, ": ", cause)
		p.rebuildCandidatesNow()
		p.scheduleExpiryRebuild(time.Until(until))
	} else {
		p.logger.Warn("proxy ", member.tag, " failure ", failures, "/", threshold, ": ", cause)
	}
}

func (p *poolOutbound) currentFailureThreshold() int {
	threshold := int(p.failureThreshold.Load())
	if threshold <= 0 {
		threshold = p.options.FailureThreshold
	}
	if threshold <= 0 {
		threshold = 3
	}
	return threshold
}

func (p *poolOutbound) currentFailureWindow() time.Duration {
	window := time.Duration(p.failureWindowNanos.Load())
	if window <= 0 {
		window = p.options.FailureWindow
	}
	return window
}

func (p *poolOutbound) recordSuccess(member *memberState, destination string) {
	if p.options.GroupID != 0 {
		if member.shared != nil {
			if entry := member.shared.entryHandle(); entry != nil {
				entry.RecordSuccess(destination)
			}
		}
		return
	}
	if member.shared != nil {
		member.shared.recordSuccess(destination)
	}
}

func (p *poolOutbound) wrapConn(conn net.Conn, member *memberState, destination string) net.Conn {
	return p.wrapConnWithRelease(conn, member, destination, nil)
}

func (p *poolOutbound) wrapConnWithRelease(conn net.Conn, member *memberState, destination string, snapshotRelease func()) net.Conn {
	return &trackedConn{
		Conn: conn,
		release: func() {
			p.decActive(member)
			if snapshotRelease != nil {
				snapshotRelease()
			}
		},
		onTraffic: func(upload, download int64) {
			if member.shared != nil {
				member.shared.addTraffic(upload, download)
			}
		},
		onError: func(err error) {
			p.recordEstablishedIOError(member, err, destination)
		},
		onEstablishedSuccess: func() {
			if member.shared != nil {
				member.shared.recordEstablishedSuccess()
			}
		},
	}
}

func (p *poolOutbound) wrapPacketConn(conn net.PacketConn, member *memberState, destination string) net.PacketConn {
	return p.wrapPacketConnWithRelease(conn, member, destination, nil)
}

func (p *poolOutbound) wrapPacketConnWithRelease(conn net.PacketConn, member *memberState, destination string, snapshotRelease func()) net.PacketConn {
	return &trackedPacketConn{
		PacketConn: conn,
		release: func() {
			p.decActive(member)
			if snapshotRelease != nil {
				snapshotRelease()
			}
		},
		onTraffic: func(upload, download int64) {
			if member.shared != nil {
				member.shared.addTraffic(upload, download)
			}
		},
		onError: func(err error) {
			p.recordEstablishedIOError(member, err, destination)
		},
		onEstablishedSuccess: func() {
			if member.shared != nil {
				member.shared.recordEstablishedSuccess()
			}
		},
	}
}

func (p *poolOutbound) recordEstablishedIOError(member *memberState, cause error, destination string) {
	if p.options.GroupID == 0 {
		p.recordFailure(member, cause, destination)
		return
	}
	if member.shared != nil {
		if entry := member.shared.entryHandle(); entry != nil {
			entry.RecordFailure(cause, destination)
		}
	}
	p.logger.Warn("group ", p.options.GroupID, " connection I/O error on ", member.tag, ": ", cause)
}

func (p *poolOutbound) makeReleaseFunc(member *memberState) func() {
	return func() {
		if member.shared != nil {
			member.shared.forceRelease()
		}
	}
}

// httpProbe performs a scheme-aware HTTP probe through an already established
// node connection. A probe succeeds only after a valid 204 response is parsed.
func httpProbe(ctx context.Context, conn net.Conn, target monitor.ProbeTarget, tlsConfig *tls.Config) (time.Duration, error) {
	deadline := time.Now().Add(10 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = conn.SetDeadline(deadline)
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()

	start := time.Now()
	probeConn := conn
	if target.Scheme == "https" {
		config := &tls.Config{MinVersion: tls.VersionTLS12}
		if tlsConfig != nil {
			config = tlsConfig.Clone()
			if config.MinVersion == 0 {
				config.MinVersion = tls.VersionTLS12
			}
		}
		config.ServerName = target.ServerName
		config.NextProtos = []string{"http/1.1"}
		tlsConn := tls.Client(conn, config)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return 0, fmt.Errorf("TLS handshake: %w", err)
		}
		probeConn = tlsConn
	}

	requestURL := target.Scheme + "://" + target.Host + target.RequestURI
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return 0, fmt.Errorf("build probe request: %w", err)
	}
	request.Header.Set("Connection", "close")
	request.Header.Set("User-Agent", "EasyProxies/health-check")
	if err := request.Write(probeConn); err != nil {
		return 0, fmt.Errorf("write probe request: %w", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(probeConn), request)
	if err != nil {
		return 0, fmt.Errorf("read probe response: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return 0, fmt.Errorf("unexpected probe status: %s", response.Status)
	}
	return time.Since(start), nil
}

func (p *poolOutbound) makeProbeFunc(member *memberState) func(ctx context.Context) (time.Duration, error) {
	if p.monitor == nil {
		return nil
	}
	return func(ctx context.Context) (time.Duration, error) {
		// 每次执行时动态获取最新的探测目标
		target, ok := p.monitor.TargetForProbe()
		if !ok {
			return 0, E.New("probe target not configured")
		}

		return p.probeMember(ctx, member, target)
	}
}

// makeProbeByTagFunc creates a probe function that works before member initialization
func (p *poolOutbound) makeProbeByTagFunc(tag string) func(ctx context.Context) (time.Duration, error) {
	if p.monitor == nil {
		return nil
	}
	return func(ctx context.Context) (time.Duration, error) {
		// 每次执行时动态获取最新的探测目标
		target, ok := p.monitor.TargetForProbe()
		if !ok {
			return 0, E.New("probe target not configured")
		}

		p.mu.Lock()
		member := p.topologyByTag[tag]
		p.mu.Unlock()
		if member == nil {
			return 0, E.New("member not found: ", tag)
		}

		return p.probeMember(ctx, member, target)
	}
}

func (p *poolOutbound) probeMember(ctx context.Context, member *memberState, target monitor.ProbeTarget) (time.Duration, error) {
	if !member.acquireOperation() {
		return 0, E.New("member retired: ", member.tag)
	}
	defer member.releaseOperation()
	dialTimeout, responseTimeout := p.monitor.ProbePhaseTimeoutsFor(ctx)
	start := time.Now()
	dialCtx, cancelDial := context.WithTimeout(ctx, dialTimeout)
	conn, err := member.outbound.DialContext(dialCtx, N.NetworkTCP, target.Destination)
	cancelDial()
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	responseCtx, cancelResponse := context.WithTimeout(ctx, responseTimeout)
	_, err = httpProbe(responseCtx, conn, target, nil)
	cancelResponse()
	if err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// makeReleaseByTagFunc creates a release function that works before member initialization
func (p *poolOutbound) makeReleaseByTagFunc(tag string) func() {
	return func() {
		releaseSharedMember(tag)
		p.rebuildCandidatesNow()
	}
}

// makeDialerFunc returns a DialerFunc that dials an arbitrary address
// ("host:port") through the given member's outbound. It mirrors makeProbeFunc
// but exposes the raw connection to any destination instead of the fixed
// probe target.
func (p *poolOutbound) makeDialerFunc(member *memberState) monitor.DialerFunc {
	if p.monitor == nil {
		return nil
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if !member.acquireOperation() {
			return nil, E.New("member retired: ", member.tag)
		}
		destination := M.ParseSocksaddr(address)
		if !destination.IsValid() {
			member.releaseOperation()
			return nil, E.New("invalid dial address: ", address)
		}
		nw := network
		if nw == "" {
			nw = N.NetworkTCP
		}
		conn, err := member.outbound.DialContext(ctx, nw, destination)
		if err != nil {
			member.releaseOperation()
			return nil, err
		}
		return &operationConn{Conn: conn, release: member.releaseOperation}, nil
	}
}

// makeDialerByTagFunc returns a DialerFunc for a tag, resolving the member on
// first use (works before member initialization, like makeProbeByTagFunc).
func (p *poolOutbound) makeDialerByTagFunc(tag string) monitor.DialerFunc {
	if p.monitor == nil {
		return nil
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		p.mu.Lock()
		member := p.topologyByTag[tag]
		p.mu.Unlock()
		if member == nil {
			return nil, E.New("member not found: ", tag)
		}
		return p.makeDialerFunc(member)(ctx, network, address)
	}
}

func (m *MemberRef) acquireOperation() bool {
	runtimeState := ensureMemberRuntime(m)
	if runtimeState.retired.Load() {
		return false
	}
	runtimeState.operations.Add(1)
	if runtimeState.retired.Load() {
		runtimeState.operations.Add(-1)
		return false
	}
	return true
}

func (m *MemberRef) releaseOperation() { ensureMemberRuntime(m).operations.Add(-1) }

func ensureMemberRuntime(member *MemberRef) *memberRuntime {
	if member.runtime == nil {
		member.runtime = newMemberRuntime(member.outbound, member.shared)
	}
	return member.runtime
}

type operationConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *operationConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

type trackedConn struct {
	net.Conn
	once                 sync.Once
	release              func()
	onTraffic            func(upload, download int64)
	onError              func(error)
	errorOnce            sync.Once
	onEstablishedSuccess func()
	successOnce          sync.Once
}

func (c *trackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.onTraffic != nil {
		c.onTraffic(0, int64(n))
	}
	if n > 0 && c.onEstablishedSuccess != nil {
		c.successOnce.Do(c.onEstablishedSuccess)
	}
	c.recordIOError(err)
	return n, err
}

func (c *trackedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 && c.onTraffic != nil {
		c.onTraffic(int64(n), 0)
	}
	c.recordIOError(err)
	return n, err
}

func (c *trackedConn) recordIOError(err error) {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return
	}
	c.errorOnce.Do(func() {
		if c.onError != nil {
			c.onError(err)
		}
	})
}

func (c *trackedConn) CloseWrite() error {
	conn, ok := c.Conn.(interface{ CloseWrite() error })
	if !ok {
		return E.New("underlying connection does not support CloseWrite")
	}
	return conn.CloseWrite()
}

func (c *trackedConn) CloseRead() error {
	conn, ok := c.Conn.(interface{ CloseRead() error })
	if !ok {
		return E.New("underlying connection does not support CloseRead")
	}
	return conn.CloseRead()
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

type trackedPacketConn struct {
	net.PacketConn
	once                 sync.Once
	release              func()
	onTraffic            func(upload, download int64)
	onError              func(error)
	errorOnce            sync.Once
	onEstablishedSuccess func()
	successOnce          sync.Once
}

func (c *trackedPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(b)
	if n > 0 && c.onTraffic != nil {
		c.onTraffic(0, int64(n))
	}
	if n > 0 && c.onEstablishedSuccess != nil {
		c.successOnce.Do(c.onEstablishedSuccess)
	}
	c.recordIOError(err)
	return n, addr, err
}

func (c *trackedPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(b, addr)
	if n > 0 && c.onTraffic != nil {
		c.onTraffic(int64(n), 0)
	}
	c.recordIOError(err)
	return n, err
}

func (c *trackedPacketConn) recordIOError(err error) {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return
	}
	c.errorOnce.Do(func() {
		if c.onError != nil {
			c.onError(err)
		}
	})
}

func (c *trackedPacketConn) Close() error {
	err := c.PacketConn.Close()
	c.once.Do(c.release)
	return err
}

func (p *poolOutbound) incActive(member *memberState) {
	if member.shared != nil {
		member.shared.incActive()
	}
}

func (p *poolOutbound) decActive(member *memberState) {
	if member.shared != nil {
		member.shared.decActive()
	}
}
