package builder

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/geoip"
	groupruntime "easy_proxies/internal/group"
	"easy_proxies/internal/groupmember"
	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/nodecodec"
	poolout "easy_proxies/internal/outbound/pool"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/json/badoption"
)

// Build converts high level config into sing-box Options tree.
func Build(cfg *config.Config) (option.Options, error) {
	baseOutbounds := make([]option.Outbound, 0, len(cfg.Nodes))
	memberTags := make([]string, 0, len(cfg.Nodes))
	metadata := make(map[string]poolout.MemberMeta)
	nodeTagsByID := make(map[int64]string)
	// nodeTagNamesByID carries the node's own tag names (the nodes.tags
	// projection), which group membership matches against. It is kept out of
	// poolout.MemberMeta on purpose: that struct is serialized into subscription
	// output, and tags are an internal classification.
	nodeTagNamesByID := make(map[int64][]string)
	var failedNodes []string
	usedTags := make(map[string]int) // Track tag usage for uniqueness

	// Track nodes by region for GeoIP routing.
	// Region codes are open-ended (any lowercased ISO country code can appear),
	// so this map is grown on demand instead of pre-seeded with a fixed set.
	regionMembers := make(map[string][]string)

	for _, node := range cfg.Nodes {
		baseTag := sanitizeTag(node.Name)
		if baseTag == "" {
			baseTag = fmt.Sprintf("node-%d", len(memberTags)+1)
		}

		// Ensure tag uniqueness by appending a counter if needed
		tag := baseTag
		if count, exists := usedTags[baseTag]; exists {
			usedTags[baseTag] = count + 1
			tag = fmt.Sprintf("%s-%d", baseTag, count+1)
		} else {
			usedTags[baseTag] = 1
		}

		outbound, err := buildNodeOutbound(tag, node.URI, cfg.SkipCertVerify)
		if err != nil {
			log.Printf("❌ Failed to build node '%s': %v (skipping)", node.Name, err)
			failedNodes = append(failedNodes, node.Name)
			continue
		}
		memberTags = append(memberTags, tag)
		baseOutbounds = append(baseOutbounds, outbound)
		meta := poolout.MemberMeta{
			NodeID: node.ID,
			Name:   node.Name,
			URI:    node.URI,
			Mode:   cfg.EntryMode(),
		}
		// Prefer the concrete per-node entry when it exists; otherwise expose the
		// shared pool entry. Disabled entries intentionally have no listen address.
		if cfg.MultiPort.Enabled {
			meta.ListenAddress = cfg.MultiPort.Address
			meta.Port = node.Port
		} else if cfg.Listener.Enabled {
			meta.ListenAddress = cfg.Listener.Address
			meta.Port = cfg.Listener.Port
		}

		// Region is authoritative only after the node has been dialed and its
		// public landing IP has been classified. Never infer it from the proxy
		// server address in the URI: relay/CDN endpoints may exit elsewhere.
		if node.Region != "" {
			meta.Region = strings.ToLower(node.Region)
			meta.Country = node.Country
			regionMembers[meta.Region] = append(regionMembers[meta.Region], tag)
		} else {
			meta.Region = geoip.RegionOther
			meta.Country = "Unknown"
			regionMembers[geoip.RegionOther] = append(regionMembers[geoip.RegionOther], tag)
		}

		metadata[tag] = meta
		if node.ID != 0 {
			nodeTagsByID[node.ID] = tag
			if len(node.Tags) > 0 {
				nodeTagNamesByID[node.ID] = node.Tags
			}
		}
	}

	// Check if we have at least one valid node
	if len(baseOutbounds) == 0 {
		return option.Options{}, fmt.Errorf("no valid nodes available (all %d nodes failed to build)", len(cfg.Nodes))
	}

	// Log summary
	if len(failedNodes) > 0 {
		log.Printf("⚠️  %d/%d nodes failed and were skipped: %v", len(failedNodes), len(cfg.Nodes), failedNodes)
	}
	log.Printf("✅ Successfully built %d/%d nodes", len(baseOutbounds), len(cfg.Nodes))

	// Log GeoIP region distribution
	if cfg.GeoIP.Enabled {
		// Stable, friendly ordering: well-known regions first, then any others
		// (e.g. sg, de, gb, ...) sorted alphabetically.
		ordered := orderedRegions(regionMembers)
		log.Println("🌍 GeoIP Region Distribution:")
		for _, region := range ordered {
			count := len(regionMembers[region])
			if count > 0 {
				log.Printf("   %s %s: %d nodes", geoip.RegionEmoji(region), geoip.RegionName(region), count)
			}
		}
	}

	// Print proxy links for each node
	printProxyLinks(cfg, metadata)

	var (
		inbounds  []option.Inbound
		outbounds = make([]option.Outbound, len(baseOutbounds))
		route     option.RouteOptions
	)
	copy(outbounds, baseOutbounds)

	// The shared pool and per-node listeners are independent and may both be disabled.
	// Keep one shared pool without an inbound when both entry types are disabled so
	// node monitoring and health checks continue to run. A multi-port-only runtime
	// already registers every node through its per-node pools.
	enablePoolInbound := cfg.Listener.Enabled
	enableMultiPort := cfg.MultiPort.Enabled
	buildSharedPool := enablePoolInbound || !enableMultiPort

	// Build the optional shared pool inbound (single entry point for all nodes).
	if enablePoolInbound {
		inbound, err := buildPoolInbound(cfg)
		if err != nil {
			return option.Options{}, err
		}
		inbounds = append(inbounds, inbound)
	}
	if buildSharedPool {
		poolOptions := poolout.Options{
			Mode:              cfg.Pool.Mode,
			Members:           memberTags,
			FailureThreshold:  cfg.Pool.FailureThreshold,
			BlacklistDuration: cfg.Pool.BlacklistDuration,
			Metadata:          metadata,
		}
		outbounds = append(outbounds, option.Outbound{
			Type:    poolout.Type,
			Tag:     poolout.Tag,
			Options: &poolOptions,
		})
		route.Final = poolout.Tag
	}

	// Build multi-port inbounds (one port per node)
	if enableMultiPort {
		addr, err := parseAddr(cfg.MultiPort.Address)
		if err != nil {
			return option.Options{}, fmt.Errorf("parse multi-port address: %w", err)
		}
		for _, tag := range memberTags {
			meta := metadata[tag]
			perMeta := map[string]poolout.MemberMeta{tag: meta}
			poolTag := fmt.Sprintf("%s-%s", poolout.Tag, tag)
			perOptions := poolout.Options{
				Mode:              "sequential",
				Members:           []string{tag},
				FailureThreshold:  cfg.Pool.FailureThreshold,
				BlacklistDuration: cfg.Pool.BlacklistDuration,
				Metadata:          perMeta,
			}
			perPool := option.Outbound{
				Type:    poolout.Type,
				Tag:     poolTag,
				Options: &perOptions,
			}
			outbounds = append(outbounds, perPool)
			inboundTag := fmt.Sprintf("in-%s", tag)
			inbound, err := buildInboundByProtocol(
				cfg.MultiPort.Protocol,
				addr,
				meta.Port,
				cfg.MultiPort.Username,
				cfg.MultiPort.Password,
				inboundTag,
			)
			if err != nil {
				return option.Options{}, fmt.Errorf("build multi-port inbound for %s: %w", tag, err)
			}
			inbounds = append(inbounds, inbound)
			route.Rules = append(route.Rules, option.Rule{
				Type: C.RuleTypeDefault,
				DefaultOptions: option.DefaultRule{
					RawDefaultRule: option.RawDefaultRule{
						Inbound: badoption.Listable[string]{inboundTag},
					},
					RuleAction: option.RuleAction{
						Action: C.RuleActionTypeRoute,
						RouteOptions: option.RouteActionOptions{
							Outbound: poolTag,
						},
					},
				},
			})
		}
	}

	// Build one independent inbound/outbound pair for each enabled group. Group
	// membership is re-evaluated on every reload, so newly subscribed nodes join
	// automatically when their region or explicit node ID matches.
	for _, group := range cfg.Groups {
		if !group.Enabled || group.ID == 0 || group.BindPort == 0 {
			continue
		}
		// memberTags order decides members[0], which is this group's default
		// outbound, so membership is asked as a per-node question rather than
		// obtained as a ready-made list.
		filter := groupmember.NewFilter(group, groupmember.WithTagNames(cfg.TagNames))
		members := make([]string, 0)
		groupMeta := make(map[string]poolout.MemberMeta)
		for _, tag := range memberTags {
			meta := metadata[tag]
			if !filter.Allow(groupmember.Node{ID: meta.NodeID, Region: meta.Region,
				Tags: nodeTagNamesByID[meta.NodeID]}) {
				continue
			}
			members = append(members, tag)
			groupMeta[tag] = meta
		}
		if len(members) == 0 {
			log.Printf("⚠️  Group %q has no matching nodes; listener %d is inactive", group.Name, group.BindPort)
			continue
		}

		// InitialGroupState must describe the final member topology exactly.
		// Persisted states can outlive a membership rule change; feeding those
		// stale entries to group.Register would recreate excluded members in the
		// runtime snapshot even though they are absent from the actual pool.
		stateByNodeID := make(map[int64]config.GroupNodeStateConfig, len(group.NodeStates))
		for _, state := range group.NodeStates {
			stateByNodeID[state.NodeID] = state
		}
		stateByTag := make(map[string]groupruntime.GroupInitialState, len(members))
		for _, tag := range members {
			nodeID := metadata[tag].NodeID
			if state, ok := stateByNodeID[nodeID]; ok {
				stateByTag[tag] = groupruntime.GroupInitialState{NodeID: nodeID,
					FailureHistory: append([]int64(nil), state.FailureHistory...), Evicted: state.Evicted,
					LastError: state.LastError, EvictedAt: state.EvictedAt}
			} else {
				stateByTag[tag] = groupruntime.GroupInitialState{NodeID: nodeID}
			}
		}
		preferredTag := nodeTagsByID[group.CurrentActiveNodeID]
		if preferredTag == "" && group.DispatchMode != "lowest_latency" {
			preferredTag = members[0]
		}
		selectorDefault := preferredTag
		if selectorDefault == "" {
			selectorDefault = members[0]
		}
		selectorTag := fmt.Sprintf("group-selector-%d", group.ID)
		groupOutboundTag := fmt.Sprintf("group-pool-%d", group.ID)
		groupInboundTag := fmt.Sprintf("group-in-%d", group.ID)
		outbounds = append(outbounds, option.Outbound{Type: C.TypeSelector, Tag: selectorTag,
			Options: &option.SelectorOutboundOptions{Outbounds: members, Default: selectorDefault, InterruptExistConnections: false}})
		groupOptions := poolout.Options{Mode: group.DispatchMode, Members: members,
			FailureThreshold: group.FailureThreshold, FailureWindow: group.FailureWindow,
			HealthCheckInterval: group.HealthCheckInterval,
			BlacklistDuration:   100 * 365 * 24 * time.Hour, Metadata: groupMeta,
			GroupID: group.ID, PreferredMember: preferredTag, InitialGroupState: stateByTag, SelectorTag: selectorTag}
		outbounds = append(outbounds, option.Outbound{Type: poolout.Type, Tag: groupOutboundTag, Options: &groupOptions})

		addr, err := parseAddr(group.BindAddress)
		if err != nil {
			return option.Options{}, fmt.Errorf("parse group %q address: %w", group.Name, err)
		}
		inbound, err := buildInboundByProtocol(group.Protocol, addr, group.BindPort,
			group.Username, group.Password, groupInboundTag)
		if err != nil {
			return option.Options{}, fmt.Errorf("build group %q inbound: %w", group.Name, err)
		}
		inbounds = append(inbounds, inbound)
		route.Rules = append(route.Rules, option.Rule{Type: C.RuleTypeDefault,
			DefaultOptions: option.DefaultRule{RawDefaultRule: option.RawDefaultRule{
				Inbound: badoption.Listable[string]{groupInboundTag}}, RuleAction: option.RuleAction{
				Action: C.RuleActionTypeRoute, RouteOptions: option.RouteActionOptions{Outbound: groupOutboundTag}}}})
		log.Printf("🧩 Group %q listening on %s:%d with %d members (%s)", group.Name, group.BindAddress, group.BindPort, len(members), group.DispatchMode)
	}

	// Build GeoIP region-based pool outbounds and routing
	if cfg.GeoIP.Enabled && enablePoolInbound {
		// Create pool outbound for each region that has nodes, in a stable,
		// friendly order (well-known regions first, others alphabetical).
		for _, region := range orderedRegions(regionMembers) {
			members := regionMembers[region]
			if len(members) == 0 {
				continue
			}

			// Build metadata for this region's members
			regionMeta := make(map[string]poolout.MemberMeta)
			for _, tag := range members {
				regionMeta[tag] = metadata[tag]
			}

			regionPoolTag := fmt.Sprintf("pool-%s", region)
			regionPoolOptions := poolout.Options{
				Mode:              cfg.Pool.Mode,
				Members:           members,
				FailureThreshold:  cfg.Pool.FailureThreshold,
				BlacklistDuration: cfg.Pool.BlacklistDuration,
				Metadata:          regionMeta,
			}
			outbounds = append(outbounds, option.Outbound{
				Type:    poolout.Type,
				Tag:     regionPoolTag,
				Options: &regionPoolOptions,
			})
		}

		// Log GeoIP routing info
		geoipPort := cfg.GeoIP.Port
		if geoipPort == 0 {
			geoipPort = cfg.Listener.Port
		}
		geoipListen := cfg.GeoIP.Listen
		if geoipListen == "" {
			geoipListen = cfg.Listener.Address
		}
		log.Println("🌐 GeoIP Region Routing Enabled:")
		log.Printf("   Access via: http://%s:%d/{region}", geoipListen, geoipPort)
		// List the regions that actually have nodes.
		active := orderedRegions(regionMembers)
		regionList := make([]string, 0, len(active))
		for _, r := range active {
			if len(regionMembers[r]) > 0 {
				regionList = append(regionList, "/"+r)
			}
		}
		if len(regionList) > 0 {
			log.Printf("   Available regions: %s", strings.Join(regionList, ", "))
		}
		log.Println("   Default (no path): all nodes pool")
	}

	opts := option.Options{
		Log: &option.LogOptions{Level: strings.ToLower(cfg.LogLevel)},
		DNS: &option.DNSOptions{RawDNSOptions: option.RawDNSOptions{
			Servers: []option.DNSServerOptions{{
				Type:    C.DNSTypeLocal,
				Tag:     "local",
				Options: &option.LocalDNSServerOptions{},
			}},
			Final: "local",
		}},
		Inbounds:  inbounds,
		Outbounds: outbounds,
		Route:     &route,
	}
	// sing-box 1.13 constructs outbound resolve dialers before installing its
	// implicit local DNS fallback. Without an explicit resolver, domain-based
	// proxy servers can capture a nil DNS transport and panic during probes.
	opts.Route.DefaultDomainResolver = &option.DomainResolveOptions{Server: "local"}
	return opts, nil
}

// BuildBase builds the application-wide runtime without any group listeners.
// Groups are hosted by independent boxes so mutating one group never requires
// rebinding the global listener or another group's listener.
func BuildBase(cfg *config.Config) (option.Options, error) {
	baseCfg := cfg.Clone()
	baseCfg.Groups = nil
	return Build(baseCfg)
}

// BuildGroup builds a self-contained sing-box instance for one enabled group.
// It retains only the target group's inbound, selector, pool, and the member
// outbounds on which that pool depends.
func BuildGroup(cfg *config.Config, groupID int64) (option.Options, error) {
	groupCfg := cfg.Clone()
	groupCfg.Listener.Enabled = false
	groupCfg.MultiPort.Enabled = false
	groupCfg.Groups = nil
	for _, candidate := range cfg.Groups {
		if candidate.ID == groupID {
			groupCfg.Groups = []config.GroupPoolConfig{candidate}
			break
		}
	}
	if len(groupCfg.Groups) != 1 || !groupCfg.Groups[0].Enabled {
		return option.Options{}, fmt.Errorf("group %d is not enabled", groupID)
	}
	opts, err := Build(groupCfg)
	if err != nil {
		return option.Options{}, err
	}
	poolTag := fmt.Sprintf("group-pool-%d", groupID)
	selectorTag := fmt.Sprintf("group-selector-%d", groupID)
	inboundTag := fmt.Sprintf("group-in-%d", groupID)
	keep := map[string]struct{}{poolTag: {}, selectorTag: {}}
	foundPool := false
	for _, outbound := range opts.Outbounds {
		if outbound.Tag != poolTag {
			continue
		}
		poolOptions, ok := outbound.Options.(*poolout.Options)
		if !ok {
			return option.Options{}, fmt.Errorf("group %d has invalid pool options", groupID)
		}
		poolOptions.MonitorObserverOnly = true
		for _, memberTag := range poolOptions.Members {
			keep[memberTag] = struct{}{}
		}
		foundPool = true
		break
	}
	if !foundPool {
		return option.Options{}, fmt.Errorf("group %d has no routable members", groupID)
	}
	filteredOutbounds := opts.Outbounds[:0]
	for _, outbound := range opts.Outbounds {
		if _, ok := keep[outbound.Tag]; ok {
			filteredOutbounds = append(filteredOutbounds, outbound)
		}
	}
	filteredInbounds := opts.Inbounds[:0]
	for _, inbound := range opts.Inbounds {
		if inbound.Tag == inboundTag {
			filteredInbounds = append(filteredInbounds, inbound)
		}
	}
	if len(filteredInbounds) != 1 {
		return option.Options{}, fmt.Errorf("group %d listener was not built", groupID)
	}
	opts.Inbounds = filteredInbounds
	opts.Outbounds = filteredOutbounds
	opts.Route = &option.RouteOptions{
		Final:                 poolTag,
		DefaultDomainResolver: &option.DomainResolveOptions{Server: "local"},
	}
	opts.Experimental = nil
	opts.Services = nil
	return opts, nil
}

func buildPoolInbound(cfg *config.Config) (option.Inbound, error) {
	listenAddr, err := parseAddr(cfg.Listener.Address)
	if err != nil {
		return option.Inbound{}, fmt.Errorf("parse listener address: %w", err)
	}
	return buildInboundByProtocol(
		cfg.Listener.Protocol,
		listenAddr,
		cfg.Listener.Port,
		cfg.Listener.Username,
		cfg.Listener.Password,
		"http-in",
	)
}

func buildInboundByProtocol(protocol string, listenAddr *badoption.Addr, port uint16, username, password, tag string) (option.Inbound, error) {
	users := []auth.User(nil)
	if username != "" {
		users = []auth.User{{Username: username, Password: password}}
	}

	switch protocol {
	case config.InboundProtocolHTTP:
		opts := &option.HTTPMixedInboundOptions{
			ListenOptions: option.ListenOptions{
				Listen:     listenAddr,
				ListenPort: port,
			},
		}
		if len(users) > 0 {
			opts.Users = users
		}
		return option.Inbound{Type: C.TypeHTTP, Tag: tag, Options: opts}, nil
	case config.InboundProtocolSOCKS5:
		opts := &option.SocksInboundOptions{
			ListenOptions: option.ListenOptions{
				Listen:     listenAddr,
				ListenPort: port,
			},
		}
		if len(users) > 0 {
			opts.Users = users
		}
		return option.Inbound{Type: C.TypeSOCKS, Tag: tag, Options: opts}, nil
	case config.InboundProtocolMixed:
		opts := &option.HTTPMixedInboundOptions{
			ListenOptions: option.ListenOptions{
				Listen:     listenAddr,
				ListenPort: port,
			},
		}
		if len(users) > 0 {
			opts.Users = users
		}
		return option.Inbound{Type: C.TypeMixed, Tag: tag, Options: opts}, nil
	default:
		return option.Inbound{}, fmt.Errorf("unsupported inbound protocol %q", protocol)
	}
}

func buildNodeOutbound(tag, rawURI string, skipCertVerify bool) (option.Outbound, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return option.Outbound{}, fmt.Errorf("parse uri: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		opts, err := buildHTTPOptions(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeHTTP, Tag: tag, Options: &opts}, nil
	case "socks5":
		opts, err := buildSOCKSOptions(parsed)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeSOCKS, Tag: tag, Options: &opts}, nil
	case "vless":
		opts, err := buildVLESSOptions(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeVLESS, Tag: tag, Options: &opts}, nil
	case "hysteria2", "hy2":
		opts, err := buildHysteria2Options(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeHysteria2, Tag: tag, Options: &opts}, nil
	case "ss", "shadowsocks":
		opts, err := buildShadowsocksOptions(parsed)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeShadowsocks, Tag: tag, Options: &opts}, nil
	case "trojan":
		opts, err := buildTrojanOptions(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeTrojan, Tag: tag, Options: &opts}, nil
	case "vmess":
		opts, err := buildVMessOptions(rawURI, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeVMess, Tag: tag, Options: &opts}, nil
	case "anytls":
		opts, err := buildAnyTLSOptions(parsed, skipCertVerify)
		if err != nil {
			return option.Outbound{}, err
		}
		return option.Outbound{Type: C.TypeAnyTLS, Tag: tag, Options: &opts}, nil
	default:
		return option.Outbound{}, fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
}

func buildHTTPOptions(u *url.URL, skipCertVerify bool) (option.HTTPOutboundOptions, error) {
	query := u.Query()
	defaultPort := 80
	if strings.EqualFold(query.Get("security"), "tls") {
		defaultPort = 443
	}
	server, port, err := hostPort(u, defaultPort)
	if err != nil {
		return option.HTTPOutboundOptions{}, err
	}

	opts := option.HTTPOutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
	}
	if u.User != nil {
		opts.Username = u.User.Username()
		if password, ok := u.User.Password(); ok {
			opts.Password = password
		}
	}
	if path := u.EscapedPath(); path != "" {
		opts.Path = path
	}
	if host := strings.TrimSpace(query.Get("host")); host != "" {
		opts.Headers = badoption.HTTPHeader{"Host": {host}}
	}
	if tlsOptions, err := buildTLSOptions(query, skipCertVerify); err != nil {
		return option.HTTPOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	return opts, nil
}

func buildSOCKSOptions(u *url.URL) (option.SOCKSOutboundOptions, error) {
	server, port, err := hostPort(u, 1080)
	if err != nil {
		return option.SOCKSOutboundOptions{}, err
	}

	opts := option.SOCKSOutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Version:       "5",
	}
	if u.User != nil {
		opts.Username = u.User.Username()
		if password, ok := u.User.Password(); ok {
			opts.Password = password
		}
	}
	if network := strings.ToLower(strings.TrimSpace(u.Query().Get("network"))); network != "" {
		opts.Network = option.NetworkList(network)
	}

	return opts, nil
}

func buildAnyTLSOptions(u *url.URL, skipCertVerify bool) (option.AnyTLSOutboundOptions, error) {
	password := ""
	if u.User != nil {
		password = u.User.Username()
	}
	if password == "" {
		return option.AnyTLSOutboundOptions{}, errors.New("anytls uri missing password in userinfo")
	}
	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.AnyTLSOutboundOptions{}, err
	}
	query := u.Query()
	opts := option.AnyTLSOutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Password:      password,
	}

	// AnyTLS requires TLS
	tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}
	tlsOptions.ServerName = server
	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	insecure := query.Get("insecure")
	if insecure == "" {
		insecure = query.Get("allowInsecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}
	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}
	if fp := query.Get("fp"); fp != "" {
		tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
	}
	opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}

	return opts, nil
}

func buildVLESSOptions(u *url.URL, skipCertVerify bool) (option.VLESSOutboundOptions, error) {
	uuid := u.User.Username()
	if uuid == "" {
		return option.VLESSOutboundOptions{}, errors.New("vless uri missing uuid in userinfo")
	}
	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.VLESSOutboundOptions{}, err
	}
	query := u.Query()
	opts := option.VLESSOutboundOptions{
		UUID:          uuid,
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
	}
	if flow := query.Get("flow"); flow != "" {
		opts.Flow = flow
	}
	if packetEncoding := query.Get("packetEncoding"); packetEncoding != "" {
		opts.PacketEncoding = &packetEncoding
	}
	if transport, err := buildV2RayTransport(query); err != nil {
		return option.VLESSOutboundOptions{}, err
	} else if transport != nil {
		opts.Transport = transport
	}
	if tlsOptions, err := buildTLSOptions(query, skipCertVerify); err != nil {
		return option.VLESSOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}
	return opts, nil
}

func buildHysteria2Options(u *url.URL, skipCertVerify bool) (option.Hysteria2OutboundOptions, error) {
	password := u.User.String()
	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.Hysteria2OutboundOptions{}, err
	}
	query := u.Query()
	opts := option.Hysteria2OutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Password:      password,
	}
	if up := query.Get("upMbps"); up != "" {
		opts.UpMbps = atoiDefault(up)
	}
	if down := query.Get("downMbps"); down != "" {
		opts.DownMbps = atoiDefault(down)
	}
	if obfs := query.Get("obfs"); obfs != "" {
		opts.Obfs = &option.Hysteria2Obfs{Type: obfs, Password: query.Get("obfs-password")}
	}
	opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: hysteriaTLSOptions(server, query, skipCertVerify)}
	return opts, nil
}

func hysteriaTLSOptions(host string, query url.Values, skipCertVerify bool) *option.OutboundTLSOptions {
	tlsOptions := &option.OutboundTLSOptions{
		Enabled:    true,
		ServerName: host,
		Insecure:   skipCertVerify,
	}
	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	insecure := query.Get("insecure")
	if insecure == "" {
		insecure = query.Get("allowInsecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}
	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}
	return tlsOptions
}

func buildTLSOptions(query url.Values, skipCertVerify bool) (*option.OutboundTLSOptions, error) {
	security := strings.ToLower(query.Get("security"))
	if security == "" || security == "none" {
		return nil, nil
	}
	tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}
	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	insecure := query.Get("allowInsecure")
	if insecure == "" {
		insecure = query.Get("insecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}
	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}
	fp := query.Get("fp")
	if fp != "" {
		tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
	}
	if security == "reality" {
		tlsOptions.Reality = &option.OutboundRealityOptions{Enabled: true, PublicKey: query.Get("pbk"), ShortID: query.Get("sid")}
		// Reality requires uTLS; use default fingerprint if not specified
		if tlsOptions.UTLS == nil {
			if fp == "" {
				fp = "chrome"
			}
			tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
		}
	}
	return tlsOptions, nil
}

func buildV2RayTransport(query url.Values) (*option.V2RayTransportOptions, error) {
	transportType := strings.ToLower(query.Get("type"))
	if transportType == "" {
		if query.Get("path") != "" {
			transportType = "ws"
		} else {
			transportType = "tcp"
		}
	}
	if transportType == "tcp" {
		return nil, nil
	}
	options := &option.V2RayTransportOptions{Type: transportType}
	switch transportType {
	case C.V2RayTransportTypeWebsocket:
		wsPath := query.Get("path")
		// 解析 path 中的 early data 参数，如 /path?ed=2048
		if idx := strings.Index(wsPath, "?ed="); idx != -1 {
			edPart := wsPath[idx+4:]
			wsPath = wsPath[:idx]
			// 解析 ed 值
			edValue := edPart
			if ampIdx := strings.Index(edPart, "&"); ampIdx != -1 {
				edValue = edPart[:ampIdx]
			}
			if ed, err := strconv.Atoi(edValue); err == nil && ed > 0 {
				options.WebsocketOptions.MaxEarlyData = uint32(ed)
				options.WebsocketOptions.EarlyDataHeaderName = "Sec-WebSocket-Protocol"
			}
		}
		options.WebsocketOptions.Path = wsPath
		if host := query.Get("host"); host != "" {
			options.WebsocketOptions.Headers = badoption.HTTPHeader{"Host": {host}}
		}
	case C.V2RayTransportTypeHTTP:
		options.HTTPOptions.Path = query.Get("path")
		if host := query.Get("host"); host != "" {
			options.HTTPOptions.Host = badoption.Listable[string]{host}
		}
	case C.V2RayTransportTypeGRPC:
		options.GRPCOptions.ServiceName = query.Get("serviceName")
	case C.V2RayTransportTypeHTTPUpgrade:
		options.HTTPUpgradeOptions.Path = query.Get("path")
	case "xhttp":
		// XHTTP is not supported by sing-box, fallback to HTTPUpgrade
		log.Printf("⚠️  XHTTP transport not supported by sing-box, falling back to HTTPUpgrade")
		options.Type = C.V2RayTransportTypeHTTPUpgrade
		options.HTTPUpgradeOptions.Path = query.Get("path")
		if host := query.Get("host"); host != "" {
			options.HTTPUpgradeOptions.Headers = badoption.HTTPHeader{"Host": {host}}
		}
	default:
		return nil, fmt.Errorf("unsupported transport type %q", transportType)
	}
	return options, nil
}

func buildShadowsocksOptions(u *url.URL) (option.ShadowsocksOutboundOptions, error) {
	identity, err := nodecodec.ParseURI(u.String())
	if err != nil {
		return option.ShadowsocksOutboundOptions{}, err
	}
	opts := option.ShadowsocksOutboundOptions{
		ServerOptions: option.ServerOptions{Server: identity.Identity.Server, ServerPort: identity.Identity.Port},
		Method:        firstIdentityOption(identity.Identity.Options, "method"),
		Password:      identity.Identity.Auth["password"],
	}
	if plugin := firstIdentityOption(identity.Identity.Options, "plugin"); plugin != "" {
		opts.Plugin = plugin
		opts.PluginOptions = firstIdentityOption(identity.Identity.Options, "plugin-opts")
	}
	return opts, nil
}

func firstIdentityOption(options map[string][]string, key string) string {
	if len(options[key]) == 0 {
		return ""
	}
	return options[key][0]
}

func buildTrojanOptions(u *url.URL, skipCertVerify bool) (option.TrojanOutboundOptions, error) {
	password := u.User.Username()
	if password == "" {
		return option.TrojanOutboundOptions{}, errors.New("trojan uri missing password in userinfo")
	}

	server, port, err := hostPort(u, 443)
	if err != nil {
		return option.TrojanOutboundOptions{}, err
	}

	query := u.Query()
	opts := option.TrojanOutboundOptions{
		ServerOptions: option.ServerOptions{Server: server, ServerPort: uint16(port)},
		Password:      password,
	}

	// Parse TLS options
	if tlsOptions, err := buildTrojanTLSOptions(query, skipCertVerify); err != nil {
		return option.TrojanOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	// Parse transport options
	if transport, err := buildV2RayTransport(query); err != nil {
		return option.TrojanOutboundOptions{}, err
	} else if transport != nil {
		opts.Transport = transport
	}

	return opts, nil
}

// vmessJSON represents the JSON structure of a VMess URI
type vmessJSON struct {
	V    interface{} `json:"v"`    // Version, can be string or int
	PS   string      `json:"ps"`   // Remarks/name
	Add  string      `json:"add"`  // Server address
	Port interface{} `json:"port"` // Server port, can be string or int
	ID   string      `json:"id"`   // UUID
	Aid  interface{} `json:"aid"`  // Alter ID, can be string or int
	Scy  string      `json:"scy"`  // Security/cipher
	Net  string      `json:"net"`  // Network type (tcp, ws, etc.)
	Type string      `json:"type"` // Header type
	Host string      `json:"host"` // Host header
	Path string      `json:"path"` // Path
	TLS  string      `json:"tls"`  // TLS (tls or empty)
	SNI  string      `json:"sni"`  // SNI
	ALPN string      `json:"alpn"` // ALPN
	FP   string      `json:"fp"`   // Fingerprint
}

func (v *vmessJSON) GetPort() int {
	switch p := v.Port.(type) {
	case float64:
		return int(p)
	case int:
		return p
	case string:
		port, _ := strconv.Atoi(p)
		return port
	}
	return 443
}

func (v *vmessJSON) GetAlterId() int {
	switch a := v.Aid.(type) {
	case float64:
		return int(a)
	case int:
		return a
	case string:
		aid, _ := strconv.Atoi(a)
		return aid
	}
	return 0
}

func buildVMessOptions(rawURI string, skipCertVerify bool) (option.VMessOutboundOptions, error) {
	// Remove vmess:// prefix
	encoded := strings.TrimPrefix(rawURI, "vmess://")
	if fragment := strings.IndexByte(encoded, '#'); fragment >= 0 {
		encoded = encoded[:fragment]
	}

	// Try to decode as base64 JSON (standard format)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// Try URL-safe base64
		decoded, err = base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			// Try as URL format: vmess://uuid@server:port?...
			return buildVMessOptionsFromURL(rawURI, skipCertVerify)
		}
	}

	var vmess vmessJSON
	if err := json.Unmarshal(decoded, &vmess); err != nil {
		return option.VMessOutboundOptions{}, fmt.Errorf("parse vmess json: %w", err)
	}

	if vmess.Add == "" {
		return option.VMessOutboundOptions{}, errors.New("vmess missing server address")
	}
	if vmess.ID == "" {
		return option.VMessOutboundOptions{}, errors.New("vmess missing uuid")
	}

	port := vmess.GetPort()
	if port == 0 {
		port = 443
	}

	opts := option.VMessOutboundOptions{
		ServerOptions: option.ServerOptions{
			Server:     vmess.Add,
			ServerPort: uint16(port),
		},
		UUID:     vmess.ID,
		AlterId:  vmess.GetAlterId(),
		Security: vmess.Scy,
	}

	// Default security
	if opts.Security == "" {
		opts.Security = "auto"
	}

	// Build transport options
	if vmess.Net != "" && vmess.Net != "tcp" {
		transport := &option.V2RayTransportOptions{}
		switch vmess.Net {
		case "ws":
			transport.Type = C.V2RayTransportTypeWebsocket
			wsPath := vmess.Path
			// Handle early data in path
			if idx := strings.Index(wsPath, "?ed="); idx != -1 {
				edPart := wsPath[idx+4:]
				wsPath = wsPath[:idx]
				edValue := edPart
				if ampIdx := strings.Index(edPart, "&"); ampIdx != -1 {
					edValue = edPart[:ampIdx]
				}
				if ed, err := strconv.Atoi(edValue); err == nil && ed > 0 {
					transport.WebsocketOptions.MaxEarlyData = uint32(ed)
					transport.WebsocketOptions.EarlyDataHeaderName = "Sec-WebSocket-Protocol"
				}
			}
			transport.WebsocketOptions.Path = wsPath
			if vmess.Host != "" {
				transport.WebsocketOptions.Headers = badoption.HTTPHeader{"Host": {vmess.Host}}
			}
		case "h2":
			transport.Type = C.V2RayTransportTypeHTTP
			transport.HTTPOptions.Path = vmess.Path
			if vmess.Host != "" {
				transport.HTTPOptions.Host = badoption.Listable[string]{vmess.Host}
			}
		case "grpc":
			transport.Type = C.V2RayTransportTypeGRPC
			transport.GRPCOptions.ServiceName = vmess.Path
		default:
			transport.Type = vmess.Net
		}
		opts.Transport = transport
	}

	// Build TLS options
	if vmess.TLS == "tls" {
		tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}
		if vmess.SNI != "" {
			tlsOptions.ServerName = vmess.SNI
		} else if vmess.Host != "" {
			tlsOptions.ServerName = vmess.Host
		}
		if vmess.ALPN != "" {
			tlsOptions.ALPN = badoption.Listable[string](strings.Split(vmess.ALPN, ","))
		}
		if vmess.FP != "" {
			tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: vmess.FP}
		}
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	return opts, nil
}

func buildVMessOptionsFromURL(rawURI string, skipCertVerify bool) (option.VMessOutboundOptions, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return option.VMessOutboundOptions{}, fmt.Errorf("parse vmess url: %w", err)
	}

	uuid := parsed.User.Username()
	if uuid == "" {
		return option.VMessOutboundOptions{}, errors.New("vmess uri missing uuid")
	}

	server, port, err := hostPort(parsed, 443)
	if err != nil {
		return option.VMessOutboundOptions{}, err
	}

	query := parsed.Query()
	opts := option.VMessOutboundOptions{
		ServerOptions: option.ServerOptions{
			Server:     server,
			ServerPort: uint16(port),
		},
		UUID:     uuid,
		Security: query.Get("encryption"),
	}

	if opts.Security == "" {
		opts.Security = "auto"
	}

	if aid := query.Get("alterId"); aid != "" {
		opts.AlterId, _ = strconv.Atoi(aid)
	}

	// Build transport
	if transport, err := buildV2RayTransport(query); err != nil {
		return option.VMessOutboundOptions{}, err
	} else if transport != nil {
		opts.Transport = transport
	}

	// Build TLS
	if tlsOptions, err := buildTLSOptions(query, skipCertVerify); err != nil {
		return option.VMessOutboundOptions{}, err
	} else if tlsOptions != nil {
		opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tlsOptions}
	}

	return opts, nil
}

func buildTrojanTLSOptions(query url.Values, skipCertVerify bool) (*option.OutboundTLSOptions, error) {
	// Trojan always uses TLS by default
	tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}

	if sni := query.Get("sni"); sni != "" {
		tlsOptions.ServerName = sni
	}
	if peer := query.Get("peer"); peer != "" && tlsOptions.ServerName == "" {
		tlsOptions.ServerName = peer
	}

	insecure := query.Get("allowInsecure")
	if insecure == "" {
		insecure = query.Get("insecure")
	}
	if insecure != "" {
		tlsOptions.Insecure = insecure == "1" || strings.EqualFold(insecure, "true")
	}

	if alpn := query.Get("alpn"); alpn != "" {
		tlsOptions.ALPN = badoption.Listable[string](strings.Split(alpn, ","))
	}

	if fp := query.Get("fp"); fp != "" {
		tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: fp}
	}

	return tlsOptions, nil
}

func hostPort(u *url.URL, defaultPort int) (string, int, error) {
	host := u.Hostname()
	if host == "" {
		return "", 0, errors.New("missing host")
	}
	portStr := u.Port()
	if portStr == "" {
		portStr = strconv.Itoa(defaultPort)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, port, nil
}

func parseAddr(value string) (*badoption.Addr, error) {
	addr := strings.TrimSpace(value)
	if addr == "" {
		return nil, nil
	}
	parsed, err := netip.ParseAddr(addr)
	if err != nil {
		return nil, err
	}
	bad := badoption.Addr(parsed)
	return &bad, nil
}

// orderedRegions returns the region codes present in regionMembers in a stable,
// friendly order: the well-known regions first (jp, kr, us, hk, tw) in their
// fixed order, then any additional country codes (e.g. sg, de, gb, ...) sorted
// alphabetically, and finally "other" last.
func orderedRegions(regionMembers map[string][]string) []string {
	seen := make(map[string]bool, len(regionMembers))
	result := make([]string, 0, len(regionMembers))
	// Well-known regions except "other", in fixed order.
	for _, r := range geoip.AllRegions() {
		if r == geoip.RegionOther {
			continue
		}
		if _, ok := regionMembers[r]; ok {
			result = append(result, r)
			seen[r] = true
		}
	}
	// Any additional country codes, sorted alphabetically.
	var extra []string
	for r := range regionMembers {
		if !seen[r] && r != geoip.RegionOther {
			extra = append(extra, r)
		}
	}
	sort.Strings(extra)
	result = append(result, extra...)
	// "other" always last, if present.
	if _, ok := regionMembers[geoip.RegionOther]; ok {
		result = append(result, geoip.RegionOther)
	}
	return result
}

func sanitizeTag(name string) string {
	lower := strings.ToLower(name)
	lower = strings.TrimSpace(lower)
	if lower == "" {
		return ""
	}
	segments := strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	result := strings.Join(segments, "-")
	result = strings.Trim(result, "-")
	return result
}

func atoiDefault(value string) int {
	if strings.HasSuffix(value, "mbps") {
		value = strings.TrimSuffix(value, "mbps")
	}
	if strings.HasSuffix(value, "Mbps") {
		value = strings.TrimSuffix(value, "Mbps")
	}
	v, _ := strconv.Atoi(value)
	return v
}

// printProxyLinks prints all proxy connection information at startup
func printProxyLinks(cfg *config.Config, metadata map[string]poolout.MemberMeta) {
	log.Println("")
	log.Println("📡 Proxy Links:")
	log.Println("═══════════════════════════════════════════════════════════════")

	showPoolEntry := cfg.Listener.Enabled
	showMultiPort := cfg.MultiPort.Enabled
	if !showPoolEntry && !showMultiPort {
		log.Println("🔒 Pool and multi-port entry points are disabled")
		return
	}

	if showPoolEntry {
		// Pool mode: single entry point for all nodes
		var auth string
		if cfg.Listener.Username != "" {
			auth = fmt.Sprintf("%s:%s@", cfg.Listener.Username, cfg.Listener.Password)
		}
		proxyURL := fmt.Sprintf("http://%s%s:%d", auth, cfg.Listener.Address, cfg.Listener.Port)
		log.Printf("🌐 Pool Entry Point:")
		log.Printf("   %s", proxyURL)
		log.Println("")
		log.Printf("   Nodes in pool (%d):", len(metadata))
		for _, meta := range metadata {
			log.Printf("   • %s", meta.Name)
		}
		if showMultiPort {
			log.Println("")
		}
	}

	if showMultiPort {
		// Multi-port mode: each node has its own port
		log.Printf("🔌 Multi-Port Entry Points (%d nodes):", len(cfg.Nodes))
		log.Println("")
		for _, node := range cfg.Nodes {
			var auth string
			username := node.Username
			password := node.Password
			if username == "" {
				username = cfg.MultiPort.Username
				password = cfg.MultiPort.Password
			}
			if username != "" {
				auth = fmt.Sprintf("%s:%s@", username, password)
			}
			proxyURL := fmt.Sprintf("http://%s%s:%d", auth, cfg.MultiPort.Address, node.Port)
			log.Printf("   [%d] %s", node.Port, node.Name)
			log.Printf("       %s", proxyURL)
		}
	}

	log.Println("═══════════════════════════════════════════════════════════════")
	log.Println("")
}
