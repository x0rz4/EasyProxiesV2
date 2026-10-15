package boxmgr

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/geoip"
	"easy_proxies/internal/nodedetect"
	"easy_proxies/internal/nodeloc"
	"easy_proxies/internal/store"
)

// applyCachedNodeLocations makes the persisted landing-IP result authoritative
// before any group topology is built. A successful cached IP is never fetched
// again during startup; if its country was not available previously we only do
// a local GeoIP lookup.
func (m *Manager) applyCachedNodeLocations(ctx context.Context, cfg *config.Config) {
	m.landingMu.Lock()
	defer m.landingMu.Unlock()
	if m.store == nil || cfg == nil {
		return
	}
	lookup := m.openLandingGeoIP(cfg)
	if lookup == nil {
		m.logger.Warnf("GeoIP database unavailable; preserving cached node locations and pausing classification")
	} else {
		defer lookup.Close()
	}
	var classify func(string) geoip.RegionInfo
	if lookup != nil {
		classify = lookup.LookupIP
	}
	m.applyCachedLocations(ctx, cfg, classify)
}

// applyCachedLocations accepts a local classifier so cached observations can
// be reconciled without network access. The caller holds landingMu.
func (m *Manager) applyCachedLocations(ctx context.Context, cfg *config.Config, classify func(string) geoip.RegionInfo) {
	results, err := m.store.ListNodeDetectionResults(ctx)
	if err != nil {
		m.logger.Warnf("load cached landing IP results: %v", err)
		return
	}
	for index := range cfg.Nodes {
		node := &cfg.Nodes[index]
		if node.ID == 0 {
			continue
		}
		state, err := m.store.GetNodeLocation(ctx, node.ID)
		if err != nil || state.Identity != node.NodeKey() {
			continue
		}
		location := state.Current
		result := results[node.ID]
		// Existing evidence is authoritative. Legacy cache may bootstrap only
		// the original identity; it cannot contribute a second confirmation.
		bootstrap := state.LastProbeID == "" && state.Revision == 1
		reclassify := state.LastProbeID != "" && !state.LastSucceeded
		if hasSuccessfulLandingIP(result) && (bootstrap || reclassify) {
			copyResult := *result
			if nodeloc.Normalize(copyResult.ExitCountryCode) == "" && classify != nil {
				classified := classify(copyResult.ExitIP)
				copyResult.ExitCountryCode, copyResult.ExitCountry = classified.ISOCode, classified.Country
			}
			started := result.ExitIPCheckedAt
			if started.IsZero() {
				started = time.Unix(0, 1).UTC()
			}
			probeID := fmt.Sprintf("cache:%d:%d", node.ID, started.UnixNano())
			if reclassify {
				probeID, started = state.LastProbeID, state.LastStartedAt
			}
			if copyResult.ExitIPCheckedAt.Before(started) {
				copyResult.ExitIPCheckedAt = started
			}
			change, err := store.ApplyLandingResult(ctx, m.store, nodeloc.Observation{
				Token: state.Token, ProbeID: probeID, StartedAt: started, Source: nodeloc.SourceGeoIPExit,
			}, &copyResult)
			if err != nil {
				m.logger.Warnf("apply cached landing location for node %d: %v", node.ID, err)
				continue
			}
			location = change.Location
		}
		node.Region, node.Country = location.ISO, location.Country
	}
}

// detectMissingNodeLocations runs only for nodes without a successful cached
// landing IP. It deliberately does not publish health events or alter routing
// availability, failure counters, or eviction state.
func (m *Manager) detectMissingNodeLocations(ctx context.Context, cfg *config.Config) bool {
	m.landingMu.Lock()
	defer m.landingMu.Unlock()
	if m.store == nil || m.monitorMgr == nil || cfg == nil {
		return false
	}
	results, err := m.store.ListNodeDetectionResults(ctx)
	if err != nil {
		m.logger.Warnf("load landing IP results before detection: %v", err)
		return false
	}
	tags := make(map[int64]string)
	for _, snapshot := range m.monitorMgr.Snapshot() {
		if snapshot.NodeID != 0 {
			tags[snapshot.NodeID] = snapshot.Tag
		}
	}
	type target struct {
		index int
		id    int64
		tag   string
	}
	var targets []target
	for index := range cfg.Nodes {
		node := &cfg.Nodes[index]
		if node.ID == 0 || hasSuccessfulLandingIP(results[node.ID]) {
			continue
		}
		if tag := tags[node.ID]; tag != "" {
			targets = append(targets, target{index: index, id: node.ID, tag: tag})
		}
	}
	if len(targets) == 0 {
		return false
	}

	lookup := m.openLandingGeoIP(cfg)
	if lookup == nil {
		m.logger.Warnf("GeoIP database unavailable; skipping landing IP initialization")
		return false
	}
	defer lookup.Close()
	concurrency := cfg.Management.NodeCheck.QualityConcurrency
	if concurrency <= 0 {
		concurrency = 5
	}
	if concurrency > 10 {
		concurrency = 10
	}
	if concurrency > len(targets) {
		concurrency = len(targets)
	}
	timeout := cfg.Management.NodeCheck.QualityTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	landingURL := strings.TrimSpace(cfg.Management.NodeCheck.LandingIPURL)
	if landingURL == "" {
		landingURL = "https://api.ipify.org"
	}

	type outcome struct {
		target
		region  string
		country string
	}
	jobs := make(chan target)
	done := make(chan outcome, len(targets))
	var wg sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				observation, err := store.BeginLandingProbe(ctx, m.store, item.id, cfg.Nodes[item.index].URI, "")
				if err != nil {
					m.logger.Warnf("begin landing probe for node %d: %v", item.id, err)
					continue
				}
				result := &store.NodeDetectionResult{NodeID: item.id, LatencyStatus: "untested", SpeedStatus: "untested", ExitIPStatus: "failed", ExitIPCheckedAt: time.Now().UTC()}
				dialer, dialErr := m.monitorMgr.DialerFor(item.tag)
				if dialErr != nil {
					result.ExitIPError = dialErr.Error()
				} else {
					requestCtx, cancel := context.WithTimeout(ctx, timeout)
					ip, detectErr := nodedetect.DiscoverExitIP(requestCtx, nodedetect.DialFunc(dialer), landingURL, timeout)
					cancel()
					if detectErr != nil {
						result.ExitIPError = detectErr.Error()
					} else {
						result.ExitIPStatus = "success"
						result.ExitIP = ip
						if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
							result.ExitIPFamily = "ipv4"
						} else {
							result.ExitIPFamily = "ipv6"
						}
						if lookup != nil {
							location := lookup.LookupIP(ip)
							if location.ISOCode != "" {
								result.ExitCountry = location.Country
								result.ExitCountryCode = strings.ToUpper(location.ISOCode)
							}
						}
					}
				}
				result.ExitIPCheckedAt = time.Now().UTC()
				change, err := store.ApplyLandingResult(context.Background(), m.store, observation, result)
				if err != nil {
					m.logger.Warnf("apply landing result for node %d: %v", item.id, err)
					continue
				}
				if change.Applied {
					done <- outcome{target: item, region: change.Location.ISO, country: change.Location.Country}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, item := range targets {
			select {
			case jobs <- item:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	close(done)

	changed := false
	for item := range done {
		node := &cfg.Nodes[item.index]
		if item.region != "" && (node.Region != item.region || node.Country != item.country) {
			node.Region, node.Country = item.region, item.country
			changed = true
		}
		if item.region != "" {
			m.monitorMgr.UpdateNodeLocation(item.id, item.region, item.country)
		}
	}
	m.logger.Infof("landing IP initialization checked %d uncached nodes", len(targets))
	return changed
}

func (m *Manager) openLandingGeoIP(cfg *config.Config) *geoip.Lookup {
	path := ""
	if cfg != nil {
		path = strings.TrimSpace(cfg.GeoIP.DatabasePath)
	}
	if path == "" {
		return nil
	}
	lookup, err := geoip.OpenExisting(path)
	if err != nil {
		m.logger.Warnf("open GeoIP database for landing IP classification: %v", err)
		return nil
	}
	return lookup
}

func hasSuccessfulLandingIP(result *store.NodeDetectionResult) bool {
	return result != nil && result.ExitIPStatus == "success" && strings.TrimSpace(result.ExitIP) != ""
}
