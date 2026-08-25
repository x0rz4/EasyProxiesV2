package pool

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
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

type memberState struct {
	outbound adapter.Outbound
	tag      string
	entry    *monitor.EntryHandle
	shared   *sharedMemberState
}

type poolOutbound struct {
	outbound.Adapter
	ctx                   context.Context
	logger                log.ContextLogger
	manager               adapter.OutboundManager
	options               Options
	mode                  string
	members               []*memberState
	mu                    sync.Mutex
	rrCounter             atomic.Uint32
	rng                   *rand.Rand
	rngMu                 sync.Mutex // protects rng for random mode
	monitor               *monitor.Manager
	selector              selectorOutbound
	selectorMu            sync.Mutex
	waitForInitialLatency atomic.Bool
	candidatesPool        sync.Pool
	unsubscribeHealth     func()
	unsubscribeActivation func()
	closeOnce             sync.Once
}

type selectorOutbound interface {
	adapter.Outbound
	SelectOutbound(tag string) bool
	Now() string
}

func newPool(ctx context.Context, _ adapter.Router, logger log.ContextLogger, tag string, options Options) (adapter.Outbound, error) {
	if len(options.Members) == 0 {
		return nil, E.New("pool requires at least one member")
	}
	manager := service.FromContext[adapter.OutboundManager](ctx)
	if manager == nil {
		return nil, E.New("missing outbound manager in context")
	}
	monitorMgr := monitor.FromContext(ctx)
	normalized := normalizeOptions(options)
	memberCount := len(normalized.Members)
	dependencies := append([]string(nil), normalized.Members...)
	if normalized.SelectorTag != "" {
		dependencies = append(dependencies, normalized.SelectorTag)
	}
	p := &poolOutbound{
		Adapter: outbound.NewAdapter(Type, tag, []string{N.NetworkTCP, N.NetworkUDP}, dependencies),
		ctx:     ctx,
		logger:  logger,
		manager: manager,
		options: normalized,
		mode:    normalized.Mode,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		monitor: monitorMgr,
		candidatesPool: sync.Pool{
			New: func() any {
				return make([]*memberState, 0, memberCount)
			},
		},
	}
	if normalized.Mode == modeLowestLatency && normalized.PreferredMember == "" {
		p.waitForInitialLatency.Store(true)
	}
	group.Register(normalized.GroupID, normalized.FailureWindow, normalized.FailureThreshold,
		normalized.PreferredMember, normalized.InitialGroupState)

	// Register nodes immediately if monitor is available
	if monitorMgr != nil {
		logger.Info("registering ", len(normalized.Members), " nodes to monitor")
		for _, memberTag := range normalized.Members {
			// Acquire shared state for this tag (creates if not exists)
			state := acquireSharedState(memberTag)

			meta := normalized.Metadata[memberTag]
			info := monitor.NodeInfo{
				Tag:           memberTag,
				Name:          meta.Name,
				URI:           meta.URI,
				Mode:          meta.Mode,
				ListenAddress: meta.ListenAddress,
				Port:          meta.Port,
				Region:        meta.Region,
				Country:       meta.Country,
			}
			entry := monitorMgr.Register(info)
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
	if p.monitor != nil && p.options.GroupID != 0 {
		p.monitor.RegisterGroupHealthSchedule(p.options.GroupID, p.options.Members, p.options.HealthCheckInterval)
		p.unsubscribeHealth = p.monitor.SubscribeHealthResults(p.handleHealthResult)
		p.reconcileCurrent()
		p.unsubscribeActivation = group.RegisterActivationHandler(p.options.GroupID, p.activateNodeID)
	}
	// 在初始化完成后，立即在后台触发健康检查
	if p.monitor != nil {
		go p.probeAllMembersOnStartup()
	}
	return nil
}

func (p *poolOutbound) Close() error {
	p.closeOnce.Do(func() {
		if p.unsubscribeActivation != nil {
			p.unsubscribeActivation()
		}
		if p.monitor != nil && p.options.GroupID != 0 {
			p.monitor.UnregisterGroupHealthSchedule(p.options.GroupID)
		}
		if p.unsubscribeHealth != nil {
			p.unsubscribeHealth()
		}
	})
	return nil
}

func (p *poolOutbound) activateNodeID(nodeID int64) error {
	candidates := p.getCandidateBuffer()
	p.mu.Lock()
	candidates = p.availableMembersLocked(time.Now(), "", candidates)
	p.mu.Unlock()
	var selected *memberState
	for _, candidate := range candidates {
		if p.options.Metadata[candidate.tag].NodeID == nodeID {
			selected = candidate
			break
		}
	}
	p.putCandidateBuffer(candidates)
	if selected == nil {
		return errors.New("node is not a healthy running group member")
	}
	p.selectorMu.Lock()
	defer p.selectorMu.Unlock()
	if p.selector != nil && !p.selector.SelectOutbound(selected.tag) {
		return errors.New("selector rejected group member")
	}
	p.waitForInitialLatency.Store(false)
	group.SetCurrentTag(p.options.GroupID, selected.tag)
	return nil
}

// initializeMembersLocked must be called with p.mu held
func (p *poolOutbound) initializeMembersLocked() error {
	if len(p.members) > 0 {
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

		member := &memberState{
			outbound: detour,
			tag:      tag,
			shared:   state,
			entry:    state.entryHandle(),
		}

		// Connect to existing monitor entry if available
		if p.monitor != nil {
			meta := p.options.Metadata[tag]
			info := monitor.NodeInfo{
				Tag:           tag,
				Name:          meta.Name,
				URI:           meta.URI,
				Mode:          meta.Mode,
				ListenAddress: meta.ListenAddress,
				Port:          meta.Port,
				Region:        meta.Region,
				Country:       meta.Country,
			}
			entry := p.monitor.Register(info)
			if entry != nil {
				state.attachEntry(entry)
				member.entry = entry
				entry.SetRelease(p.makeReleaseFunc(member))
				if probe := p.makeProbeFunc(member); probe != nil {
					entry.SetProbe(probe)
				}
				if dialer := p.makeDialerFunc(member); dialer != nil {
					entry.SetDialer(dialer)
				}
			}
		}
		members = append(members, member)
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
	p.members = members
	p.logger.Info("pool initialized with ", len(members), " members")

	return nil
}

// probeAllMembersOnStartup performs initial health checks on all members
func (p *poolOutbound) probeAllMembersOnStartup() {
	_, ok := p.monitor.DestinationForProbe()
	if !ok {
		p.logger.Warn("probe target not configured, skipping initial health check")
		// 没有配置探测目标时，标记所有节点为可用
		p.mu.Lock()
		members := append([]*memberState(nil), p.members...)
		p.mu.Unlock()
		for _, member := range members {
			_ = p.monitor.MarkAvailableWithoutProbe(member.tag)
		}
		return
	}
	p.monitor.RequestProbeAllOnce(15 * time.Second)
}

func (p *poolOutbound) memberName(member *memberState) string {
	if meta, ok := p.options.Metadata[member.tag]; ok && meta.Name != "" {
		return meta.Name
	}
	return member.tag
}

func (p *poolOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	dst := destination.String()
	attempted := make(map[*memberState]struct{})
	var dialErr error
	for {
		member, err := p.pickMemberExcluding(network, attempted)
		if err != nil {
			if dialErr != nil {
				return nil, fmt.Errorf("all available proxies failed for %s: %w", dst, dialErr)
			}
			return nil, err
		}
		attempted[member] = struct{}{}
		p.logger.Info("→ ", dst, " ⇒ ", p.memberName(member), " [", network, "]")
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
		return p.wrapConn(conn, member, dst), nil
	}
}

func (p *poolOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	dst := destination.String()
	attempted := make(map[*memberState]struct{})
	var listenErr error
	for {
		member, err := p.pickMemberExcluding(N.NetworkUDP, attempted)
		if err != nil {
			if listenErr != nil {
				return nil, fmt.Errorf("all available proxies failed for %s: %w", dst, listenErr)
			}
			return nil, err
		}
		attempted[member] = struct{}{}
		p.logger.Info("→ ", dst, " ⇒ ", p.memberName(member), " [udp]")
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
		return p.wrapPacketConn(conn, member, dst), nil
	}
}

func (p *poolOutbound) dialSelected(ctx context.Context, member *memberState, network string, destination M.Socksaddr) (net.Conn, error) {
	if p.selector == nil {
		return member.outbound.DialContext(ctx, network, destination)
	}
	p.selectorMu.Lock()
	defer p.selectorMu.Unlock()
	if !p.selector.SelectOutbound(member.tag) {
		return nil, E.New("selector rejected member: ", member.tag)
	}
	p.waitForInitialLatency.Store(false)
	group.SetCurrentTag(p.options.GroupID, member.tag)
	return p.selector.DialContext(ctx, network, destination)
}

func (p *poolOutbound) listenPacketSelected(ctx context.Context, member *memberState, destination M.Socksaddr) (net.PacketConn, error) {
	if p.selector == nil {
		return member.outbound.ListenPacket(ctx, destination)
	}
	p.selectorMu.Lock()
	defer p.selectorMu.Unlock()
	if !p.selector.SelectOutbound(member.tag) {
		return nil, E.New("selector rejected member: ", member.tag)
	}
	p.waitForInitialLatency.Store(false)
	group.SetCurrentTag(p.options.GroupID, member.tag)
	return p.selector.ListenPacket(ctx, destination)
}

func (p *poolOutbound) pickMember(network string) (*memberState, error) {
	return p.pickMemberExcluding(network, nil)
}

func (p *poolOutbound) pickMemberExcluding(network string, excluded map[*memberState]struct{}) (*memberState, error) {
	now := time.Now()
	candidates := p.getCandidateBuffer()

	p.mu.Lock()
	if len(p.members) == 0 {
		if err := p.initializeMembersLocked(); err != nil {
			p.mu.Unlock()
			p.putCandidateBuffer(candidates)
			return nil, err
		}
	}
	candidates = p.availableMembersLocked(now, network, candidates)
	candidates = excludeMembers(candidates, excluded)
	p.mu.Unlock()

	if len(candidates) == 0 {
		p.mu.Lock()
		if p.releaseIfAllBlacklistedLocked(now) {
			candidates = p.availableMembersLocked(now, network, candidates)
			candidates = excludeMembers(candidates, excluded)
		}
		p.mu.Unlock()
	}

	if len(candidates) == 0 {
		p.putCandidateBuffer(candidates)
		return nil, E.New("no healthy proxy available")
	}

	member := p.selectMember(candidates)
	p.putCandidateBuffer(candidates)
	return member, nil
}

func excludeMembers(candidates []*memberState, excluded map[*memberState]struct{}) []*memberState {
	if len(excluded) == 0 {
		return candidates
	}
	result := candidates[:0]
	for _, member := range candidates {
		if _, ok := excluded[member]; !ok {
			result = append(result, member)
		}
	}
	return result
}

func (p *poolOutbound) availableMembersLocked(now time.Time, network string, buf []*memberState) []*memberState {
	result := buf[:0]
	for _, member := range p.members {
		if p.options.GroupID != 0 && !group.MemberAvailable(p.options.GroupID, member.tag) {
			continue
		}
		if p.options.GroupID != 0 && p.monitor != nil {
			snapshot := p.monitor.SnapshotForTag(member.tag)
			if snapshot == nil || !snapshot.InitialCheckDone || !snapshot.Available || snapshot.Blacklisted {
				continue
			}
		}
		// Check blacklist via shared state (auto-clears if expired)
		if member.shared != nil && member.shared.isBlacklisted(now) {
			continue
		}
		if network != "" && !common.Contains(member.outbound.Network(), network) {
			continue
		}
		result = append(result, member)
	}
	return result
}

func (p *poolOutbound) releaseIfAllBlacklistedLocked(now time.Time) bool {
	if len(p.members) == 0 {
		return false
	}
	// Check if all members are blacklisted
	for _, member := range p.members {
		if member.shared == nil || !member.shared.isBlacklisted(now) {
			return false
		}
	}
	// All blacklisted, force release all
	for _, member := range p.members {
		if member.shared != nil {
			member.shared.forceRelease()
		}
	}
	p.logger.Warn("all upstream proxies were blacklisted, releasing them for retry")
	return true
}

func (p *poolOutbound) selectMember(candidates []*memberState) *memberState {
	switch p.mode {
	case modeRandom:
		p.rngMu.Lock()
		idx := p.rng.Intn(len(candidates))
		p.rngMu.Unlock()
		return candidates[idx]
	case modeBalance:
		var selected *memberState
		var minActive int32
		for _, member := range candidates {
			var active int32
			if member.shared != nil {
				active = member.shared.activeCount()
			}
			if selected == nil || active < minActive {
				selected = member
				minActive = active
			}
		}
		return selected
	case modeFixed:
		if current := memberByTag(candidates, group.CurrentTag(p.options.GroupID)); current != nil {
			return current
		}
		return p.nextFixedMember(candidates, p.selectorCurrentTag())
	case modeLowestLatency:
		if current := memberByTag(candidates, group.CurrentTag(p.options.GroupID)); current != nil {
			return current
		}
		return p.lowestLatencyMember(candidates)
	default:
		idx := int(p.rrCounter.Add(1)-1) % len(candidates)
		return candidates[idx]
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

func (p *poolOutbound) selectorCurrentTag() string {
	p.selectorMu.Lock()
	defer p.selectorMu.Unlock()
	if p.selector == nil {
		return ""
	}
	return p.selector.Now()
}

func (p *poolOutbound) nextFixedMember(candidates []*memberState, previousTag string) *memberState {
	if previousTag == "" || len(p.members) == 0 {
		return candidates[0]
	}
	available := make(map[string]*memberState, len(candidates))
	for _, candidate := range candidates {
		available[candidate.tag] = candidate
	}
	previousIndex := -1
	for index, member := range p.members {
		if member.tag == previousTag {
			previousIndex = index
			break
		}
	}
	if previousIndex < 0 {
		return candidates[0]
	}
	for offset := 1; offset <= len(p.members); offset++ {
		member := p.members[(previousIndex+offset)%len(p.members)]
		if candidate := available[member.tag]; candidate != nil {
			return candidate
		}
	}
	return candidates[0]
}

func (p *poolOutbound) lowestLatencyMember(candidates []*memberState) *memberState {
	selected := candidates[0]
	for _, candidate := range candidates[1:] {
		if p.latencyMemberLess(candidate, selected) {
			selected = candidate
		}
	}
	return selected
}

func (p *poolOutbound) latencyMemberLess(left, right *memberState) bool {
	leftLatency, rightLatency := int64(-1), int64(-1)
	if p.monitor != nil {
		if snapshot := p.monitor.SnapshotForTag(left.tag); snapshot != nil {
			leftLatency = snapshot.LastLatencyMs
		}
		if snapshot := p.monitor.SnapshotForTag(right.tag); snapshot != nil {
			rightLatency = snapshot.LastLatencyMs
		}
	}
	leftKnown, rightKnown := leftLatency > 0, rightLatency > 0
	if leftKnown != rightKnown {
		return leftKnown
	}
	if leftKnown && leftLatency != rightLatency {
		return leftLatency < rightLatency
	}
	leftID := p.options.Metadata[left.tag].NodeID
	rightID := p.options.Metadata[right.tag].NodeID
	if leftID != rightID {
		return leftID < rightID
	}
	return left.tag < right.tag
}

func (p *poolOutbound) handleHealthResult(event monitor.HealthResultEvent) {
	if p.options.GroupID == 0 || !p.hasMember(event.Tag) {
		return
	}
	if event.Success {
		group.RecordHealthSuccess(p.options.GroupID, event.Tag)
	} else {
		group.RecordGroupHealthFailure(p.options.GroupID, event.Tag, errors.New(event.Error), event.CheckedAt)
	}
	p.reconcileCurrent()
}

func (p *poolOutbound) hasMember(tag string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, member := range p.members {
		if member.tag == tag {
			return true
		}
	}
	return false
}

func (p *poolOutbound) reconcileCurrent() {
	candidates := p.getCandidateBuffer()
	p.mu.Lock()
	candidates = p.availableMembersLocked(time.Now(), "", candidates)
	p.mu.Unlock()
	current := group.CurrentTag(p.options.GroupID)
	for _, candidate := range candidates {
		if candidate.tag == current {
			p.waitForInitialLatency.Store(false)
			p.putCandidateBuffer(candidates)
			return
		}
	}
	if p.waitForInitialLatency.Load() && !p.allInitialChecksDone() {
		p.putCandidateBuffer(candidates)
		return
	}
	p.waitForInitialLatency.Store(false)
	if len(candidates) == 0 {
		p.putCandidateBuffer(candidates)
		group.SetCurrentTag(p.options.GroupID, "")
		return
	}
	selected := p.selectMember(candidates)
	p.putCandidateBuffer(candidates)
	p.selectorMu.Lock()
	defer p.selectorMu.Unlock()
	if p.selector != nil && !p.selector.SelectOutbound(selected.tag) {
		p.logger.Warn("selector rejected hot replacement: ", selected.tag)
		return
	}
	group.SetCurrentTag(p.options.GroupID, selected.tag)
}

func (p *poolOutbound) allInitialChecksDone() bool {
	if p.monitor == nil {
		return true
	}
	for _, tag := range p.options.Members {
		snapshot := p.monitor.SnapshotForTag(tag)
		if snapshot == nil || !snapshot.InitialCheckDone {
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
		return
	}
	if member.shared == nil {
		p.logger.Warn("proxy ", member.tag, " failure (no shared state): ", cause)
		return
	}
	failures, blacklisted, _ := member.shared.recordFailure(cause, p.options.FailureThreshold, p.options.BlacklistDuration, destination)
	if blacklisted {
		p.logger.Warn("proxy ", member.tag, " blacklisted for ", p.options.BlacklistDuration, ": ", cause)
	} else {
		p.logger.Warn("proxy ", member.tag, " failure ", failures, "/", p.options.FailureThreshold, ": ", cause)
	}
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
	return &trackedConn{
		Conn: conn,
		release: func() {
			p.decActive(member)
		},
		onTraffic: func(upload, download int64) {
			if member.shared != nil {
				member.shared.addTraffic(upload, download)
			}
		},
		onError: func(err error) {
			p.recordEstablishedIOError(member, err, destination)
		},
	}
}

func (p *poolOutbound) wrapPacketConn(conn net.PacketConn, member *memberState, destination string) net.PacketConn {
	return &trackedPacketConn{
		PacketConn: conn,
		release: func() {
			p.decActive(member)
		},
		onTraffic: func(upload, download int64) {
			if member.shared != nil {
				member.shared.addTraffic(upload, download)
			}
		},
		onError: func(err error) {
			p.recordEstablishedIOError(member, err, destination)
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

// httpProbe performs an HTTP probe through the connection and measures TTFB.
// It sends a minimal HTTP request and waits for the first byte of response.
func httpProbe(conn net.Conn, host string) (time.Duration, error) {
	// Build HTTP request
	req := fmt.Sprintf("GET /generate_204 HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nUser-Agent: Mozilla/5.0\r\n\r\n", host)

	// Try to set write deadline (ignore errors for connections that don't support it)
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

	// Record time just before sending request
	start := time.Now()

	// Send HTTP request
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, fmt.Errorf("write request: %w", err)
	}

	// Try to set read deadline (ignore errors for connections that don't support it)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	// Read first byte (TTFB - Time To First Byte)
	reader := bufio.NewReader(conn)
	_, err := reader.ReadByte()
	if err != nil {
		return 0, fmt.Errorf("read response: %w", err)
	}

	// Calculate TTFB
	ttfb := time.Since(start)
	return ttfb, nil
}

func (p *poolOutbound) makeProbeFunc(member *memberState) func(ctx context.Context) (time.Duration, error) {
	if p.monitor == nil {
		return nil
	}
	// 仅在创建时检查是否有探测目标，实际目标在执行时动态获取
	if _, ok := p.monitor.DestinationForProbe(); !ok {
		return nil
	}
	return func(ctx context.Context) (time.Duration, error) {
		// 每次执行时动态获取最新的探测目标
		destination, ok := p.monitor.DestinationForProbe()
		if !ok {
			return 0, E.New("probe target not configured")
		}

		start := time.Now()
		conn, err := member.outbound.DialContext(ctx, N.NetworkTCP, destination)
		if err != nil {
			return 0, err
		}
		defer conn.Close()

		// Perform HTTP probe to measure actual latency (TTFB)
		_, err = httpProbe(conn, destination.AddrString())
		if err != nil {
			return 0, err
		}

		// Total duration = dial time + HTTP probe
		duration := time.Since(start)
		return duration, nil
	}
}

// makeProbeByTagFunc creates a probe function that works before member initialization
func (p *poolOutbound) makeProbeByTagFunc(tag string) func(ctx context.Context) (time.Duration, error) {
	if p.monitor == nil {
		return nil
	}
	// 仅在创建时检查是否有探测目标，实际目标在执行时动态获取
	if _, ok := p.monitor.DestinationForProbe(); !ok {
		return nil
	}
	return func(ctx context.Context) (time.Duration, error) {
		// 每次执行时动态获取最新的探测目标
		destination, ok := p.monitor.DestinationForProbe()
		if !ok {
			return 0, E.New("probe target not configured")
		}

		// Ensure members are initialized
		p.mu.Lock()
		if len(p.members) == 0 {
			if err := p.initializeMembersLocked(); err != nil {
				p.mu.Unlock()
				return 0, err
			}
		}

		// Find the member by tag
		var member *memberState
		for _, m := range p.members {
			if m.tag == tag {
				member = m
				break
			}
		}
		p.mu.Unlock()

		if member == nil {
			return 0, E.New("member not found: ", tag)
		}

		start := time.Now()
		conn, err := member.outbound.DialContext(ctx, N.NetworkTCP, destination)
		if err != nil {
			return 0, err
		}
		defer conn.Close()

		// Perform HTTP probe to measure actual latency (TTFB)
		_, err = httpProbe(conn, destination.AddrString())
		if err != nil {
			return 0, err
		}

		// Total duration = dial time + TTFB
		duration := time.Since(start)
		return duration, nil
	}
}

// makeReleaseByTagFunc creates a release function that works before member initialization
func (p *poolOutbound) makeReleaseByTagFunc(tag string) func() {
	return func() {
		releaseSharedMember(tag)
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
		destination := M.ParseSocksaddr(address)
		if !destination.IsValid() {
			return nil, E.New("invalid dial address: ", address)
		}
		nw := network
		if nw == "" {
			nw = N.NetworkTCP
		}
		return member.outbound.DialContext(ctx, nw, destination)
	}
}

// makeDialerByTagFunc returns a DialerFunc for a tag, resolving the member on
// first use (works before member initialization, like makeProbeByTagFunc).
func (p *poolOutbound) makeDialerByTagFunc(tag string) monitor.DialerFunc {
	if p.monitor == nil {
		return nil
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		// Ensure members are initialized
		p.mu.Lock()
		if len(p.members) == 0 {
			if err := p.initializeMembersLocked(); err != nil {
				p.mu.Unlock()
				return nil, err
			}
		}

		var member *memberState
		for _, m := range p.members {
			if m.tag == tag {
				member = m
				break
			}
		}
		p.mu.Unlock()

		if member == nil {
			return nil, E.New("member not found: ", tag)
		}

		destination := M.ParseSocksaddr(address)
		if !destination.IsValid() {
			return nil, E.New("invalid dial address: ", address)
		}
		nw := network
		if nw == "" {
			nw = N.NetworkTCP
		}
		return member.outbound.DialContext(ctx, nw, destination)
	}
}

type trackedConn struct {
	net.Conn
	once      sync.Once
	release   func()
	onTraffic func(upload, download int64)
	onError   func(error)
	errorOnce sync.Once
}

func (c *trackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.onTraffic != nil {
		c.onTraffic(0, int64(n))
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
	once      sync.Once
	release   func()
	onTraffic func(upload, download int64)
	onError   func(error)
	errorOnce sync.Once
}

func (c *trackedPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(b)
	if n > 0 && c.onTraffic != nil {
		c.onTraffic(0, int64(n))
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

func (p *poolOutbound) getCandidateBuffer() []*memberState {
	if buf := p.candidatesPool.Get(); buf != nil {
		return buf.([]*memberState)
	}
	return make([]*memberState, 0, len(p.options.Members))
}

func (p *poolOutbound) putCandidateBuffer(buf []*memberState) {
	if buf == nil {
		return
	}
	const maxCached = 4096
	if cap(buf) > maxCached {
		return
	}
	p.candidatesPool.Put(buf[:0])
}
