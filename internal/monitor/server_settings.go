package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"easy_proxies/internal/config"
	json "easy_proxies/internal/jsonx"
)

type probeSettingsResponse struct {
	ProbeTarget               string `json:"probe_target"`
	StartupAvailabilityPolicy string `json:"startup_availability_policy"`
	HealthCheckInterval       string `json:"health_check_interval"`
	ProbeConcurrency          int    `json:"probe_concurrency"`
	StartupProbeTimeout       string `json:"startup_probe_timeout"`
	RoutineProbeTimeout       string `json:"routine_probe_timeout"`
	ProbeDialTimeout          string `json:"probe_dial_timeout"`
	ProbeResponseTimeout      string `json:"probe_response_timeout"`
	RoutineProbeRetries       int    `json:"routine_probe_retries"`
}

type probeStatusResponse struct {
	NodeCount                    int              `json:"node_count"`
	ConcurrencyMode              string           `json:"concurrency_mode"`
	ConfiguredConcurrency        int              `json:"configured_concurrency"`
	EffectiveConcurrency         int              `json:"effective_concurrency"`
	EstimatedStartupWorstCase    string           `json:"estimated_startup_worst_case"`
	EstimatedStartupWorstSeconds float64          `json:"estimated_startup_worst_seconds"`
	EstimatedRoutineWorstCase    string           `json:"estimated_routine_worst_case"`
	EstimatedRoutineWorstSeconds float64          `json:"estimated_routine_worst_seconds"`
	Converging                   bool             `json:"converging"`
	InitialPending               int              `json:"initial_pending"`
	Queued                       int              `json:"queued"`
	Scheduled                    int              `json:"scheduled"`
	WaitingForManual             bool             `json:"waiting_for_manual"`
	Round                        ProbeRoundStatus `json:"round"`
}

func probeSettingsFromConfig(c *config.Config) probeSettingsResponse {
	return probeSettingsResponse{
		ProbeTarget:               c.Management.ProbeTarget,
		StartupAvailabilityPolicy: c.Management.StartupAvailabilityPolicy,
		HealthCheckInterval:       c.Management.HealthCheckInterval.String(),
		ProbeConcurrency:          c.Management.ProbeConcurrency,
		StartupProbeTimeout:       c.Management.StartupProbeTimeout.String(),
		RoutineProbeTimeout:       c.Management.RoutineProbeTimeout.String(),
		ProbeDialTimeout:          c.Management.ProbeDialTimeout.String(),
		ProbeResponseTimeout:      c.Management.ProbeResponseTimeout.String(),
		RoutineProbeRetries:       c.RoutineProbeRetryCount(),
	}
}

func parsePositiveDuration(name, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s必须是大于 0 的 Go 时长（例如 5s、2m）", name)
	}
	return duration, nil
}

func (s *Server) handleProbeSettingsGet(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	cfg := s.cfgSrc
	s.cfgMu.RUnlock()
	if cfg == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "配置存储未初始化")
		return
	}
	snapshot := cfg.Snapshot()
	writeJSON(w, probeSettingsFromConfig(snapshot))
}

func (s *Server) handleProbeSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	var request probeSettingsResponse
	if err := decodeJSON(r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	startupTimeout, err := parsePositiveDuration("启动探测超时", request.StartupProbeTimeout)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	routineTimeout, err := parsePositiveDuration("周期/手动探测超时", request.RoutineProbeTimeout)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	dialTimeout, err := parsePositiveDuration("拨号超时", request.ProbeDialTimeout)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	responseTimeout, err := parsePositiveDuration("响应超时", request.ProbeResponseTimeout)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	healthInterval, err := parsePositiveDuration("健康检查间隔", request.HealthCheckInterval)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	probeTarget := strings.TrimSpace(request.ProbeTarget)
	if probeTarget != "" {
		if _, err := parseProbeTarget(probeTarget); err != nil {
			writeAPIError(w, http.StatusBadRequest, "探测目标无效: "+err.Error())
			return
		}
	}
	availabilityPolicy := normalizeStartupAvailabilityPolicy(request.StartupAvailabilityPolicy)
	if strings.TrimSpace(request.StartupAvailabilityPolicy) != "" && availabilityPolicy != strings.ToLower(strings.TrimSpace(request.StartupAvailabilityPolicy)) {
		writeAPIError(w, http.StatusBadRequest, "启动可用策略必须是 optimistic 或 strict")
		return
	}
	if err := config.ValidateManagementProbeSettings(request.ProbeConcurrency, startupTimeout,
		routineTimeout, dialTimeout, responseTimeout, request.RoutineProbeRetries); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.configMutationMu.Lock()
	defer s.configMutationMu.Unlock()
	s.cfgMu.RLock()
	current := s.cfgSrc
	s.cfgMu.RUnlock()
	if current == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "配置存储未初始化")
		return
	}
	updated := current.Snapshot()
	oldTarget := updated.Management.ProbeTarget
	updated.Management.ProbeTarget = probeTarget
	updated.Management.StartupAvailabilityPolicy = availabilityPolicy
	updated.Management.HealthCheckInterval = healthInterval
	updated.Management.ProbeConcurrency = request.ProbeConcurrency
	updated.Management.StartupProbeTimeout = startupTimeout
	updated.Management.RoutineProbeTimeout = routineTimeout
	updated.Management.ProbeDialTimeout = dialTimeout
	updated.Management.ProbeResponseTimeout = responseTimeout
	retries := request.RoutineProbeRetries
	updated.Management.RoutineProbeRetries = &retries
	if err := updated.SaveSettings(); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
		return
	}
	s.cfgMu.Lock()
	s.cfgSrc = updated
	s.cfg.ProbeTarget = probeTarget
	s.cfgMu.Unlock()
	s.mgr.UpdateProbePolicy(ProbePolicy{
		Concurrency: request.ProbeConcurrency, StartupTimeout: startupTimeout,
		RoutineTimeout: routineTimeout, DialTimeout: dialTimeout,
		ResponseTimeout: responseTimeout, RoutineRetries: retries,
	})
	s.mgr.SetStartupAvailabilityPolicy(availabilityPolicy)
	s.mgr.SetHealthCheckInterval(healthInterval)
	if oldTarget != probeTarget {
		if err := s.mgr.UpdateProbeTarget(probeTarget); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "运行时应用探测目标失败: "+err.Error())
			return
		}
	}
	writeJSON(w, probeSettingsFromConfig(updated))
}

func (s *Server) handleProbeStatus(w http.ResponseWriter, r *http.Request) {
	policy := s.mgr.ProbePolicy()
	nodeCount := s.mgr.ProbeNodeCount()
	effective := effectiveProbeConcurrency(policy.Concurrency, nodeCount)
	rounds := 0
	if effective > 0 {
		rounds = (nodeCount + effective - 1) / effective
	}
	startupEstimate := time.Duration(rounds) * (2*policy.StartupTimeout + initialProbeRetryDelay)
	routineEstimate := time.Duration(rounds) * policy.RoutineTimeout
	initial := s.mgr.InitialProbeStatus()
	mode := "fixed"
	if policy.Concurrency == 0 {
		mode = "auto"
	}
	round := s.mgr.ProbeRoundStatus()
	scheduled := 0
	if round.InFlight && round.Kind == ProbeRoundStartup {
		scheduled = max(0, round.Total-round.Completed)
	}
	writeJSON(w, probeStatusResponse{
		NodeCount: nodeCount, ConcurrencyMode: mode,
		ConfiguredConcurrency: policy.Concurrency, EffectiveConcurrency: effective,
		EstimatedStartupWorstCase: startupEstimate.String(), EstimatedStartupWorstSeconds: startupEstimate.Seconds(),
		EstimatedRoutineWorstCase: routineEstimate.String(), EstimatedRoutineWorstSeconds: routineEstimate.Seconds(),
		Converging: initial.Converging, InitialPending: initial.Pending, Queued: initial.Queued,
		Scheduled: scheduled, WaitingForManual: initial.Converging && round.InFlight && round.Kind == ProbeRoundManual,
		Round: round,
	})
}

// allSettingsResponse is the JSON structure for GET /api/settings.
type allSettingsResponse struct {
	// Global
	LogLevel       string `json:"log_level"`
	ExternalIP     string `json:"external_ip"`
	SkipCertVerify bool   `json:"skip_cert_verify"`

	// Listener
	ListenerEnabled  bool   `json:"listener_enabled"`
	ListenerAddress  string `json:"listener_address"`
	ListenerPort     uint16 `json:"listener_port"`
	ListenerProtocol string `json:"listener_protocol"`
	ListenerUsername string `json:"listener_username"`
	ListenerPassword string `json:"listener_password"`

	// Multi-port
	MultiPortEnabled  bool   `json:"multi_port_enabled"`
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
	ManagementEnabled                   bool   `json:"management_enabled"`
	ManagementListen                    string `json:"management_listen"`
	ManagementProbeTarget               string `json:"management_probe_target"`
	ManagementStartupAvailabilityPolicy string `json:"management_startup_availability_policy"`
	ManagementPassword                  string `json:"management_password"`
	ManagementHealthCheckInterval       string `json:"management_health_check_interval"`

	// Subscription auto-refresh moved to /api/subscription/refresh-settings
	// (subscription management page); it is intentionally absent from this
	// generic settings surface.

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
	LogLevel       string `json:"log_level"`
	ExternalIP     string `json:"external_ip"`
	SkipCertVerify bool   `json:"skip_cert_verify"`

	// Listener
	ListenerEnabled  bool   `json:"listener_enabled"`
	ListenerAddress  string `json:"listener_address"`
	ListenerPort     uint16 `json:"listener_port"`
	ListenerProtocol string `json:"listener_protocol"`
	ListenerUsername string `json:"listener_username"`
	ListenerPassword string `json:"listener_password"`

	// Multi-port
	MultiPortEnabled  bool   `json:"multi_port_enabled"`
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
	ManagementEnabled                   *bool  `json:"management_enabled"`
	ManagementListen                    string `json:"management_listen"`
	ManagementProbeTarget               string `json:"management_probe_target"`
	ManagementStartupAvailabilityPolicy string `json:"management_startup_availability_policy"`
	ManagementPassword                  string `json:"management_password"`
	ManagementHealthCheckInterval       string `json:"management_health_check_interval"`

	// Subscription auto-refresh moved to /api/subscription/refresh-settings
	// (subscription management page); it is intentionally absent from this
	// generic settings surface.

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

// updateAllSettings applies all settings from request and persists to config file.
func (s *Server) updateAllSettings(ctx context.Context, req allSettingsRequest) (SettingsUpdateResult, error) {
	// Validate request before applying
	if err := config.ValidateSettingsRequest(
		req.ListenerEnabled, req.MultiPortEnabled, req.ListenerPort, req.MultiPortBasePort,
		req.ListenerProtocol, req.MultiPortProtocol,
		req.PoolBlacklistDuration,
		req.GeoIPAutoUpdateInterval, req.ManagementHealthCheckInterval,
	); err != nil {
		return SettingsUpdateResult{}, settingsValidationError{fmt.Errorf("参数验证失败: %w", err)}
	}
	if target := strings.TrimSpace(req.ManagementProbeTarget); target != "" {
		if _, err := parseProbeTarget(target); err != nil {
			return SettingsUpdateResult{}, settingsValidationError{fmt.Errorf("参数验证失败: 探测目标无效: %w", err)}
		}
	}
	availabilityPolicy := normalizeStartupAvailabilityPolicy(req.ManagementStartupAvailabilityPolicy)
	if strings.TrimSpace(req.ManagementStartupAvailabilityPolicy) != "" && availabilityPolicy != strings.ToLower(strings.TrimSpace(req.ManagementStartupAvailabilityPolicy)) {
		return SettingsUpdateResult{}, settingsValidationError{errors.New("参数验证失败: 启动可用策略必须是 optimistic 或 strict")}
	}
	s.configMutationMu.Lock()
	defer s.configMutationMu.Unlock()

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
	c.LogLevel = req.LogLevel
	c.ExternalIP = strings.TrimSpace(req.ExternalIP)
	c.SkipCertVerify = req.SkipCertVerify

	// Listener
	c.Listener.Enabled = req.ListenerEnabled
	c.Listener.Address = req.ListenerAddress
	c.Listener.Port = req.ListenerPort
	if p, err := config.NormalizeInboundProtocol(req.ListenerProtocol); err == nil {
		c.Listener.Protocol = p
	}
	c.Listener.Username = req.ListenerUsername
	c.Listener.Password = req.ListenerPassword

	// Multi-port
	c.MultiPort.Enabled = req.MultiPortEnabled
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
	c.Management.StartupAvailabilityPolicy = availabilityPolicy
	c.Management.Password = req.ManagementPassword
	if d, err := time.ParseDuration(req.ManagementHealthCheckInterval); err == nil && d > 0 {
		c.Management.HealthCheckInterval = d
	}

	// Subscription refresh is intentionally NOT writable through this endpoint;
	// it is owned by PUT /api/subscription/refresh-settings so the generic
	// settings form can never clobber it. Persisted values are preserved.
	c.SubscriptionRefresh = old.SubscriptionRefresh

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
	s.cfg.StartupAvailabilityPolicy = c.Management.StartupAvailabilityPolicy
	s.cfg.SkipCertVerify = c.SkipCertVerify
	s.cfg.Password = c.Management.Password
	s.cfgMu.Unlock()

	if s.mgr != nil {
		s.mgr.SetStartupAvailabilityPolicy(c.Management.StartupAvailabilityPolicy)
		if err := s.mgr.UpdateProbeTarget(c.Management.ProbeTarget); err != nil {
			s.logger.Printf("更新探测目标失败: %v", err)
		}
		s.mgr.SetHealthCheckInterval(c.Management.HealthCheckInterval)
	}
	if s.subRefresher != nil {
		s.subRefresher.ApplyConfig(c)
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

// handleSettingsGet returns every system setting.
func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.getAllSettings())
}

// handleSettingsUpdate validates, persists and hot-applies system settings.
func (s *Server) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
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
}

// subscriptionRefreshSettingsResponse is the wire shape of
// config.SubscriptionRefreshConfig for the subscription management page.
type subscriptionRefreshSettingsResponse struct {
	Enabled            bool   `json:"sub_refresh_enabled"`
	Interval           string `json:"sub_refresh_interval"`
	Timeout            string `json:"sub_refresh_timeout"`
	HealthCheckTimeout string `json:"sub_refresh_health_check_timeout"`
	DrainTimeout       string `json:"sub_refresh_drain_timeout"`
	MinAvailableNodes  int    `json:"sub_refresh_min_available_nodes"`
}

// subscriptionRefreshSettingsRequest is the PUT body for the same surface.
type subscriptionRefreshSettingsRequest = subscriptionRefreshSettingsResponse

// The subscription auto-refresh surface is the single writer of
// config.SubscriptionRefresh, so the generic /api/settings form can never
// overwrite it with stale values.
func (s *Server) handleSubscriptionRefreshSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.subscriptionRefreshSettings())
}

func (s *Server) handleSubscriptionRefreshSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	var req subscriptionRefreshSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.updateSubscriptionRefreshSettings(r.Context(), req)
	if err != nil {
		status := http.StatusInternalServerError
		var validationErr settingsValidationError
		if errors.As(err, &validationErr) {
			status = http.StatusBadRequest
		}
		writeAPIError(w, status, err.Error())
		return
	}
	writeJSON(w, result)
}

// subscriptionRefreshSettings reads the current auto-refresh configuration.
func (s *Server) subscriptionRefreshSettings() subscriptionRefreshSettingsResponse {
	s.cfgMu.RLock()
	c := s.cfgSrc
	s.cfgMu.RUnlock()
	if c == nil {
		return subscriptionRefreshSettingsResponse{}
	}

	c.RLock()
	defer c.RUnlock()
	return subscriptionRefreshSettingsResponse{
		Enabled:            c.SubscriptionRefresh.Enabled,
		Interval:           c.SubscriptionRefresh.Interval.String(),
		Timeout:            c.SubscriptionRefresh.Timeout.String(),
		HealthCheckTimeout: c.SubscriptionRefresh.HealthCheckTimeout.String(),
		DrainTimeout:       c.SubscriptionRefresh.DrainTimeout.String(),
		MinAvailableNodes:  c.SubscriptionRefresh.MinAvailableNodes,
	}
}

// updateSubscriptionRefreshSettings validates, persists and hot-applies the
// auto-refresh configuration. Empty or non-positive durations keep the stored
// value, matching the generic settings form. Only drain timeout and minimum
// available nodes are owned by the runtime box manager and therefore need a
// reload; interval/timeout/health-check timeout are applied in place.
func (s *Server) updateSubscriptionRefreshSettings(ctx context.Context, req subscriptionRefreshSettingsRequest) (SettingsUpdateResult, error) {
	if err := config.ValidateSubscriptionRefreshSettings(req.Interval, req.Timeout, req.HealthCheckTimeout, req.DrainTimeout); err != nil {
		return SettingsUpdateResult{}, settingsValidationError{fmt.Errorf("参数验证失败: %w", err)}
	}
	if req.MinAvailableNodes < 0 {
		return SettingsUpdateResult{}, settingsValidationError{errors.New("参数验证失败: 最少可用节点数不能为负")}
	}

	s.configMutationMu.Lock()
	defer s.configMutationMu.Unlock()

	s.cfgMu.RLock()
	c := s.cfgSrc
	s.cfgMu.RUnlock()
	if c == nil {
		return SettingsUpdateResult{}, errors.New("配置存储未初始化")
	}

	old := c.Snapshot()
	c = old.Clone()

	c.SubscriptionRefresh.Enabled = req.Enabled
	if d, err := time.ParseDuration(req.Interval); err == nil && d > 0 {
		c.SubscriptionRefresh.Interval = d
	}
	if d, err := time.ParseDuration(req.Timeout); err == nil && d > 0 {
		c.SubscriptionRefresh.Timeout = d
	}
	if d, err := time.ParseDuration(req.HealthCheckTimeout); err == nil && d > 0 {
		c.SubscriptionRefresh.HealthCheckTimeout = d
	}
	if d, err := time.ParseDuration(req.DrainTimeout); err == nil && d > 0 {
		c.SubscriptionRefresh.DrainTimeout = d
	}
	c.SubscriptionRefresh.MinAvailableNodes = req.MinAvailableNodes

	if err := c.SaveSettings(); err != nil {
		return SettingsUpdateResult{}, fmt.Errorf("保存配置失败: %w", err)
	}

	s.cfgMu.Lock()
	s.cfgSrc = c
	s.cfgMu.Unlock()

	if s.subRefresher != nil {
		s.subRefresher.ApplyConfig(c)
	}
	// Per-subscription overrides default to the global interval/timeout, so a
	// change here must be fanned out to every stored subscription.
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

	result := SettingsUpdateResult{
		Saved:   true,
		Applied: []string{"sub_refresh_enabled"},
		Pending: []string{},
		Message: "订阅自动刷新设置已保存并生效",
	}
	if old.SubscriptionRefresh.DrainTimeout != c.SubscriptionRefresh.DrainTimeout || old.SubscriptionRefresh.MinAvailableNodes != c.SubscriptionRefresh.MinAvailableNodes {
		if s.nodeMgr != nil {
			if err := s.nodeMgr.TriggerReload(ctx); err != nil {
				result.NeedReload = true
				result.Pending = append(result.Pending, "runtime_config")
				result.ReloadError = err.Error()
				result.Message = "设置已保存；运行时重载未完成"
			} else {
				result.Applied = append(result.Applied, "runtime_config")
			}
		}
	}
	return result, nil
}
