package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"easy_proxies/internal/controlplane"
	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/monitor"
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
	NodeCount                    int                      `json:"node_count"`
	ConcurrencyMode              string                   `json:"concurrency_mode"`
	ConfiguredConcurrency        int                      `json:"configured_concurrency"`
	EffectiveConcurrency         int                      `json:"effective_concurrency"`
	EstimatedStartupWorstCase    string                   `json:"estimated_startup_worst_case"`
	EstimatedStartupWorstSeconds float64                  `json:"estimated_startup_worst_seconds"`
	EstimatedRoutineWorstCase    string                   `json:"estimated_routine_worst_case"`
	EstimatedRoutineWorstSeconds float64                  `json:"estimated_routine_worst_seconds"`
	Converging                   bool                     `json:"converging"`
	InitialPending               int                      `json:"initial_pending"`
	Queued                       int                      `json:"queued"`
	Scheduled                    int                      `json:"scheduled"`
	WaitingForManual             bool                     `json:"waiting_for_manual"`
	Round                        monitor.ProbeRoundStatus `json:"round"`
}

func probeSettingsFrom(settings controlplane.ProbeSettings) probeSettingsResponse {
	return probeSettingsResponse{
		ProbeTarget:               settings.ProbeTarget,
		StartupAvailabilityPolicy: settings.StartupAvailabilityPolicy,
		HealthCheckInterval:       settings.HealthCheckInterval.String(),
		ProbeConcurrency:          settings.ProbeConcurrency,
		StartupProbeTimeout:       settings.StartupProbeTimeout.String(),
		RoutineProbeTimeout:       settings.RoutineProbeTimeout.String(),
		ProbeDialTimeout:          settings.ProbeDialTimeout.String(),
		ProbeResponseTimeout:      settings.ProbeResponseTimeout.String(),
		RoutineProbeRetries:       settings.RoutineProbeRetries,
	}
}

// probeSettingsTo parses the wire form. Every duration must be strictly
// positive, which is why parsing lives here rather than in the service: the
// message names the offending field for the form.
func probeSettingsTo(req probeSettingsResponse) (controlplane.ProbeSettings, error) {
	startupTimeout, err := parsePositiveDuration("启动探测超时", req.StartupProbeTimeout)
	if err != nil {
		return controlplane.ProbeSettings{}, err
	}
	routineTimeout, err := parsePositiveDuration("周期/手动探测超时", req.RoutineProbeTimeout)
	if err != nil {
		return controlplane.ProbeSettings{}, err
	}
	dialTimeout, err := parsePositiveDuration("拨号超时", req.ProbeDialTimeout)
	if err != nil {
		return controlplane.ProbeSettings{}, err
	}
	responseTimeout, err := parsePositiveDuration("响应超时", req.ProbeResponseTimeout)
	if err != nil {
		return controlplane.ProbeSettings{}, err
	}
	healthInterval, err := parsePositiveDuration("健康检查间隔", req.HealthCheckInterval)
	if err != nil {
		return controlplane.ProbeSettings{}, err
	}
	return controlplane.ProbeSettings{
		ProbeTarget:               req.ProbeTarget,
		StartupAvailabilityPolicy: req.StartupAvailabilityPolicy,
		HealthCheckInterval:       healthInterval,
		ProbeConcurrency:          req.ProbeConcurrency,
		StartupProbeTimeout:       startupTimeout,
		RoutineProbeTimeout:       routineTimeout,
		ProbeDialTimeout:          dialTimeout,
		ProbeResponseTimeout:      responseTimeout,
		RoutineProbeRetries:       req.RoutineProbeRetries,
	}, nil
}

func parsePositiveDuration(name, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s必须是大于 0 的 Go 时长（例如 5s、2m）", name)
	}
	return duration, nil
}

func (s *Server) handleProbeSettingsGet(w http.ResponseWriter, r *http.Request) {
	settings, err := s.settingsService().ProbeSettings()
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "配置存储未初始化")
		return
	}
	writeJSON(w, probeSettingsFrom(settings))
}

func (s *Server) handleProbeSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	var request probeSettingsResponse
	if err := decodeJSON(r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	parsed, err := probeSettingsTo(request)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.settingsService().UpdateProbeSettings(r.Context(), parsed)
	if err != nil {
		var validation controlplane.ValidationError
		switch {
		case errors.As(err, &validation):
			writeAPIError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, controlplane.ErrConfigUnavailable):
			writeAPIError(w, http.StatusServiceUnavailable, "配置存储未初始化")
		default:
			writeAPIError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, probeSettingsFrom(updated))
}

func (s *Server) handleProbeStatus(w http.ResponseWriter, r *http.Request) {
	policy := s.mgr.ProbePolicy()
	nodeCount := s.mgr.ProbeNodeCount()
	effective := monitor.EffectiveProbeConcurrency(policy.Concurrency, nodeCount)
	rounds := 0
	if effective > 0 {
		rounds = (nodeCount + effective - 1) / effective
	}
	startupEstimate := time.Duration(rounds) * (2*policy.StartupTimeout + monitor.InitialProbeRetryDelay)
	routineEstimate := time.Duration(rounds) * policy.RoutineTimeout
	initial := s.mgr.InitialProbeStatus()
	mode := "fixed"
	if policy.Concurrency == 0 {
		mode = "auto"
	}
	round := s.mgr.ProbeRoundStatus()
	scheduled := 0
	if round.InFlight && round.Kind == monitor.ProbeRoundStartup {
		scheduled = max(0, round.Total-round.Completed)
	}
	writeJSON(w, probeStatusResponse{
		NodeCount: nodeCount, ConcurrencyMode: mode,
		ConfiguredConcurrency: policy.Concurrency, EffectiveConcurrency: effective,
		EstimatedStartupWorstCase: startupEstimate.String(), EstimatedStartupWorstSeconds: startupEstimate.Seconds(),
		EstimatedRoutineWorstCase: routineEstimate.String(), EstimatedRoutineWorstSeconds: routineEstimate.Seconds(),
		Converging: initial.Converging, InitialPending: initial.Pending, Queued: initial.Queued,
		Scheduled: scheduled, WaitingForManual: initial.Converging && round.InFlight && round.Kind == monitor.ProbeRoundManual,
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

func allSettingsResponseOf(settings controlplane.AllSettings) allSettingsResponse {
	return allSettingsResponse{
		LogLevel:       settings.LogLevel,
		ExternalIP:     settings.ExternalIP,
		SkipCertVerify: settings.SkipCertVerify,

		ListenerEnabled:  settings.ListenerEnabled,
		ListenerAddress:  settings.ListenerAddress,
		ListenerPort:     settings.ListenerPort,
		ListenerProtocol: settings.ListenerProtocol,
		ListenerUsername: settings.ListenerUsername,
		ListenerPassword: settings.ListenerPassword,

		MultiPortEnabled:  settings.MultiPortEnabled,
		MultiPortAddress:  settings.MultiPortAddress,
		MultiPortBasePort: settings.MultiPortBasePort,
		MultiPortProtocol: settings.MultiPortProtocol,
		MultiPortUsername: settings.MultiPortUsername,
		MultiPortPassword: settings.MultiPortPassword,

		PoolMode:              settings.PoolMode,
		PoolFailureThreshold:  settings.PoolFailureThreshold,
		PoolBlacklistDuration: settings.PoolBlacklistDuration,

		ManagementEnabled:                   settings.ManagementEnabled,
		ManagementListen:                    settings.ManagementListen,
		ManagementProbeTarget:               settings.ManagementProbeTarget,
		ManagementStartupAvailabilityPolicy: settings.ManagementStartupAvailabilityPolicy,
		ManagementPassword:                  settings.ManagementPassword,
		ManagementHealthCheckInterval:       settings.ManagementHealthCheckInterval,

		GeoIPEnabled:            settings.GeoIPEnabled,
		GeoIPDatabasePath:       settings.GeoIPDatabasePath,
		GeoIPAutoUpdateEnabled:  settings.GeoIPAutoUpdateEnabled,
		GeoIPAutoUpdateInterval: settings.GeoIPAutoUpdateInterval,

		Subscriptions: settings.Subscriptions,
	}
}

func allSettingsRequestOf(req allSettingsRequest) controlplane.AllSettingsRequest {
	return controlplane.AllSettingsRequest{
		LogLevel:       req.LogLevel,
		ExternalIP:     req.ExternalIP,
		SkipCertVerify: req.SkipCertVerify,

		ListenerEnabled:  req.ListenerEnabled,
		ListenerAddress:  req.ListenerAddress,
		ListenerPort:     req.ListenerPort,
		ListenerProtocol: req.ListenerProtocol,
		ListenerUsername: req.ListenerUsername,
		ListenerPassword: req.ListenerPassword,

		MultiPortEnabled:  req.MultiPortEnabled,
		MultiPortAddress:  req.MultiPortAddress,
		MultiPortBasePort: req.MultiPortBasePort,
		MultiPortProtocol: req.MultiPortProtocol,
		MultiPortUsername: req.MultiPortUsername,
		MultiPortPassword: req.MultiPortPassword,

		PoolMode:              req.PoolMode,
		PoolFailureThreshold:  req.PoolFailureThreshold,
		PoolBlacklistDuration: req.PoolBlacklistDuration,

		ManagementEnabled:                   req.ManagementEnabled,
		ManagementListen:                    req.ManagementListen,
		ManagementProbeTarget:               req.ManagementProbeTarget,
		ManagementStartupAvailabilityPolicy: req.ManagementStartupAvailabilityPolicy,
		ManagementPassword:                  req.ManagementPassword,
		ManagementHealthCheckInterval:       req.ManagementHealthCheckInterval,

		GeoIPEnabled:            req.GeoIPEnabled,
		GeoIPDatabasePath:       req.GeoIPDatabasePath,
		GeoIPAutoUpdateEnabled:  req.GeoIPAutoUpdateEnabled,
		GeoIPAutoUpdateInterval: req.GeoIPAutoUpdateInterval,
	}
}

func (s *Server) getAllSettings() allSettingsResponse {
	settings, err := s.settingsService().AllSettings()
	if err != nil {
		return allSettingsResponse{}
	}
	return allSettingsResponseOf(settings)
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

	result, err := s.settingsService().UpdateAllSettings(r.Context(), allSettingsRequestOf(req))
	if err != nil {
		status := http.StatusInternalServerError
		var validationErr controlplane.ValidationError
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

func subscriptionRefreshSettingsFrom(settings controlplane.SubscriptionRefreshSettings) subscriptionRefreshSettingsResponse {
	return subscriptionRefreshSettingsResponse{
		Enabled:            settings.Enabled,
		Interval:           settings.Interval.String(),
		Timeout:            settings.Timeout.String(),
		HealthCheckTimeout: settings.HealthCheckTimeout.String(),
		DrainTimeout:       settings.DrainTimeout.String(),
		MinAvailableNodes:  settings.MinAvailableNodes,
	}
}

// subscriptionRefreshSettingsTo parses the wire form. Empty or non-positive
// durations are passed through as zero, which the service treats as "keep the
// stored value" — matching the generic settings form.
func subscriptionRefreshSettingsTo(req subscriptionRefreshSettingsRequest) (controlplane.SubscriptionRefreshSettings, error) {
	parse := func(name, value string) (time.Duration, error) {
		value = strings.TrimSpace(value)
		if value == "" {
			return 0, nil
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("%s必须是大于 0 的 Go 时长（例如 5s、2m）", name)
		}
		return d, nil
	}
	interval, err := parse("刷新间隔", req.Interval)
	if err != nil {
		return controlplane.SubscriptionRefreshSettings{}, err
	}
	timeout, err := parse("刷新超时", req.Timeout)
	if err != nil {
		return controlplane.SubscriptionRefreshSettings{}, err
	}
	healthTimeout, err := parse("健康检查超时", req.HealthCheckTimeout)
	if err != nil {
		return controlplane.SubscriptionRefreshSettings{}, err
	}
	drainTimeout, err := parse("排空超时", req.DrainTimeout)
	if err != nil {
		return controlplane.SubscriptionRefreshSettings{}, err
	}
	return controlplane.SubscriptionRefreshSettings{
		Enabled:            req.Enabled,
		Interval:           interval,
		Timeout:            timeout,
		HealthCheckTimeout: healthTimeout,
		DrainTimeout:       drainTimeout,
		MinAvailableNodes:  req.MinAvailableNodes,
	}, nil
}

// The subscription auto-refresh surface is the single writer of
// config.SubscriptionRefresh, so the generic /api/settings form can never
// overwrite it with stale values.
func (s *Server) handleSubscriptionRefreshSettingsGet(w http.ResponseWriter, r *http.Request) {
	settings, err := s.settingsService().SubscriptionRefreshSettings()
	if err != nil {
		writeJSON(w, subscriptionRefreshSettingsResponse{})
		return
	}
	writeJSON(w, subscriptionRefreshSettingsFrom(settings))
}

func (s *Server) handleSubscriptionRefreshSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	var req subscriptionRefreshSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	parsed, err := subscriptionRefreshSettingsTo(req)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.settingsService().UpdateSubscriptionRefreshSettings(r.Context(), parsed)
	if err != nil {
		status := http.StatusInternalServerError
		var validationErr controlplane.ValidationError
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
	settings, err := s.settingsService().SubscriptionRefreshSettings()
	if err != nil {
		return subscriptionRefreshSettingsResponse{}
	}
	return subscriptionRefreshSettingsFrom(settings)
}
