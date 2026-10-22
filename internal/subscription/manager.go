package subscription

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/config"
	"easy_proxies/internal/controlplane"
	"easy_proxies/internal/group"
	"easy_proxies/internal/runtimenodes"
	"easy_proxies/internal/store"

	"golang.org/x/sync/errgroup"
)

var ErrNotFound = errors.New("订阅不存在")

type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// staleNodeMissLimit is how many consecutive snapshots may omit a member before
// it is removed. Two tolerates one truncated provider response while still
// retiring nodes that were genuinely delisted.
const staleNodeMissLimit = 2

// fetchConcurrency bounds parallel downloads during one refresh pass. Commits
// stay serial because SQLite has a single writer connection.
const fetchConcurrency = 3

type Option func(*Manager)

func WithLogger(l Logger) Option           { return func(m *Manager) { m.logger = l } }
func WithStore(s store.Store) Option       { return func(m *Manager) { m.store = s } }
func WithHTTPClient(c *http.Client) Option { return func(m *Manager) { m.httpClient = c } }

// WithRefreshObserver registers a notification for every successfully committed
// snapshot, whoever triggered it (create, manual refresh, timer). The runtime
// uses it to re-derive auto tags for nodes that appeared or disappeared.
func WithRefreshObserver(notify func()) Option {
	return func(m *Manager) { m.refreshObserver = notify }
}

var _ boxmgr.ConfigUpdateListener = (*Manager)(nil)
var _ controlplane.SubscriptionRefresher = (*Manager)(nil)

type runtimeManager interface {
	ReloadWithPortMap(newCfg *config.Config, portMap map[string]uint16) error
	CurrentPortMap() map[string]uint16
}

type Manager struct {
	mu         sync.RWMutex
	baseCfg    *config.Config
	boxMgr     runtimeManager
	logger     Logger
	httpClient *http.Client
	store      store.Store
	status     controlplane.SubscriptionStatus
	ctx        context.Context
	cancel     context.CancelFunc
	refreshMu  sync.Mutex
	// resultMu serializes source edits/deletes with result validation and writes.
	// Never hold it across fetching, runtime reconciliation or observer callbacks.
	resultMu       sync.Mutex
	sourceVersions map[int64]uint64
	// refreshing tracks in-flight downloads by subscription ID. It is the only
	// source of IsRefreshing, so a single refresh reports itself the same way a
	// full pass does. Guarded by mu.
	refreshing      map[int64]struct{}
	refreshObserver func()
}

func New(cfg *config.Config, boxMgr runtimeManager, opts ...Option) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 100, MaxIdleConnsPerHost: 10,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second, ResponseHeaderTimeout: 10 * time.Second,
	}
	// No client-wide timeout: each request carries the subscription's own
	// deadline. A 60s client cap silently overrode any larger per-sub timeout.
	m := &Manager{baseCfg: cfg, boxMgr: boxMgr, ctx: ctx, cancel: cancel,
		refreshing: make(map[int64]struct{}), sourceVersions: make(map[int64]uint64),
		httpClient: &http.Client{Transport: transport}}
	for _, opt := range opts {
		opt(m)
	}
	if m.logger == nil {
		m.logger = defaultLogger{}
	}
	return m
}

func (m *Manager) Start() {
	if err := m.importConfiguredSubscriptions(m.ctx); err != nil {
		m.logger.Errorf("import configured subscriptions: %v", err)
	}
	go m.refreshLoop()
	go m.refreshInitialSnapshots()
}

// refreshInitialSnapshots populates memberships after migrating from the
// legacy URL list. Existing successful snapshots are left untouched.
func (m *Manager) refreshInitialSnapshots() {
	subs, err := m.List(m.ctx)
	if err != nil {
		m.logger.Errorf("list subscriptions for initial refresh: %v", err)
		return
	}
	needsRefresh := false
	for _, sub := range subs {
		if sub.Enabled && sub.LastSuccess.IsZero() {
			needsRefresh = true
			break
		}
	}
	if !needsRefresh || !m.refreshMu.TryLock() {
		return
	}
	defer m.refreshMu.Unlock()
	if err := m.refreshDue(m.ctx, true); err != nil {
		m.logger.Warnf("initial subscription refresh failed: %v", err)
	}
}

func (m *Manager) Stop() {
	m.cancel()
	if m.httpClient != nil {
		m.httpClient.CloseIdleConnections()
	}
}

func (m *Manager) defaults() (int, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	interval := int(m.baseCfg.SubscriptionRefresh.Interval.Seconds())
	timeout := int(m.baseCfg.SubscriptionRefresh.Timeout.Seconds())
	if interval <= 0 {
		interval = 3600
	}
	if timeout <= 0 {
		timeout = 30
	}
	return interval, timeout
}

func (m *Manager) importConfiguredSubscriptions(ctx context.Context) error {
	if m.store == nil {
		return errors.New("store not available")
	}
	m.mu.RLock()
	urls := append([]string(nil), m.baseCfg.Subscriptions...)
	m.mu.RUnlock()
	interval, timeout := m.defaults()
	for i, rawURL := range urls {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			continue
		}
		existing, err := m.store.GetSubscriptionByURL(ctx, rawURL)
		if err != nil {
			return err
		}
		if existing != nil {
			// Older v3 builds incorrectly tied subscription enabled state to the
			// global auto-refresh switch. Repair only records that have never
			// produced a snapshot; established user choices remain untouched.
			if !existing.Enabled && existing.LastSuccess.IsZero() && existing.NodeCount == 0 {
				if err := m.store.SetSubscriptionEnabled(ctx, existing.ID, true); err != nil {
					return err
				}
			}
			continue
		}
		sub := &store.Subscription{Name: fmt.Sprintf("订阅 %d", i+1), URL: rawURL, Enabled: true,
			RefreshIntervalSeconds: interval, RefreshTimeoutSeconds: timeout, SortOrder: i}
		if err := validateSubscription(sub); err != nil {
			return fmt.Errorf("导入订阅 %d: %w", i+1, err)
		}
		if err := m.store.CreateSubscription(ctx, sub); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) List(ctx context.Context) ([]store.Subscription, error) {
	if m.store == nil {
		return nil, errors.New("store not available")
	}
	return m.store.ListSubscriptions(ctx)
}

func (m *Manager) Get(ctx context.Context, id int64) (*store.Subscription, error) {
	if id <= 0 {
		return nil, ErrNotFound
	}
	sub, err := m.store.GetSubscription(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub == nil {
		return nil, ErrNotFound
	}
	return sub, nil
}

func (m *Manager) Create(ctx context.Context, sub store.Subscription) (*store.Subscription, error) {
	applyDefaults(&sub, m.defaults)
	if err := validateSubscription(&sub); err != nil {
		return nil, err
	}
	if err := m.store.CreateSubscription(ctx, &sub); err != nil {
		return nil, err
	}
	if err := m.reconcileRuntime(ctx); err != nil {
		return nil, err
	}
	// A freshly added subscription has no members until it is fetched. Trigger
	// that now rather than waiting up to a minute (or forever, when the global
	// auto-refresh switch is off).
	if sub.Enabled {
		m.refreshInBackground(sub.ID)
	}
	return &sub, nil
}

func (m *Manager) Update(ctx context.Context, id int64, input store.Subscription) (*store.Subscription, error) {
	if err := m.updateSettings(ctx, id, input); err != nil {
		return nil, err
	}
	if err := m.reconcileRuntime(ctx); err != nil {
		return nil, err
	}
	if input.Enabled {
		m.refreshInBackground(id)
	}
	return m.Get(ctx, id)
}

func (m *Manager) updateSettings(ctx context.Context, id int64, input store.Subscription) error {
	m.resultMu.Lock()
	defer m.resultMu.Unlock()
	current, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	settings := store.SubscriptionSettings{
		Name:                   input.Name,
		URL:                    input.URL,
		Format:                 input.Format,
		UserAgent:              input.UserAgent,
		Enabled:                input.Enabled,
		RefreshIntervalSeconds: input.RefreshIntervalSeconds,
		RefreshTimeoutSeconds:  input.RefreshTimeoutSeconds,
		SortOrder:              input.SortOrder,
	}
	if err := applySettingsDefaults(&settings, m.defaults); err != nil {
		return err
	}
	if err := validateSettings(&settings); err != nil {
		return err
	}
	urlChanged := strings.TrimSpace(current.URL) != settings.URL
	if err := m.store.UpdateSubscriptionSettings(ctx, id, settings); err != nil {
		return err
	}
	if urlChanged || current.Format != settings.Format || current.UserAgent != settings.UserAgent {
		// A -> B -> A must also invalidate the original in-flight request.
		m.sourceVersions[id]++
	}
	if urlChanged {
		// Pointing the subscription at another provider makes every existing
		// member an artifact of the old upstream. Clear them instead of merging
		// two unrelated node sets together.
		if err := m.store.ResetSubscriptionNodes(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// refreshInBackground queues one subscription behind any in-flight refresh. The
// caller returns immediately; the fetch uses the manager context so shutdown
// cancels it.
func (m *Manager) refreshInBackground(id int64) {
	go func() {
		m.refreshMu.Lock()
		defer m.refreshMu.Unlock()
		if err := m.refreshOneLocked(m.ctx, id, false); err != nil {
			m.logger.Warnf("background refresh subscription %d failed: %v", id, err)
		}
	}()
}

func (m *Manager) Delete(ctx context.Context, id int64) error {
	if _, err := m.Get(ctx, id); err != nil {
		return err
	}
	m.resultMu.Lock()
	err := m.store.DeleteSubscription(ctx, id)
	if err == nil {
		delete(m.sourceVersions, id)
	}
	m.resultMu.Unlock()
	if err != nil {
		return err
	}
	return m.reconcileRuntime(ctx)
}

func (m *Manager) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	sub, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := m.store.SetSubscriptionEnabled(ctx, id, enabled); err != nil {
		return err
	}
	if err := m.reconcileRuntime(ctx); err != nil {
		return err
	}
	// Re-enabling a subscription that never produced a snapshot should show
	// nodes without waiting for the timer.
	if enabled && sub.LastSuccess.IsZero() {
		m.refreshInBackground(id)
	}
	return nil
}

func (m *Manager) ActivateExclusive(ctx context.Context, id int64) error {
	if _, err := m.Get(ctx, id); err != nil {
		return err
	}
	if err := m.store.ActivateSubscriptionExclusive(ctx, id); err != nil {
		return err
	}
	return m.reconcileRuntime(ctx)
}

func (m *Manager) Nodes(ctx context.Context, id int64) ([]store.SubscriptionNode, error) {
	if _, err := m.Get(ctx, id); err != nil {
		return nil, err
	}
	return m.store.ListSubscriptionNodes(ctx, id)
}

func applyDefaults(sub *store.Subscription, defaults func() (int, int)) {
	interval, timeout := defaults()
	sub.Name, sub.URL = strings.TrimSpace(sub.Name), strings.TrimSpace(sub.URL)
	sub.UserAgent = strings.TrimSpace(sub.UserAgent)
	if format, err := config.NormalizeSubscriptionFormat(sub.Format); err == nil {
		sub.Format = format
	}
	if sub.RefreshIntervalSeconds <= 0 {
		sub.RefreshIntervalSeconds = interval
	}
	if sub.RefreshTimeoutSeconds <= 0 {
		sub.RefreshTimeoutSeconds = timeout
	}
}

func applySettingsDefaults(settings *store.SubscriptionSettings, defaults func() (int, int)) error {
	interval, timeout := defaults()
	settings.Name, settings.URL = strings.TrimSpace(settings.Name), strings.TrimSpace(settings.URL)
	settings.UserAgent = strings.TrimSpace(settings.UserAgent)
	if format, err := config.NormalizeSubscriptionFormat(settings.Format); err == nil {
		settings.Format = format
	}
	if settings.RefreshIntervalSeconds <= 0 {
		settings.RefreshIntervalSeconds = interval
	}
	if settings.RefreshTimeoutSeconds <= 0 {
		settings.RefreshTimeoutSeconds = timeout
	}
	return nil
}

func validateSubscription(sub *store.Subscription) error {
	settings := store.SubscriptionSettings{
		Name: sub.Name, URL: sub.URL, Format: sub.Format, UserAgent: sub.UserAgent,
		Enabled: sub.Enabled, RefreshIntervalSeconds: sub.RefreshIntervalSeconds,
		RefreshTimeoutSeconds: sub.RefreshTimeoutSeconds, SortOrder: sub.SortOrder,
	}
	if err := validateSettings(&settings); err != nil {
		return err
	}
	sub.Format = settings.Format
	return nil
}

func validateSettings(settings *store.SubscriptionSettings) error {
	if strings.TrimSpace(settings.Name) == "" {
		return errors.New("订阅名称不能为空")
	}
	u, err := url.ParseRequestURI(strings.TrimSpace(settings.URL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("订阅 URL 必须是有效的 http/https URL")
	}
	if settings.RefreshIntervalSeconds <= 0 {
		return errors.New("刷新间隔必须大于 0")
	}
	if settings.RefreshTimeoutSeconds <= 0 {
		return errors.New("刷新超时必须大于 0")
	}
	format, err := config.NormalizeSubscriptionFormat(settings.Format)
	if err != nil {
		return err
	}
	settings.Format = format
	if len(settings.UserAgent) > 512 || strings.ContainsAny(settings.UserAgent, "\r\n") {
		return errors.New("订阅 User-Agent 无效")
	}
	return nil
}

func (m *Manager) RefreshOne(ctx context.Context, id int64) error {
	return m.refreshOne(ctx, id, false)
}

func (m *Manager) RefreshForHealing(ctx context.Context, id int64) error {
	return m.refreshOne(ctx, id, true)
}

func (m *Manager) refreshOne(ctx context.Context, id int64, healing bool) error {
	if !m.refreshMu.TryLock() {
		if healing {
			return &group.DeferredHealing{Reason: "subscription refresh already running", Until: time.Now().Add(time.Minute)}
		}
		return controlplane.ErrRefreshInProgress
	}
	defer m.refreshMu.Unlock()
	return m.refreshOneLocked(ctx, id, healing)
}

// refreshOneLocked runs one subscription refresh; the caller holds refreshMu.
func (m *Manager) refreshOneLocked(ctx context.Context, id int64, healing bool) error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	m.resetRunCounters()
	sub, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if healing {
		now := time.Now()
		if !sub.Enabled {
			return &group.DeferredHealing{Reason: "subscription disabled", Until: now.Add(5 * time.Minute)}
		}
		if next := sub.LastAttempt.Add(5 * time.Minute); now.Before(next) {
			return &group.DeferredHealing{Reason: "subscription refresh cooldown", Until: next}
		}
		// Record before network I/O so process shutdown cannot erase the cooldown.
		if err := m.store.MarkSubscriptionAttempt(ctx, id, now); err != nil {
			return err
		}
	}
	m.beginRefreshing(id)
	defer m.endRefreshing(id)
	err = m.refreshSubscription(ctx, sub)
	if err == nil {
		err = m.reconcileRuntime(ctx)
	}
	m.recordRunResult(err)
	return err
}

func (m *Manager) RefreshNow() error {
	if !m.refreshMu.TryLock() {
		return controlplane.ErrRefreshInProgress
	}
	defer m.refreshMu.Unlock()
	return m.refreshDue(m.ctx, false)
}

func (m *Manager) refreshDue(ctx context.Context, dueOnly bool) error {
	subs, err := m.List(ctx)
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		return errors.New("没有配置订阅链接")
	}
	now := time.Now().UTC()
	m.resetRunCounters()
	var pending []*store.Subscription
	for i := range subs {
		sub := &subs[i]
		if !sub.Enabled {
			continue
		}
		if dueOnly && !sub.LastAttempt.IsZero() && now.Before(sub.LastAttempt.Add(time.Duration(sub.RefreshIntervalSeconds)*time.Second)) {
			continue
		}
		pending = append(pending, sub)
	}

	// Fetch in parallel — one slow provider should not hold up the rest — but
	// commit strictly in order afterwards. Commits share the single SQLite
	// writer and the run counters, and reconcile must see the final state.
	fetched := make([]fetchedSubscription, len(pending))
	groupFetch := new(errgroup.Group)
	groupFetch.SetLimit(fetchConcurrency)
	for i := range pending {
		i := i
		groupFetch.Go(func() error {
			sub := pending[i]
			m.beginRefreshing(sub.ID)
			fetched[i] = m.fetchFor(ctx, sub)
			m.endRefreshing(sub.ID)
			return nil
		})
	}
	_ = groupFetch.Wait()

	var errs []error
	changed := false
	for i, sub := range pending {
		removed, commitErr := m.commitFetched(ctx, sub, fetched[i])
		if commitErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sub.Name, commitErr))
			m.logger.Warnf("refresh subscription %d failed: %v", sub.ID, commitErr)
			continue
		}
		changed = changed || removed
	}
	if changed {
		if err := m.reconcileRuntime(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	joined := errors.Join(errs...)
	m.recordRunResult(joined)
	return joined
}

func (m *Manager) resetRunCounters() {
	m.mu.Lock()
	m.status.Parsed, m.status.Created, m.status.Updated = 0, 0, 0
	m.status.DuplicatesSkipped, m.status.Invalid = 0, 0
	m.mu.Unlock()
}

// recordRunResult publishes the outcome of a refresh run, whether it covered one
// subscription or all of them.
func (m *Manager) recordRunResult(err error) {
	m.mu.Lock()
	m.status.LastRefresh = time.Now().UTC()
	m.status.RefreshCount++
	if err != nil {
		m.status.LastError = err.Error()
	} else {
		m.status.LastError = ""
	}
	m.mu.Unlock()
}

func (m *Manager) beginRefreshing(id int64) {
	m.mu.Lock()
	m.refreshing[id] = struct{}{}
	m.status.IsRefreshing = true
	m.mu.Unlock()
}

func (m *Manager) endRefreshing(id int64) {
	m.mu.Lock()
	delete(m.refreshing, id)
	m.status.IsRefreshing = len(m.refreshing) > 0
	m.mu.Unlock()
}

// fetchedSubscription is one completed download, before anything is written.
// Splitting fetch from commit lets a refresh pass overlap the network work while
// keeping every database mutation serial.
type fetchedSubscription struct {
	sourceVersion uint64
	nodes         []config.NodeConfig
	parseIssues   []string
	etag          string
	lastModified  string
	notModified   bool
	err           error
}

func (m *Manager) fetchFor(ctx context.Context, sub *store.Subscription) fetchedSubscription {
	// RefreshDue may have listed this row before an edit. Read the request
	// settings and its version together immediately before starting the fetch.
	m.resultMu.Lock()
	current, err := m.Get(ctx, sub.ID)
	version := m.sourceVersions[sub.ID]
	if err == nil {
		*sub = *current
	}
	m.resultMu.Unlock()
	if err != nil {
		return fetchedSubscription{sourceVersion: version, err: err}
	}
	timeout := time.Duration(sub.RefreshTimeoutSeconds) * time.Second
	if timeout <= 0 {
		_, seconds := m.defaults()
		timeout = time.Duration(seconds) * time.Second
	}
	nodes, parseIssues, etag, lastModified, notModified, err := m.fetchSubscription(ctx, sub, timeout)
	return fetchedSubscription{sourceVersion: version, nodes: nodes, parseIssues: parseIssues, etag: etag,
		lastModified: lastModified, notModified: notModified, err: err}
}

// commitFetched persists one fetch result. It returns whether a successful
// snapshot changed the node set (which requires a runtime reconcile).
func (m *Manager) commitFetched(ctx context.Context, sub *store.Subscription, fetched fetchedSubscription) (bool, error) {
	m.resultMu.Lock()
	changed, err := m.commitFetchedLocked(ctx, sub, fetched)
	m.resultMu.Unlock()
	if err == nil && !fetched.notModified && m.refreshObserver != nil {
		m.refreshObserver()
	}
	return changed, err
}

func (m *Manager) commitFetchedLocked(ctx context.Context, sub *store.Subscription, fetched fetchedSubscription) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := m.Get(ctx, sub.ID); err != nil {
		return false, err
	}
	if fetched.sourceVersion != m.sourceVersions[sub.ID] {
		return false, errors.New("subscription source changed during refresh; result discarded")
	}
	attempt := time.Now().UTC()
	// Rows the parser dropped are not fatal, but they silently shrink the node
	// list; log them so the operator can see the gap.
	for _, issue := range fetched.parseIssues {
		m.logger.Warnf("subscription %d parse issue: %s", sub.ID, issue)
	}
	if fetched.err == nil && !fetched.notModified && len(fetched.nodes) == 0 {
		// An empty response is a provider failure, not a signal to delete the
		// current members. Report it and leave the stored snapshot alone.
		fetched.err = errors.New("订阅未返回有效节点")
	}
	if fetched.err != nil {
		m.mu.Lock()
		m.status.Invalid++
		m.mu.Unlock()
		if err := m.store.RecordSubscriptionFailure(ctx, sub.ID, attempt, fetched.err.Error()); err != nil {
			return false, errors.Join(fetched.err, err)
		}
		return false, fetched.err
	}
	if fetched.notModified {
		// Keep the stored validators and snapshot; only the attempt moves.
		if err := m.store.RecordSubscriptionNotModified(ctx, sub.ID, attempt); err != nil {
			return false, fmt.Errorf("record 304 for subscription %d: %w", sub.ID, err)
		}
		return false, nil
	}
	inputs := make([]store.SubscriptionNodeInput, 0, len(fetched.nodes))
	seen := make(map[string]struct{}, len(fetched.nodes))
	for i, n := range fetched.nodes {
		uri := strings.TrimSpace(n.URI)
		identityKey := n.IdentityHash
		if identityKey == "" {
			identityKey = uri
		}
		if _, ok := seen[identityKey]; ok {
			continue
		}
		seen[identityKey] = struct{}{}
		logicalName := strings.TrimSpace(n.Name)
		name := nodeName(logicalName, uri, i+1)
		inputs = append(inputs, store.SubscriptionNodeInput{URI: uri, Name: name, Port: n.Port,
			Username: n.Username, Password: n.Password, Enabled: true,
			IdentityHash: n.IdentityHash, CanonicalJSON: n.CanonicalJSON, LogicalName: logicalName})
	}
	m.mu.Lock()
	m.status.Parsed += len(fetched.nodes)
	m.status.DuplicatesSkipped += len(fetched.nodes) - len(inputs)
	m.mu.Unlock()
	commit, err := m.store.CommitSnapshot(ctx, sub.ID, inputs, store.SubscriptionSnapshot{
		Attempt: attempt, Success: time.Now().UTC(), ETag: fetched.etag,
		LastModified: fetched.lastModified, RemoveAfterMisses: staleNodeMissLimit,
	})
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	m.status.Created += commit.Created
	m.status.Updated += commit.Updated
	m.mu.Unlock()
	return commit.Created > 0 || commit.Updated > 0 || commit.Removed > 0, nil
}

func (m *Manager) refreshSubscription(ctx context.Context, sub *store.Subscription) error {
	_, err := m.commitFetched(ctx, sub, m.fetchFor(ctx, sub))
	return err
}

func (m *Manager) fetchSubscription(parent context.Context, sub *store.Subscription, timeout time.Duration) ([]config.NodeConfig, []string, string, string, bool, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sub.URL, nil)
	if err != nil {
		return nil, nil, "", "", false, fmt.Errorf("create request: %w", err)
	}
	format, err := config.NormalizeSubscriptionFormat(sub.Format)
	if err != nil {
		return nil, nil, "", "", false, err
	}
	userAgent := strings.TrimSpace(sub.UserAgent)
	if userAgent == "" {
		userAgent = defaultSubscriptionUserAgent(format)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", subscriptionAccept(format))
	// Conditional request: the stored validators were saved but never sent, so
	// every refresh re-downloaded and re-parsed the full body even when the
	// upstream content was unchanged. A 304 short-circuits the whole chain.
	if sub.ETag != "" {
		req.Header.Set("If-None-Match", sub.ETag)
	}
	if sub.LastModified != "" {
		req.Header.Set("If-Modified-Since", sub.LastModified)
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, nil, "", "", false, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, nil, "", "", true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, "", "", false, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024+1))
	if err != nil {
		return nil, nil, "", "", false, fmt.Errorf("read body: %w", err)
	}
	if len(body) > 10*1024*1024 {
		return nil, nil, "", "", false, errors.New("subscription response exceeds 10MB")
	}
	nodes, parseIssues, err := config.ParseSubscriptionContentAsReport(string(body), format)
	if err != nil {
		contentType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
		if contentType != "" {
			err = fmt.Errorf("解析订阅响应（Content-Type %s）: %w", contentType, err)
		}
	}
	// Fall back per validator. Using the stored value only when *both* headers
	// were missing erased a previously stored Last-Modified as soon as the
	// upstream started sending an ETag alone.
	etag, lastModified := resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	if etag == "" {
		etag = sub.ETag
	}
	if lastModified == "" {
		lastModified = sub.LastModified
	}
	return nodes, parseIssues, etag, lastModified, false, err
}

func defaultSubscriptionUserAgent(format string) string {
	switch format {
	case config.SubscriptionFormatBase64:
		return "v2rayN"
	case config.SubscriptionFormatSingBox:
		return "sing-box"
	default:
		// Auto mode prefers the broadly supported Mihomo representation; its
		// response is still parsed structurally and can fall back to Base64/URIs.
		return "mihomo"
	}
}

func subscriptionAccept(format string) string {
	switch format {
	case config.SubscriptionFormatClash:
		return "application/yaml, text/yaml, text/plain;q=0.9, */*;q=0.8"
	case config.SubscriptionFormatSingBox:
		return "application/json, text/plain;q=0.9, */*;q=0.8"
	default:
		return "text/plain, application/yaml, application/json, */*;q=0.8"
	}
}

func (m *Manager) reconcileRuntime(ctx context.Context) error {
	if m.store == nil || m.boxMgr == nil {
		return nil
	}
	all, err := runtimenodes.Load(ctx, m.store)
	if err != nil {
		return err
	}
	m.mu.RLock()
	newCfg := m.baseCfg.Clone()
	m.mu.RUnlock()
	newCfg.Nodes = all
	// Group definitions and their explicit/excluded node relationships are
	// persisted independently from config.yaml. Always hydrate them from the
	// store immediately before reload so a stale subscription config snapshot
	// can never make BoxManager stop or roll back an existing group runtime.
	groups, err := m.store.ListGroupPools(ctx)
	if err != nil {
		return fmt.Errorf("list groups for runtime reconciliation: %w", err)
	}
	newCfg.Groups = boxmgr.GroupConfigsFromStore(groups)
	if err := m.boxMgr.ReloadWithPortMap(newCfg, m.boxMgr.CurrentPortMap()); err != nil {
		return fmt.Errorf("reload runtime: %w", err)
	}
	m.mu.Lock()
	m.status.NodeCount = len(all)
	m.mu.Unlock()
	return nil
}

func (m *Manager) refreshLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.mu.RLock()
			enabled := m.baseCfg.SubscriptionRefresh.Enabled
			m.mu.RUnlock()
			if enabled && m.refreshMu.TryLock() {
				_ = m.refreshDue(m.ctx, true)
				m.refreshMu.Unlock()
			}
		}
	}
}

func (m *Manager) Status() controlplane.SubscriptionStatus {
	m.mu.RLock()
	status := m.status
	globalEnabled := m.baseCfg.SubscriptionRefresh.Enabled
	status.IsRefreshing = len(m.refreshing) > 0
	if len(m.refreshing) > 0 {
		ids := make([]int64, 0, len(m.refreshing))
		for id := range m.refreshing {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		status.RefreshingIDs = ids
	}
	m.mu.RUnlock()
	if subs, err := m.List(m.ctx); err == nil {
		status.HasSubscriptions = len(subs) > 0
		status.Enabled = globalEnabled
		var next time.Time
		for _, sub := range subs {
			if !sub.Enabled {
				continue
			}
			due := sub.LastAttempt.Add(time.Duration(sub.RefreshIntervalSeconds) * time.Second)
			if sub.LastAttempt.IsZero() {
				due = time.Now()
			}
			if next.IsZero() || due.Before(next) {
				next = due
			}
		}
		status.NextRefresh = next
	}
	return status
}

func (m *Manager) OnConfigUpdate(cfg *config.Config) {
	m.ApplyConfig(cfg)
}

// ApplyConfig publishes subscription defaults and enabled state immediately.
func (m *Manager) ApplyConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	m.mu.Lock()
	m.baseCfg = cfg
	m.mu.Unlock()
}

func nodeName(name, uri string, index int) string {
	name = strings.TrimSpace(name)
	if name == "" {
		if parsed, err := url.Parse(uri); err == nil && parsed.Fragment != "" {
			name, _ = url.QueryUnescape(parsed.Fragment)
		}
	}
	if name == "" {
		name = fmt.Sprintf("node-%d", index)
	}
	return name
}

type defaultLogger struct{}

func (defaultLogger) Infof(format string, args ...any) { log.Printf("[subscription] "+format, args...) }
func (defaultLogger) Warnf(format string, args ...any) {
	log.Printf("[subscription] WARN: "+format, args...)
}
func (defaultLogger) Errorf(format string, args ...any) {
	log.Printf("[subscription] ERROR: "+format, args...)
}
