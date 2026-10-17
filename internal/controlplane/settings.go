package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/config"
)

// ApplyPlan reports what a settings change needs before it is fully live.
// NeedReload marks changes only a runtime rebuild can apply; NeedRestart marks
// changes only a process restart can apply (the management listener itself).
type ApplyPlan struct {
	NeedReload  bool     `json:"need_reload"`
	NeedRestart bool     `json:"need_restart"`
	Applied     []string `json:"applied"`
	Pending     []string `json:"pending"`
}

// SettingsUpdateResult is what the settings endpoints return.
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

// ValidationError marks a rejected request as the caller's fault. The HTTP
// layer maps it to 400; every other error is 500.
type ValidationError struct{ Err error }

func (e ValidationError) Error() string { return e.Err.Error() }
func (e ValidationError) Unwrap() error { return e.Err }

// ErrConfigUnavailable reports that no persistable config is bound.
var ErrConfigUnavailable = errors.New("配置存储未初始化")

// RuntimeApplier is the slice of monitor.Manager that settings changes touch.
// Keeping it here means the HTTP layer never reaches into the manager for
// settings, and the tests can supply a fake.
type RuntimeApplier interface {
	SetStartupAvailabilityPolicy(policy string)
	SetHealthCheckInterval(interval time.Duration)
	UpdateProbeTarget(target string) error
	UpdateProbePolicy(policy ProbePolicy)
}

// ProbePolicy mirrors the runtime probe policy settings can change.
type ProbePolicy struct {
	Concurrency     int
	StartupTimeout  time.Duration
	RoutineTimeout  time.Duration
	DialTimeout     time.Duration
	ResponseTimeout time.Duration
	RoutineRetries  int
}

// ReloadTrigger is the reload capability settings need.
type ReloadTrigger interface {
	TriggerReload(ctx context.Context) error
}

// SubscriptionApplier is the slice of the subscription manager that a refresh
// settings change pushes into the running refresher.
type SubscriptionApplier interface {
	ApplyConfig(cfg *config.Config)
}

// SubscriptionRefreshStore persists the per-subscription overrides that default
// to the global interval and timeout.
type SubscriptionRefreshStore interface {
	UpdateAllSubscriptionRefreshSettings(ctx context.Context, intervalSeconds, timeoutSeconds int) error
}

// SettingsOptions wires the settings domain.
type SettingsOptions struct {
	Logger *log.Logger
	// Config returns the persistable config object currently in force. It is a
	// callback because the object is replaced wholesale on every save.
	Config func() *config.Config
	// Commit publishes a new config object after it has been persisted. It must
	// be atomic with respect to Config.
	Commit   func(*config.Config)
	Runtime  RuntimeApplier
	Reloader ReloadTrigger
	Subs     SubscriptionApplier
	SubStore SubscriptionRefreshStore

	// ValidateProbeTarget and NormalizeAvailability are injected rather than
	// reimplemented: both rules belong to the runtime (probe target parsing and
	// the optimistic/strict vocabulary). Copying them here would let the two
	// drift, so callers pass the runtime's own functions.
	ValidateProbeTarget   func(target string) error
	NormalizeAvailability func(policy string) string
}

// Settings owns the config document: reading it, validating a proposed change,
// persisting it, publishing it, and telling the runtime what to hot-apply.
//
// It serialises writes internally, so two concurrent settings requests cannot
// interleave read-modify-write on the config file.
type Settings struct {
	mu       sync.Mutex // serialises the whole read-validate-save-publish cycle
	logger   *log.Logger
	getCfg   func() *config.Config
	commit   func(*config.Config)
	runtime  RuntimeApplier
	reloader ReloadTrigger
	subs     SubscriptionApplier
	subStore SubscriptionRefreshStore

	validateProbeTarget   func(string) error
	normalizeAvailability func(string) string
}

func NewSettings(opts SettingsOptions) *Settings {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	validate := opts.ValidateProbeTarget
	if validate == nil {
		validate = func(string) error { return nil }
	}
	normalize := opts.NormalizeAvailability
	if normalize == nil {
		normalize = normalizeAvailability
	}
	return &Settings{
		logger:   logger,
		getCfg:   opts.Config,
		commit:   opts.Commit,
		runtime:  opts.Runtime,
		reloader: opts.Reloader,
		subs:     opts.Subs,
		subStore: opts.SubStore,

		validateProbeTarget:   validate,
		normalizeAvailability: normalize,
	}
}

// Snapshot returns a deep copy of the current config, or nil when none is
// bound. Callers may read it without holding any lock.
func (s *Settings) Snapshot() *config.Config {
	if s.getCfg == nil {
		return nil
	}
	cfg := s.getCfg()
	if cfg == nil {
		return nil
	}
	return cfg.Snapshot()
}

// ProbeSettings is the probe-related slice of the config.
type ProbeSettings struct {
	ProbeTarget               string
	StartupAvailabilityPolicy string
	HealthCheckInterval       time.Duration
	ProbeConcurrency          int
	StartupProbeTimeout       time.Duration
	RoutineProbeTimeout       time.Duration
	ProbeDialTimeout          time.Duration
	ProbeResponseTimeout      time.Duration
	RoutineProbeRetries       int
}

// ProbeSettings reads the current probe settings.
func (s *Settings) ProbeSettings() (ProbeSettings, error) {
	cfg := s.Snapshot()
	if cfg == nil {
		return ProbeSettings{}, ErrConfigUnavailable
	}
	return probeSettingsOf(cfg), nil
}

func probeSettingsOf(c *config.Config) ProbeSettings {
	return ProbeSettings{
		ProbeTarget:               c.Management.ProbeTarget,
		StartupAvailabilityPolicy: c.Management.StartupAvailabilityPolicy,
		HealthCheckInterval:       c.Management.HealthCheckInterval,
		ProbeConcurrency:          c.Management.ProbeConcurrency,
		StartupProbeTimeout:       c.Management.StartupProbeTimeout,
		RoutineProbeTimeout:       c.Management.RoutineProbeTimeout,
		ProbeDialTimeout:          c.Management.ProbeDialTimeout,
		ProbeResponseTimeout:      c.Management.ProbeResponseTimeout,
		RoutineProbeRetries:       c.RoutineProbeRetryCount(),
	}
}

// UpdateProbeSettings validates, persists and hot-applies probe settings.
//
// Durations must be strictly positive; an invalid probe target or availability
// policy is rejected before anything is written.
func (s *Settings) UpdateProbeSettings(ctx context.Context, req ProbeSettings) (ProbeSettings, error) {
	target := strings.TrimSpace(req.ProbeTarget)
	if target != "" {
		if err := s.validateProbeTarget(target); err != nil {
			return ProbeSettings{}, ValidationError{fmt.Errorf("探测目标无效: %w", err)}
		}
	}
	policy := s.normalizeAvailability(req.StartupAvailabilityPolicy)
	if strings.TrimSpace(req.StartupAvailabilityPolicy) != "" &&
		policy != strings.ToLower(strings.TrimSpace(req.StartupAvailabilityPolicy)) {
		return ProbeSettings{}, ValidationError{errors.New("启动可用策略必须是 optimistic 或 strict")}
	}
	if err := config.ValidateManagementProbeSettings(req.ProbeConcurrency, req.StartupProbeTimeout,
		req.RoutineProbeTimeout, req.ProbeDialTimeout, req.ProbeResponseTimeout, req.RoutineProbeRetries); err != nil {
		return ProbeSettings{}, ValidationError{err}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.getCfg()
	if current == nil {
		return ProbeSettings{}, ErrConfigUnavailable
	}
	updated := current.Snapshot()
	oldTarget := updated.Management.ProbeTarget
	updated.Management.ProbeTarget = target
	updated.Management.StartupAvailabilityPolicy = policy
	updated.Management.HealthCheckInterval = req.HealthCheckInterval
	updated.Management.ProbeConcurrency = req.ProbeConcurrency
	updated.Management.StartupProbeTimeout = req.StartupProbeTimeout
	updated.Management.RoutineProbeTimeout = req.RoutineProbeTimeout
	updated.Management.ProbeDialTimeout = req.ProbeDialTimeout
	updated.Management.ProbeResponseTimeout = req.ProbeResponseTimeout
	retries := req.RoutineProbeRetries
	updated.Management.RoutineProbeRetries = &retries

	if err := updated.SaveSettings(); err != nil {
		return ProbeSettings{}, fmt.Errorf("保存配置失败: %w", err)
	}
	if s.commit != nil {
		s.commit(updated)
	}
	if s.runtime != nil {
		s.runtime.UpdateProbePolicy(ProbePolicy{
			Concurrency: req.ProbeConcurrency, StartupTimeout: req.StartupProbeTimeout,
			RoutineTimeout: req.RoutineProbeTimeout, DialTimeout: req.ProbeDialTimeout,
			ResponseTimeout: req.ProbeResponseTimeout, RoutineRetries: retries,
		})
		s.runtime.SetStartupAvailabilityPolicy(policy)
		s.runtime.SetHealthCheckInterval(req.HealthCheckInterval)
		if oldTarget != target {
			// The config is already saved; a runtime failure here is reported but
			// does not roll the file back, matching the previous behaviour.
			if err := s.runtime.UpdateProbeTarget(target); err != nil {
				return ProbeSettings{}, fmt.Errorf("运行时应用探测目标失败: %w", err)
			}
		}
	}
	return probeSettingsOf(updated), nil
}

// AllSettings is the flat view of every editable setting.
type AllSettings struct {
	LogLevel       string
	ExternalIP     string
	SkipCertVerify bool

	ListenerEnabled  bool
	ListenerAddress  string
	ListenerPort     uint16
	ListenerProtocol string
	ListenerUsername string
	ListenerPassword string

	MultiPortEnabled  bool
	MultiPortAddress  string
	MultiPortBasePort uint16
	MultiPortProtocol string
	MultiPortUsername string
	MultiPortPassword string

	PoolMode              string
	PoolFailureThreshold  int
	PoolBlacklistDuration string

	ManagementEnabled                   bool
	ManagementListen                    string
	ManagementProbeTarget               string
	ManagementStartupAvailabilityPolicy string
	ManagementPassword                  string
	ManagementHealthCheckInterval       string

	GeoIPEnabled            bool
	GeoIPDatabasePath       string
	GeoIPAutoUpdateEnabled  bool
	GeoIPAutoUpdateInterval string

	Subscriptions []string
}

// AllSettings reads every editable setting.
func (s *Settings) AllSettings() (AllSettings, error) {
	cfg := s.Snapshot()
	if cfg == nil {
		return AllSettings{}, ErrConfigUnavailable
	}
	return allSettingsOf(cfg), nil
}

func allSettingsOf(c *config.Config) AllSettings {
	mgmtEnabled := true
	if c.Management.Enabled != nil {
		mgmtEnabled = *c.Management.Enabled
	}
	return AllSettings{
		LogLevel:       c.LogLevel,
		ExternalIP:     c.ExternalIP,
		SkipCertVerify: c.SkipCertVerify,

		ListenerEnabled:  c.Listener.Enabled,
		ListenerAddress:  c.Listener.Address,
		ListenerPort:     c.Listener.Port,
		ListenerProtocol: c.Listener.Protocol,
		ListenerUsername: c.Listener.Username,
		ListenerPassword: c.Listener.Password,

		MultiPortEnabled:  c.MultiPort.Enabled,
		MultiPortAddress:  c.MultiPort.Address,
		MultiPortBasePort: c.MultiPort.BasePort,
		MultiPortProtocol: c.MultiPort.Protocol,
		MultiPortUsername: c.MultiPort.Username,
		MultiPortPassword: c.MultiPort.Password,

		PoolMode:              c.Pool.Mode,
		PoolFailureThreshold:  c.Pool.FailureThreshold,
		PoolBlacklistDuration: c.Pool.BlacklistDuration.String(),

		ManagementEnabled:                   mgmtEnabled,
		ManagementListen:                    c.Management.Listen,
		ManagementProbeTarget:               c.Management.ProbeTarget,
		ManagementStartupAvailabilityPolicy: c.Management.StartupAvailabilityPolicy,
		ManagementPassword:                  c.Management.Password,
		ManagementHealthCheckInterval:       c.Management.HealthCheckInterval.String(),

		GeoIPEnabled:            c.GeoIP.Enabled,
		GeoIPDatabasePath:       c.GeoIP.DatabasePath,
		GeoIPAutoUpdateEnabled:  c.GeoIP.AutoUpdateEnabled,
		GeoIPAutoUpdateInterval: c.GeoIP.AutoUpdateInterval.String(),

		Subscriptions: c.Subscriptions,
	}
}

// AllSettingsRequest is a proposed full-settings change. ManagementEnabled is a
// pointer so "absent" (leave unchanged) is distinguishable from "false".
type AllSettingsRequest struct {
	LogLevel       string
	ExternalIP     string
	SkipCertVerify bool

	ListenerEnabled  bool
	ListenerAddress  string
	ListenerPort     uint16
	ListenerProtocol string
	ListenerUsername string
	ListenerPassword string

	MultiPortEnabled  bool
	MultiPortAddress  string
	MultiPortBasePort uint16
	MultiPortProtocol string
	MultiPortUsername string
	MultiPortPassword string

	PoolMode              string
	PoolFailureThreshold  int
	PoolBlacklistDuration string

	ManagementEnabled                   *bool
	ManagementListen                    string
	ManagementProbeTarget               string
	ManagementStartupAvailabilityPolicy string
	ManagementPassword                  string
	ManagementHealthCheckInterval       string

	GeoIPEnabled            bool
	GeoIPDatabasePath       string
	GeoIPAutoUpdateEnabled  bool
	GeoIPAutoUpdateInterval string
}

// UpdateAllSettings validates, persists and hot-applies the full settings form.
//
// Subscription auto-refresh is intentionally not writable here: it is owned by
// UpdateSubscriptionRefreshSettings so this form can never clobber it with
// stale values, and the persisted values are carried over unchanged.
func (s *Settings) UpdateAllSettings(ctx context.Context, req AllSettingsRequest) (SettingsUpdateResult, error) {
	if err := config.ValidateSettingsRequest(
		req.ListenerEnabled, req.MultiPortEnabled, req.ListenerPort, req.MultiPortBasePort,
		req.ListenerProtocol, req.MultiPortProtocol,
		req.PoolBlacklistDuration,
		req.GeoIPAutoUpdateInterval, req.ManagementHealthCheckInterval,
	); err != nil {
		return SettingsUpdateResult{}, ValidationError{fmt.Errorf("参数验证失败: %w", err)}
	}
	if target := strings.TrimSpace(req.ManagementProbeTarget); target != "" {
		if err := s.validateProbeTarget(target); err != nil {
			return SettingsUpdateResult{}, ValidationError{fmt.Errorf("参数验证失败: 探测目标无效: %w", err)}
		}
	}
	availabilityPolicy := s.normalizeAvailability(req.ManagementStartupAvailabilityPolicy)
	if strings.TrimSpace(req.ManagementStartupAvailabilityPolicy) != "" &&
		availabilityPolicy != strings.ToLower(strings.TrimSpace(req.ManagementStartupAvailabilityPolicy)) {
		return SettingsUpdateResult{}, ValidationError{errors.New("参数验证失败: 启动可用策略必须是 optimistic 或 strict")}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.getCfg()
	if current == nil {
		return SettingsUpdateResult{}, ErrConfigUnavailable
	}
	old := current.Snapshot()
	updated := old.Clone()

	updated.LogLevel = req.LogLevel
	updated.ExternalIP = strings.TrimSpace(req.ExternalIP)
	updated.SkipCertVerify = req.SkipCertVerify

	updated.Listener.Enabled = req.ListenerEnabled
	updated.Listener.Address = req.ListenerAddress
	updated.Listener.Port = req.ListenerPort
	if p, err := config.NormalizeInboundProtocol(req.ListenerProtocol); err == nil {
		updated.Listener.Protocol = p
	}
	updated.Listener.Username = req.ListenerUsername
	updated.Listener.Password = req.ListenerPassword

	updated.MultiPort.Enabled = req.MultiPortEnabled
	updated.MultiPort.Address = req.MultiPortAddress
	updated.MultiPort.BasePort = req.MultiPortBasePort
	if p, err := config.NormalizeInboundProtocol(req.MultiPortProtocol); err == nil {
		updated.MultiPort.Protocol = p
	}
	updated.MultiPort.Username = req.MultiPortUsername
	updated.MultiPort.Password = req.MultiPortPassword

	updated.Pool.Mode = req.PoolMode
	updated.Pool.FailureThreshold = req.PoolFailureThreshold
	if d, err := time.ParseDuration(req.PoolBlacklistDuration); err == nil && d > 0 {
		updated.Pool.BlacklistDuration = d
	}

	if req.ManagementEnabled != nil {
		updated.Management.Enabled = req.ManagementEnabled
	}
	updated.Management.Listen = req.ManagementListen
	updated.Management.ProbeTarget = strings.TrimSpace(req.ManagementProbeTarget)
	updated.Management.StartupAvailabilityPolicy = availabilityPolicy
	updated.Management.Password = req.ManagementPassword
	if d, err := time.ParseDuration(req.ManagementHealthCheckInterval); err == nil && d > 0 {
		updated.Management.HealthCheckInterval = d
	}

	// Owned by the subscription refresh endpoint; carry the stored value over.
	updated.SubscriptionRefresh = old.SubscriptionRefresh
	// Legacy subscriptions are managed by the subscription API.
	updated.Subscriptions = append([]string(nil), old.Subscriptions...)

	plan := settingsApplyPlan(old, updated)

	if err := updated.SaveSettings(); err != nil {
		return SettingsUpdateResult{}, fmt.Errorf("保存配置失败: %w", err)
	}
	if s.commit != nil {
		s.commit(updated)
	}
	if s.runtime != nil {
		s.runtime.SetStartupAvailabilityPolicy(updated.Management.StartupAvailabilityPolicy)
		if err := s.runtime.UpdateProbeTarget(updated.Management.ProbeTarget); err != nil {
			s.logger.Printf("更新探测目标失败: %v", err)
		}
		s.runtime.SetHealthCheckInterval(updated.Management.HealthCheckInterval)
	}
	if s.subs != nil {
		s.subs.ApplyConfig(updated)
	}

	result := SettingsUpdateResult{
		Saved: true, NeedReload: plan.NeedReload, NeedRestart: plan.NeedRestart,
		Applied: plan.Applied, Pending: plan.Pending,
	}
	if plan.NeedReload && s.reloader != nil {
		if err := s.reloader.TriggerReload(ctx); err != nil {
			result.ReloadError = err.Error()
		} else {
			result.NeedReload, result.Reloaded = false, true
			result.Applied = append(result.Applied, "runtime_config")
			result.Pending = removeString(result.Pending, "runtime_config")
		}
	}
	switch {
	case result.NeedRestart:
		result.Message = "设置已保存；管理服务启用状态或监听地址变更，需重启进程生效"
	case result.NeedReload:
		result.Message = "设置已保存；运行时重载未完成"
	default:
		result.Message = "设置已保存并应用"
	}
	return result, nil
}

// SubscriptionRefreshSettings is the auto-refresh configuration.
type SubscriptionRefreshSettings struct {
	Enabled            bool
	Interval           time.Duration
	Timeout            time.Duration
	HealthCheckTimeout time.Duration
	DrainTimeout       time.Duration
	MinAvailableNodes  int
}

// SubscriptionRefreshSettings reads the current auto-refresh configuration.
func (s *Settings) SubscriptionRefreshSettings() (SubscriptionRefreshSettings, error) {
	cfg := s.Snapshot()
	if cfg == nil {
		return SubscriptionRefreshSettings{}, ErrConfigUnavailable
	}
	c := cfg.SubscriptionRefresh
	return SubscriptionRefreshSettings{
		Enabled: c.Enabled, Interval: c.Interval, Timeout: c.Timeout,
		HealthCheckTimeout: c.HealthCheckTimeout, DrainTimeout: c.DrainTimeout,
		MinAvailableNodes: c.MinAvailableNodes,
	}, nil
}

// UpdateSubscriptionRefreshSettings is the single writer of the auto-refresh
// block. Empty or non-positive durations keep the stored value.
//
// Drain timeout and the minimum-available-nodes floor are owned by the runtime
// box manager, so changing them needs a reload; the timeouts apply in place.
func (s *Settings) UpdateSubscriptionRefreshSettings(ctx context.Context, req SubscriptionRefreshSettings) (SettingsUpdateResult, error) {
	if req.MinAvailableNodes < 0 {
		return SettingsUpdateResult{}, ValidationError{errors.New("参数验证失败: 最少可用节点数不能为负")}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.getCfg()
	if current == nil {
		return SettingsUpdateResult{}, ErrConfigUnavailable
	}
	old := current.Snapshot()

	// A zero duration means "keep the stored value", so validation must run
	// against the effective values: validating the raw zeroes would reject an
	// omitted field as "0s".
	effective := old.SubscriptionRefresh
	if req.Interval > 0 {
		effective.Interval = req.Interval
	}
	if req.Timeout > 0 {
		effective.Timeout = req.Timeout
	}
	if req.HealthCheckTimeout > 0 {
		effective.HealthCheckTimeout = req.HealthCheckTimeout
	}
	if req.DrainTimeout > 0 {
		effective.DrainTimeout = req.DrainTimeout
	}
	if err := config.ValidateSubscriptionRefreshSettings(
		effective.Interval.String(), effective.Timeout.String(),
		effective.HealthCheckTimeout.String(), effective.DrainTimeout.String(),
	); err != nil {
		return SettingsUpdateResult{}, ValidationError{fmt.Errorf("参数验证失败: %w", err)}
	}

	updated := old.Clone()

	updated.SubscriptionRefresh.Enabled = req.Enabled
	if req.Interval > 0 {
		updated.SubscriptionRefresh.Interval = req.Interval
	}
	if req.Timeout > 0 {
		updated.SubscriptionRefresh.Timeout = req.Timeout
	}
	if req.HealthCheckTimeout > 0 {
		updated.SubscriptionRefresh.HealthCheckTimeout = req.HealthCheckTimeout
	}
	if req.DrainTimeout > 0 {
		updated.SubscriptionRefresh.DrainTimeout = req.DrainTimeout
	}
	updated.SubscriptionRefresh.MinAvailableNodes = req.MinAvailableNodes

	if err := updated.SaveSettings(); err != nil {
		return SettingsUpdateResult{}, fmt.Errorf("保存配置失败: %w", err)
	}
	if s.commit != nil {
		s.commit(updated)
	}
	if s.subs != nil {
		s.subs.ApplyConfig(updated)
	}
	// Per-subscription overrides default to the global interval and timeout, so
	// a change here has to be fanned out to every stored subscription.
	if s.subStore != nil &&
		(old.SubscriptionRefresh.Interval != updated.SubscriptionRefresh.Interval ||
			old.SubscriptionRefresh.Timeout != updated.SubscriptionRefresh.Timeout) {
		interval, timeout := 0, 0
		if old.SubscriptionRefresh.Interval != updated.SubscriptionRefresh.Interval {
			interval = int(updated.SubscriptionRefresh.Interval.Seconds())
		}
		if old.SubscriptionRefresh.Timeout != updated.SubscriptionRefresh.Timeout {
			timeout = int(updated.SubscriptionRefresh.Timeout.Seconds())
		}
		if err := s.subStore.UpdateAllSubscriptionRefreshSettings(ctx, interval, timeout); err != nil {
			s.logger.Printf("批量更新订阅刷新设置失败: %v", err)
		}
	}

	result := SettingsUpdateResult{
		Saved:   true,
		Applied: []string{"sub_refresh_enabled"},
		Pending: []string{},
		Message: "订阅自动刷新设置已保存并生效",
	}
	needsReload := old.SubscriptionRefresh.DrainTimeout != updated.SubscriptionRefresh.DrainTimeout ||
		old.SubscriptionRefresh.MinAvailableNodes != updated.SubscriptionRefresh.MinAvailableNodes
	if needsReload && s.reloader != nil {
		if err := s.reloader.TriggerReload(ctx); err != nil {
			result.NeedReload = true
			result.Pending = append(result.Pending, "runtime_config")
			result.ReloadError = err.Error()
			result.Message = "设置已保存；运行时重载未完成"
		} else {
			result.Applied = append(result.Applied, "runtime_config")
		}
	}
	return result, nil
}

// settingsApplyPlan classifies a change: restart for the management listener
// itself, reload for everything the runtime must rebuild, applied for fields
// the runtime picks up in place.
func settingsApplyPlan(old, updated *config.Config) ApplyPlan {
	plan := ApplyPlan{Applied: []string{}, Pending: []string{}}
	plan.NeedRestart = old.ManagementEnabled() != updated.ManagementEnabled() ||
		old.Management.Listen != updated.Management.Listen
	a, b := old.Clone(), updated.Clone()
	a.Management.Enabled, b.Management.Enabled = nil, nil
	a.Management.Listen, b.Management.Listen = "", ""
	a.Management.Password, b.Management.Password = "", ""
	a.Management.ProbeTarget, b.Management.ProbeTarget = "", ""
	a.Management.StartupAvailabilityPolicy, b.Management.StartupAvailabilityPolicy = "", ""
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
	if old.Management.StartupAvailabilityPolicy != updated.Management.StartupAvailabilityPolicy {
		plan.Applied = append(plan.Applied, "management_startup_availability_policy")
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

// normalizeAvailability canonicalises the startup availability policy.
func normalizeAvailability(policy string) string {
	if strings.EqualFold(strings.TrimSpace(policy), "strict") {
		return "strict"
	}
	return "optimistic"
}

// Mutate serialises a config-document change that is not one of the three
// settings forms: it hands fn a private clone, persists the result, and only
// commits when saving succeeded. Using it keeps every writer — the generic
// form, probe settings, subscription refresh and the node-check settings — on
// the same mutex, so two concurrent writes cannot clobber each other.
func (s *Settings) Mutate(ctx context.Context, fn func(updated *config.Config) error) (*config.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.getCfg()
	if current == nil {
		return nil, ErrConfigUnavailable
	}
	updated := current.Snapshot()
	if err := fn(updated); err != nil {
		return nil, err
	}
	if err := updated.SaveSettings(); err != nil {
		return nil, fmt.Errorf("保存配置失败: %w", err)
	}
	if s.commit != nil {
		s.commit(updated)
	}
	return updated, nil
}

// ApplyPlanForTest exposes the change classifier to the HTTP layer's contract
// tests. It is not part of the service API: production code reaches it only
// through UpdateAllSettings.
func ApplyPlanForTest(old, updated *config.Config) ApplyPlan { return settingsApplyPlan(old, updated) }
