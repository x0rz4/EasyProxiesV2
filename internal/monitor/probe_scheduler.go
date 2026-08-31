package monitor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type probeBatchRequest struct {
	ctx      context.Context
	kind     ProbeRoundKind
	dueOnly  bool
	onStart  func(int)
	onResult func(ProbeBatchResult)
	response chan probeBatchResponse
}

type probeBatchResponse struct {
	summary ProbeBatchSummary
	err     error
}

type enqueueInitialCommand struct {
	tags []string
	done chan struct{}
}
type routineProbeCommand struct{ dueOnly bool }
type runBatchCommand struct{ request *probeBatchRequest }
type wakeProbeCommand struct{}

type probeExecutionEvent struct {
	roundID uint64
	result  ProbeBatchResult
	done    bool
}

var errStaleProbeGeneration = errors.New("probe runtime generation changed")

type activeProbeRound struct {
	id          uint64
	request     *probeBatchRequest
	ctx         context.Context
	cancel      context.CancelFunc
	items       map[string]probeWorkItem
	retry       []probeWorkItem
	summary     ProbeBatchSummary
	policy      ProbePolicy
	startup     bool
	convergence bool
	attempt     int
}

// probeScheduler is a single-owner actor for all startup, periodic and manual
// scheduling decisions. Network attempts run in a bounded executor and report
// immutable results back to the actor; no scheduling lock crosses a dial.
type probeScheduler struct {
	manager  *Manager
	commands chan any
	events   chan probeExecutionEvent

	leaseMu      sync.Mutex
	tagsInFlight map[string]struct{}

	statusMu        sync.RWMutex
	round           ProbeRoundStatus
	initialRunning  bool
	initialQueued   int
	periodicDirty   atomic.Bool
	attemptMu       sync.Mutex
	runningAttempts map[probeAttemptKey]*probeAttemptState
	detachedProbes  atomic.Int64
}

type probeAttemptKey struct {
	entry *entry
	tag   string
}

type probeAttemptState struct{ detached bool }

type probeCallResult struct {
	latency time.Duration
	err     error
}

func newProbeScheduler(manager *Manager) *probeScheduler {
	return &probeScheduler{
		manager:         manager,
		commands:        make(chan any, 128),
		events:          make(chan probeExecutionEvent, 256),
		tagsInFlight:    make(map[string]struct{}),
		runningAttempts: make(map[probeAttemptKey]*probeAttemptState),
	}
}

func (s *probeScheduler) signal() {
	select {
	case s.commands <- wakeProbeCommand{}:
	default:
	}
}

func (s *probeScheduler) enqueueInitial(tags []string) {
	if len(tags) == 0 {
		return
	}
	copyTags := append([]string(nil), tags...)
	done := make(chan struct{})
	select {
	case s.commands <- enqueueInitialCommand{tags: copyTags, done: done}:
	case <-s.manager.ctx.Done():
		return
	}
	select {
	case <-done:
	case <-s.manager.ctx.Done():
	}
}

func (s *probeScheduler) requestRoutine(dueOnly bool) {
	if _, ready := s.manager.TargetForProbe(); !ready {
		return
	}
	select {
	case s.commands <- routineProbeCommand{dueOnly: dueOnly}:
	case <-s.manager.ctx.Done():
	}
}

func (s *probeScheduler) reschedulePeriodic() {
	s.periodicDirty.Store(true)
	s.signal()
}

func (s *probeScheduler) runBatch(ctx context.Context, kind ProbeRoundKind, dueOnly bool,
	onStart func(total int), onResult func(ProbeBatchResult)) (ProbeBatchSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ready := s.manager.TargetForProbe(); !ready {
		return ProbeBatchSummary{}, errors.New("probe target not configured")
	}
	request := &probeBatchRequest{
		ctx: ctx, kind: kind, dueOnly: dueOnly, onStart: onStart, onResult: onResult,
		response: make(chan probeBatchResponse, 1),
	}
	select {
	case s.commands <- runBatchCommand{request: request}:
	case <-ctx.Done():
		return ProbeBatchSummary{}, ctx.Err()
	case <-s.manager.ctx.Done():
		return ProbeBatchSummary{}, s.manager.ctx.Err()
	}
	select {
	case response := <-request.response:
		return response.summary, response.err
	case <-s.manager.ctx.Done():
		return ProbeBatchSummary{}, s.manager.ctx.Err()
	}
}

func (s *probeScheduler) run() {
	initialQueue := make(map[string]struct{})
	manualQueue := make([]*probeBatchRequest, 0)
	routinePending := false
	routineDueOnly := false
	var active *activeProbeRound
	var activeC <-chan struct{}
	var retryTimer *time.Timer
	var retryC <-chan time.Time
	var periodicTimer *time.Timer
	var periodicC <-chan time.Time
	var nextRoundID uint64

	resetPeriodicTimer := func() {
		if periodicTimer != nil {
			if !periodicTimer.Stop() {
				select {
				case <-periodicTimer.C:
				default:
				}
			}
			periodicTimer = nil
			periodicC = nil
		}
		delay, scheduled := s.manager.nextProbeDelay(time.Now())
		if !scheduled {
			return
		}
		// A zero-delay rescan after an empty/busy round would spin. Lease release
		// and registry changes also reschedule, so a tiny floor is sufficient.
		if delay < 10*time.Millisecond {
			delay = 10 * time.Millisecond
		}
		periodicTimer = time.NewTimer(delay)
		periodicC = periodicTimer.C
	}

	updateInitialStatus := func() {
		s.statusMu.Lock()
		s.initialQueued = len(initialQueue)
		s.statusMu.Unlock()
	}

	startNext := func() {
		if active != nil || retryC != nil {
			return
		}
		for len(manualQueue) > 0 && manualQueue[0].ctx.Err() != nil {
			request := manualQueue[0]
			manualQueue = manualQueue[1:]
			request.response <- probeBatchResponse{err: request.ctx.Err()}
		}

		var request *probeBatchRequest
		convergence := false
		if len(manualQueue) > 0 {
			request = manualQueue[0]
			manualQueue = manualQueue[1:]
		} else if s.isInitialRunning() {
			items, pending, waiting := s.collectInitialProbeItems(initialQueue)
			updateInitialStatus()
			if len(items) == 0 {
				if pending == 0 {
					s.finishInitialConvergence(initialQueue)
					updateInitialStatus()
				} else if waiting {
					return
				}
			} else {
				request = &probeBatchRequest{ctx: s.manager.ctx, kind: ProbeRoundStartup}
				convergence = true
				nextRoundID++
				active = s.newActiveRound(nextRoundID, request, items, convergence)
				activeC = active.ctx.Done()
				s.startActiveRound(active)
				return
			}
		} else if routinePending {
			request = &probeBatchRequest{ctx: s.manager.ctx, kind: ProbeRoundPeriodic, dueOnly: routineDueOnly}
			routinePending = false
			routineDueOnly = false
		}
		if request == nil {
			return
		}
		items := s.collectBatchProbeItems(request.dueOnly)
		nextRoundID++
		active = s.newActiveRound(nextRoundID, request, items, convergence)
		activeC = active.ctx.Done()
		s.startActiveRound(active)
	}

	for {
		if s.periodicDirty.Swap(false) {
			resetPeriodicTimer()
		}
		startNext()
		select {
		case <-s.manager.ctx.Done():
			if retryTimer != nil {
				retryTimer.Stop()
			}
			if periodicTimer != nil {
				periodicTimer.Stop()
			}
			if active != nil {
				active.cancel()
				for tag := range active.items {
					s.releaseTagProbe(tag, false)
				}
			}
			for _, request := range manualQueue {
				request.response <- probeBatchResponse{err: s.manager.ctx.Err()}
			}
			return
		case command := <-s.commands:
			switch value := command.(type) {
			case enqueueInitialCommand:
				started := false
				added := 0
				if !s.isInitialRunning() {
					s.setInitialRunning(true)
					started = true
				}
				for _, tag := range value.tags {
					if tag == "" {
						continue
					}
					if _, exists := initialQueue[tag]; !exists {
						initialQueue[tag] = struct{}{}
						added++
					}
				}
				updateInitialStatus()
				if started && s.manager.logger != nil {
					s.manager.logger.Info("initial health convergence started with ", len(initialQueue), " queued nodes")
				} else if added > 0 && s.manager.logger != nil {
					s.manager.logger.Info("initial health convergence queued ", added, " additional nodes; ", len(initialQueue), " waiting")
				}
				close(value.done)
			case routineProbeCommand:
				if !routinePending {
					routineDueOnly = value.dueOnly
				} else if !value.dueOnly {
					routineDueOnly = false
				}
				routinePending = true
			case runBatchCommand:
				if active != nil || retryC != nil || len(manualQueue) > 0 {
					value.request.response <- probeBatchResponse{err: ErrProbeRoundInProgress}
				} else {
					manualQueue = append(manualQueue, value.request)
				}
			case wakeProbeCommand:
			}
		case event := <-s.events:
			if active == nil || event.roundID != active.id {
				continue
			}
			if event.done {
				if active.startup && active.attempt == 1 && len(active.retry) > 0 {
					if active.ctx.Err() == nil {
						retryTimer = time.NewTimer(initialProbeRetryDelay)
						retryC = retryTimer.C
						continue
					}
					for _, item := range active.retry {
						s.releaseTagProbe(item.tag, false)
						delete(active.items, item.tag)
					}
					active.retry = nil
				}
				s.finishActiveRound(active, initialQueue)
				updateInitialStatus()
				active = nil
				activeC = nil
				resetPeriodicTimer()
				continue
			}
			s.handleExecutionResult(active, event.result)
		case <-retryC:
			retryC = nil
			retryTimer = nil
			if active == nil {
				continue
			}
			if active.ctx.Err() != nil {
				for _, item := range active.retry {
					s.releaseTagProbe(item.tag, false)
					delete(active.items, item.tag)
				}
				active.retry = nil
				s.finishActiveRound(active, initialQueue)
				updateInitialStatus()
				active = nil
				activeC = nil
				resetPeriodicTimer()
				continue
			}
			active.attempt = 2
			s.statusMu.Lock()
			s.round.Attempt = 2
			s.statusMu.Unlock()
			s.launchInitialWave(active, active.retry, 2, true)
		case <-activeC:
			if active == nil {
				activeC = nil
				continue
			}
			if retryC == nil {
				// The executor observes this context and will publish its terminal
				// events. Disable the closed channel until that completion arrives.
				activeC = nil
				continue
			}
			if retryTimer != nil {
				retryTimer.Stop()
			}
			retryTimer = nil
			retryC = nil
			for _, item := range active.retry {
				s.releaseTagProbe(item.tag, false)
				delete(active.items, item.tag)
			}
			active.retry = nil
			s.finishActiveRound(active, initialQueue)
			updateInitialStatus()
			active = nil
			activeC = nil
			resetPeriodicTimer()
		case <-periodicC:
			periodicTimer = nil
			periodicC = nil
			if !routinePending {
				routineDueOnly = true
			}
			routinePending = true
		}
	}
}

func (s *probeScheduler) newActiveRound(id uint64, request *probeBatchRequest, items []probeWorkItem, convergence bool) *activeProbeRound {
	ctx, cancel := context.WithCancel(request.ctx)
	stopManagerCancel := context.AfterFunc(s.manager.ctx, cancel)
	wrappedCancel := func() {
		stopManagerCancel()
		cancel()
	}
	byTag := make(map[string]probeWorkItem, len(items))
	for _, item := range items {
		byTag[item.tag] = item
	}
	return &activeProbeRound{
		id: id, request: request, ctx: ctx, cancel: wrappedCancel, items: byTag,
		summary: ProbeBatchSummary{Total: len(items)}, startup: request.kind == ProbeRoundStartup,
		convergence: convergence, attempt: 1, policy: s.manager.ProbePolicy(),
	}
}

func (s *probeScheduler) startActiveRound(active *activeProbeRound) {
	now := time.Now()
	status := ProbeRoundStatus{InFlight: true, Kind: active.request.kind, StartedAt: now, Total: active.summary.Total, Attempt: active.attempt, LastProgressAt: now,
		DetachedProbes: int(s.detachedProbes.Load())}
	s.statusMu.Lock()
	s.round = status
	s.statusMu.Unlock()
	if active.request.onStart != nil {
		active.request.onStart(active.summary.Total)
	}
	if active.summary.Total == 0 {
		s.events <- probeExecutionEvent{roundID: active.id, done: true}
		return
	}
	if s.manager.logger != nil {
		s.manager.logger.Info("starting ", active.request.kind, " health check for ", active.summary.Total, " nodes")
	}
	items := make([]probeWorkItem, 0, len(active.items))
	for _, item := range active.items {
		items = append(items, item)
	}
	if active.startup {
		s.launchInitialWave(active, items, 1, false)
		return
	}
	s.launchRoutineWave(active, items)
}

func (s *probeScheduler) launchInitialWave(active *activeProbeRound, items []probeWorkItem, attempt int, commitFailure bool) {
	limit := effectiveProbeConcurrency(active.policy.Concurrency, len(items))
	go func(roundID uint64) {
		results := collectLimited(limit, items, func(item probeWorkItem) ProbeBatchResult {
			return s.probeInitialAttempt(active.ctx, item, active.policy, attempt, commitFailure)
		})
		for result := range results {
			if !s.emit(probeExecutionEvent{roundID: roundID, result: result}) {
				return
			}
		}
		s.emit(probeExecutionEvent{roundID: roundID, done: true})
	}(active.id)
}

func (s *probeScheduler) launchRoutineWave(active *activeProbeRound, items []probeWorkItem) {
	limit := effectiveProbeConcurrency(active.policy.Concurrency, len(items))
	go func(roundID uint64) {
		results := collectLimited(limit, items, func(item probeWorkItem) ProbeBatchResult {
			return s.probeRoutineItem(active.ctx, item, active.policy)
		})
		for result := range results {
			if !s.emit(probeExecutionEvent{roundID: roundID, result: result}) {
				return
			}
		}
		s.emit(probeExecutionEvent{roundID: roundID, done: true})
	}(active.id)
}

func (s *probeScheduler) emit(event probeExecutionEvent) bool {
	select {
	case s.events <- event:
		return true
	case <-s.manager.ctx.Done():
		return false
	}
}

func (s *probeScheduler) handleExecutionResult(active *activeProbeRound, result ProbeBatchResult) {
	if active.startup && active.attempt == 1 && result.Err != nil && active.ctx.Err() == nil {
		if item, exists := active.items[result.Tag]; exists && item.probe != nil && entryRuntimeTag(item.entry) == item.tag {
			active.retry = append(active.retry, item)
			return
		}
	}
	s.releaseTagProbe(result.Tag, true)
	delete(active.items, result.Tag)
	if result.Err != nil {
		active.summary.Failed++
		if s.manager.logger != nil && !errors.Is(result.Err, context.Canceled) {
			s.manager.logger.Warn("probe failed for ", result.Tag, ": ", result.Err)
		}
	} else {
		active.summary.Success++
	}
	s.statusMu.Lock()
	s.round.Completed++
	s.round.LastProgressAt = time.Now()
	if result.Err != nil {
		s.round.Failed++
	} else {
		s.round.Success++
	}
	s.statusMu.Unlock()
	if active.request.onResult != nil {
		active.request.onResult(result)
	}
}

func (s *probeScheduler) updateWorkerStatus(delta int) {
	s.statusMu.Lock()
	s.round.ActiveWorkers += delta
	if s.round.ActiveWorkers < 0 {
		s.round.ActiveWorkers = 0
	}
	s.statusMu.Unlock()
}

// runProbeAttempt adds an outer deadline around outbound implementations that
// fail to honor context cancellation. The caller is released at the deadline;
// the detached call remains tracked and cannot be started again concurrently.
func (s *probeScheduler) runProbeAttempt(ctx context.Context, item probeWorkItem, timeout time.Duration) (time.Duration, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	key := probeAttemptKey{entry: item.entry, tag: item.tag}
	state := &probeAttemptState{}
	s.attemptMu.Lock()
	if _, running := s.runningAttempts[key]; running {
		s.attemptMu.Unlock()
		return 0, errors.New("previous probe call is still running")
	}
	s.runningAttempts[key] = state
	s.attemptMu.Unlock()
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	resultC := make(chan probeCallResult, 1)
	s.updateWorkerStatus(1)
	go func() {
		latency, err := item.probe(attemptCtx)
		resultC <- probeCallResult{latency: latency, err: err}
		s.attemptMu.Lock()
		if s.runningAttempts[key] == state {
			delete(s.runningAttempts, key)
			if state.detached {
				s.detachedProbes.Add(-1)
				s.statusMu.Lock()
				s.round.DetachedProbes = int(s.detachedProbes.Load())
				s.statusMu.Unlock()
			}
		}
		s.attemptMu.Unlock()
	}()
	select {
	case result := <-resultC:
		cancel()
		s.updateWorkerStatus(-1)
		return result.latency, result.err
	case <-attemptCtx.Done():
		cancel()
		s.attemptMu.Lock()
		if s.runningAttempts[key] == state {
			state.detached = true
			s.detachedProbes.Add(1)
			s.statusMu.Lock()
			s.round.DetachedProbes = int(s.detachedProbes.Load())
			if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
				s.round.HardTimeouts++
			}
			s.statusMu.Unlock()
		}
		s.attemptMu.Unlock()
		s.updateWorkerStatus(-1)
		if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			return 0, fmt.Errorf("probe hard timeout after %s: %w", timeout, context.DeadlineExceeded)
		}
		return 0, attemptCtx.Err()
	}
}

func (s *probeScheduler) finishActiveRound(active *activeProbeRound, initialQueue map[string]struct{}) {
	roundErr := active.ctx.Err()
	active.cancel()
	s.statusMu.Lock()
	s.round.InFlight = false
	s.statusMu.Unlock()
	if s.manager.logger != nil && active.summary.Total > 0 {
		s.manager.logger.Info(active.request.kind, " health check completed: ", active.summary.Success, " available, ", active.summary.Failed, " failed")
	}
	if active.request.response != nil {
		active.request.response <- probeBatchResponse{summary: active.summary, err: roundErr}
	}
	if active.convergence {
		s.refreshInitialQueue(initialQueue)
	}
}

func (s *probeScheduler) collectBatchProbeItems(dueOnly bool) []probeWorkItem {
	entries := s.manager.registry.entries()
	items := make([]probeWorkItem, 0, len(entries))
	now := time.Now()
	schedule := s.manager.probeScheduleSnapshot()
	for _, item := range entries {
		item.mu.RLock()
		work := probeWorkItem{entry: item, tag: item.info.Tag, name: item.info.Name, probe: item.probe}
		lastCheck := item.lastHealthCheck
		nodeID := item.info.NodeID
		item.mu.RUnlock()
		if work.probe == nil || (dueOnly && !lastCheck.IsZero() && now.Before(lastCheck.Add(schedule.intervalFor(work.tag, nodeID)))) {
			continue
		}
		items = append(items, work)
	}
	return s.reserveBatchProbeItems(items)
}

func (s *probeScheduler) collectInitialProbeItems(queue map[string]struct{}) ([]probeWorkItem, int, bool) {
	entries := s.manager.registry.entriesByTag()
	pending := make(map[string]probeWorkItem, len(entries))
	for tag, item := range entries {
		item.mu.RLock()
		if !item.initialCheckDone {
			pending[tag] = probeWorkItem{entry: item, tag: tag, name: item.info.Name, probe: item.probe}
		}
		item.mu.RUnlock()
	}
	for tag := range queue {
		if _, exists := pending[tag]; !exists {
			delete(queue, tag)
		}
	}
	for tag := range pending {
		queue[tag] = struct{}{}
	}
	items := make([]probeWorkItem, 0, len(queue))
	s.leaseMu.Lock()
	for tag := range queue {
		work, exists := pending[tag]
		if !exists {
			continue
		}
		if _, busy := s.tagsInFlight[tag]; busy {
			continue
		}
		s.tagsInFlight[tag] = struct{}{}
		items = append(items, work)
	}
	s.leaseMu.Unlock()
	for _, item := range items {
		delete(queue, item.tag)
	}
	return items, len(pending), len(items) == 0 && len(queue) > 0
}

func (s *probeScheduler) refreshInitialQueue(queue map[string]struct{}) {
	pending := s.pendingTags()
	if len(pending) == 0 {
		s.finishInitialConvergence(queue)
		return
	}
	for _, tag := range pending {
		queue[tag] = struct{}{}
	}
}

func (s *probeScheduler) finishInitialConvergence(queue map[string]struct{}) {
	pending := s.pendingTags()
	if len(pending) > 0 {
		for _, tag := range pending {
			queue[tag] = struct{}{}
		}
		return
	}
	if s.isInitialRunning() {
		s.setInitialRunning(false)
		clear(queue)
		if s.manager.logger != nil {
			s.manager.logger.Info("initial health convergence completed: pending nodes reached zero")
		}
	}
}

func (s *probeScheduler) pendingTags() []string {
	entries := s.manager.registry.entriesByTag()
	pending := make([]string, 0)
	for tag, item := range entries {
		item.mu.RLock()
		isPending := !item.initialCheckDone
		item.mu.RUnlock()
		if isPending {
			pending = append(pending, tag)
		}
	}
	return pending
}

func (s *probeScheduler) probeInitialAttempt(ctx context.Context, item probeWorkItem, policy ProbePolicy, attempt int, commitFailure bool) ProbeBatchResult {
	result := ProbeBatchResult{Tag: item.tag, Name: item.name, Attempts: attempt}
	if ctx.Err() != nil {
		result.Err = ctx.Err()
		return result
	}
	if entryRuntimeTag(item.entry) != item.tag {
		result.Err = errStaleProbeGeneration
		return result
	}
	if item.probe == nil {
		result.Err = errors.New("probe function not configured")
	} else {
		probeCtx := withProbePhaseTimeouts(ctx, policy.DialTimeout, policy.ResponseTimeout)
		result.Latency, result.Err = s.runProbeAttempt(probeCtx, item, policy.StartupTimeout)
	}
	if ctx.Err() == nil && entryRuntimeTag(item.entry) == item.tag && (result.Err == nil || commitFailure || item.probe == nil) {
		s.manager.applyHealthResult(item.entry, result.Latency, result.Err, time.Now())
	}
	return result
}

func (s *probeScheduler) probeRoutineItem(ctx context.Context, item probeWorkItem, policy ProbePolicy) ProbeBatchResult {
	result := ProbeBatchResult{Tag: item.tag, Name: item.name}
	if entryRuntimeTag(item.entry) != item.tag {
		result.Err = errStaleProbeGeneration
		return result
	}
	if item.probe == nil {
		result.Err = errors.New("probe function not configured")
		return result
	}
	nodeCtx, cancel := context.WithTimeout(ctx, policy.RoutineTimeout)
	defer cancel()
	nodeCtx = withProbePhaseTimeouts(nodeCtx, policy.DialTimeout, policy.ResponseTimeout)
	for attempt := 0; attempt <= policy.RoutineRetries; attempt++ {
		result.Attempts = attempt + 1
		result.Latency, result.Err = s.runProbeAttempt(nodeCtx, item, policy.RoutineTimeout)
		if result.Err == nil || nodeCtx.Err() != nil {
			break
		}
	}
	if ctx.Err() == nil && entryRuntimeTag(item.entry) == result.Tag {
		s.manager.applyHealthResult(item.entry, result.Latency, result.Err, time.Now())
	}
	return result
}

func (s *probeScheduler) reserveBatchProbeItems(items []probeWorkItem) []probeWorkItem {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	reserved := items[:0]
	for _, item := range items {
		if _, exists := s.tagsInFlight[item.tag]; exists {
			continue
		}
		s.tagsInFlight[item.tag] = struct{}{}
		reserved = append(reserved, item)
	}
	return reserved
}

func (s *probeScheduler) beginTagProbe(tag string) bool {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if _, exists := s.tagsInFlight[tag]; exists {
		return false
	}
	s.tagsInFlight[tag] = struct{}{}
	return true
}

func (s *probeScheduler) endTagProbe(tag string) {
	s.periodicDirty.Store(true)
	s.releaseTagProbe(tag, true)
}

func (s *probeScheduler) releaseTagProbe(tag string, wake bool) {
	s.leaseMu.Lock()
	delete(s.tagsInFlight, tag)
	s.leaseMu.Unlock()
	if wake {
		s.signal()
	}
}

func (s *probeScheduler) roundStatus() ProbeRoundStatus {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.round
}

func (s *probeScheduler) initialStatus(pending int) InitialProbeStatus {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return InitialProbeStatus{Converging: s.initialRunning, Pending: pending, Queued: s.initialQueued}
}

func (s *probeScheduler) isInitialRunning() bool {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.initialRunning
}

func (s *probeScheduler) setInitialRunning(running bool) {
	s.statusMu.Lock()
	s.initialRunning = running
	s.statusMu.Unlock()
}
