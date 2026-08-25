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
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

var ErrNotFound = errors.New("订阅不存在")

type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

type Option func(*Manager)

func WithLogger(l Logger) Option           { return func(m *Manager) { m.logger = l } }
func WithStore(s store.Store) Option       { return func(m *Manager) { m.store = s } }
func WithHTTPClient(c *http.Client) Option { return func(m *Manager) { m.httpClient = c } }

var _ boxmgr.ConfigUpdateListener = (*Manager)(nil)
var _ monitor.SubscriptionRefresher = (*Manager)(nil)

type Manager struct {
	mu         sync.RWMutex
	baseCfg    *config.Config
	boxMgr     *boxmgr.Manager
	logger     Logger
	httpClient *http.Client
	store      store.Store
	status     monitor.SubscriptionStatus
	ctx        context.Context
	cancel     context.CancelFunc
	refreshMu  sync.Mutex
}

func New(cfg *config.Config, boxMgr *boxmgr.Manager, opts ...Option) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxIdleConns: 100, MaxIdleConnsPerHost: 10,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second, ResponseHeaderTimeout: 10 * time.Second,
	}
	m := &Manager{baseCfg: cfg, boxMgr: boxMgr, ctx: ctx, cancel: cancel,
		httpClient: &http.Client{Transport: transport, Timeout: 60 * time.Second}}
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
	return &sub, nil
}

func (m *Manager) Update(ctx context.Context, id int64, input store.Subscription) (*store.Subscription, error) {
	current, err := m.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	current.Name, current.URL, current.Enabled = input.Name, input.URL, input.Enabled
	current.RefreshIntervalSeconds = input.RefreshIntervalSeconds
	current.RefreshTimeoutSeconds = input.RefreshTimeoutSeconds
	current.SortOrder = input.SortOrder
	applyDefaults(current, m.defaults)
	if err := validateSubscription(current); err != nil {
		return nil, err
	}
	if err := m.store.UpdateSubscription(ctx, current); err != nil {
		return nil, err
	}
	if err := m.reconcileRuntime(ctx); err != nil {
		return nil, err
	}
	return current, nil
}

func (m *Manager) Delete(ctx context.Context, id int64) error {
	if _, err := m.Get(ctx, id); err != nil {
		return err
	}
	if err := m.store.DeleteSubscription(ctx, id); err != nil {
		return err
	}
	return m.reconcileRuntime(ctx)
}

func (m *Manager) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	if _, err := m.Get(ctx, id); err != nil {
		return err
	}
	if err := m.store.SetSubscriptionEnabled(ctx, id, enabled); err != nil {
		return err
	}
	return m.reconcileRuntime(ctx)
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
	if sub.RefreshIntervalSeconds <= 0 {
		sub.RefreshIntervalSeconds = interval
	}
	if sub.RefreshTimeoutSeconds <= 0 {
		sub.RefreshTimeoutSeconds = timeout
	}
}

func validateSubscription(sub *store.Subscription) error {
	if strings.TrimSpace(sub.Name) == "" {
		return errors.New("订阅名称不能为空")
	}
	u, err := url.ParseRequestURI(strings.TrimSpace(sub.URL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("订阅 URL 必须是有效的 http/https URL")
	}
	if sub.RefreshIntervalSeconds <= 0 {
		return errors.New("刷新间隔必须大于 0")
	}
	if sub.RefreshTimeoutSeconds <= 0 {
		return errors.New("刷新超时必须大于 0")
	}
	return nil
}

func (m *Manager) RefreshOne(ctx context.Context, id int64) error {
	if !m.refreshMu.TryLock() {
		return errors.New("刷新正在进行")
	}
	defer m.refreshMu.Unlock()
	sub, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	err = m.refreshSubscription(ctx, sub)
	reconcileErr := m.reconcileRuntime(ctx)
	if err != nil {
		return err
	}
	return reconcileErr
}

func (m *Manager) RefreshNow() error {
	if !m.refreshMu.TryLock() {
		return errors.New("刷新正在进行")
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
	m.mu.Lock()
	m.status.IsRefreshing = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.status.IsRefreshing = false; m.status.RefreshCount++; m.mu.Unlock() }()
	var errs []error
	for i := range subs {
		sub := &subs[i]
		if !sub.Enabled {
			continue
		}
		if dueOnly && !sub.LastAttempt.IsZero() && now.Before(sub.LastAttempt.Add(time.Duration(sub.RefreshIntervalSeconds)*time.Second)) {
			continue
		}
		if err := m.refreshSubscription(ctx, sub); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sub.Name, err))
			m.logger.Warnf("refresh subscription %d failed: %v", sub.ID, err)
		}
	}
	if err := m.reconcileRuntime(ctx); err != nil {
		errs = append(errs, err)
	}
	joined := errors.Join(errs...)
	m.mu.Lock()
	m.status.LastRefresh = now
	if joined != nil {
		m.status.LastError = joined.Error()
	} else {
		m.status.LastError = ""
	}
	m.mu.Unlock()
	return joined
}

func (m *Manager) refreshSubscription(ctx context.Context, sub *store.Subscription) error {
	attempt := time.Now().UTC()
	timeout := time.Duration(sub.RefreshTimeoutSeconds) * time.Second
	if timeout <= 0 {
		_, seconds := m.defaults()
		timeout = time.Duration(seconds) * time.Second
	}
	nodes, etag, lastModified, err := m.fetchSubscription(ctx, sub.URL, timeout)
	if err == nil && len(nodes) == 0 {
		err = errors.New("订阅未返回有效节点")
	}
	if err != nil {
		sub.LastAttempt, sub.LastError = attempt, err.Error()
		if updateErr := m.store.UpdateSubscription(ctx, sub); updateErr != nil {
			return errors.Join(err, updateErr)
		}
		return err
	}
	inputs := make([]store.SubscriptionNodeInput, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	for i, n := range nodes {
		uri := strings.TrimSpace(n.URI)
		if _, ok := seen[uri]; ok {
			continue
		}
		seen[uri] = struct{}{}
		name := nodeName(n.Name, uri, i+1)
		inputs = append(inputs, store.SubscriptionNodeInput{URI: uri, Name: name, Port: n.Port,
			Username: n.Username, Password: n.Password, Enabled: true})
	}
	return m.store.CommitSnapshot(ctx, sub.ID, inputs, store.SubscriptionSnapshot{
		Attempt: attempt, Success: time.Now().UTC(), ETag: etag, LastModified: lastModified,
	})
}

func (m *Manager) fetchSubscription(parent context.Context, rawURL string, timeout time.Duration) ([]config.NodeConfig, string, string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "*/*")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024+1))
	if err != nil {
		return nil, "", "", fmt.Errorf("read body: %w", err)
	}
	if len(body) > 10*1024*1024 {
		return nil, "", "", errors.New("subscription response exceeds 10MB")
	}
	nodes, err := config.ParseSubscriptionContent(string(body))
	return nodes, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), err
}

func (m *Manager) reconcileRuntime(ctx context.Context) error {
	if m.store == nil || m.boxMgr == nil {
		return nil
	}
	regular, err := m.store.ListNodes(ctx, store.NodeFilter{})
	if err != nil {
		return fmt.Errorf("list runtime nodes: %w", err)
	}
	subscriptionNodes, err := m.store.ListEffectiveSubscriptionNodes(ctx)
	if err != nil {
		return fmt.Errorf("list effective subscription nodes: %w", err)
	}
	seen := make(map[string]struct{}, len(regular)+len(subscriptionNodes))
	all := make([]config.NodeConfig, 0, len(regular)+len(subscriptionNodes))
	appendNode := func(n store.Node) {
		uri := strings.TrimSpace(n.URI)
		if !n.Enabled || uri == "" {
			return
		}
		if _, ok := seen[uri]; ok {
			return
		}
		seen[uri] = struct{}{}
		all = append(all, config.NodeConfig{ID: n.ID, Name: n.Name, URI: uri, Port: n.Port,
			Username: n.Username, Password: n.Password, Source: config.NodeSource(n.Source),
			Region: n.Region, Country: n.Country})
	}
	for _, n := range regular {
		if n.Source != store.NodeSourceSubscription {
			appendNode(n)
		}
	}
	for _, n := range subscriptionNodes {
		appendNode(n)
	}
	m.mu.RLock()
	newCfg := m.baseCfg.Clone()
	m.mu.RUnlock()
	newCfg.Nodes = all
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

func (m *Manager) Status() monitor.SubscriptionStatus {
	m.mu.RLock()
	status := m.status
	globalEnabled := m.baseCfg.SubscriptionRefresh.Enabled
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
func (m *Manager) CheckNodesModified() bool { return false }
func (m *Manager) MarkNodesModified()       { m.mu.Lock(); m.status.NodesModified = true; m.mu.Unlock() }

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
