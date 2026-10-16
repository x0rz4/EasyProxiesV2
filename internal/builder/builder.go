package builder

import (
	"errors"
	"fmt"
	"log"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/geoip"
	groupruntime "easy_proxies/internal/group"
	"easy_proxies/internal/groupmember"
	"easy_proxies/internal/nodecodec"
	poolout "easy_proxies/internal/outbound/pool"
	"easy_proxies/internal/runtimetag"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/json/badoption"
)

// Build converts high level config into a sing-box Options tree. The initial
// runtime is versioned too, so cold-start members never use a separate legacy
// tag namespace from members created by a later reconciliation.
func Build(cfg *config.Config) (option.Options, error) {
	return BuildVersion(cfg, runtimetag.InitialVersion)
}

// BuildVersion converts high level config into a sing-box Options tree using
// one runtime tag version for every node outbound in the tree.
func BuildVersion(cfg *config.Config, runtimeTagVersion uint64) (option.Options, error) {
	if runtimeTagVersion == 0 {
		return option.Options{}, errors.New("runtime tag version must be greater than zero")
	}
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
	usedTags := make(map[string]struct{}, len(cfg.Nodes))

	// Track nodes by region for GeoIP routing.
	// Region codes are open-ended (any lowercased ISO country code can appear),
	// so this map is grown on demand instead of pre-seeded with a fixed set.
	regionMembers := make(map[string][]string)

	for _, node := range cfg.Nodes {
		tag, err := runtimetag.Format(node.NodeKey(), runtimeTagVersion)
		if err != nil {
			log.Printf("❌ Failed to build node '%s': %v (skipping)", node.Name, err)
			failedNodes = append(failedNodes, node.Name)
			continue
		}
		if _, exists := usedTags[tag]; exists {
			return option.Options{}, fmt.Errorf("runtime tag collision %q for node %q", tag, node.Name)
		}

		outbound, err := buildNodeOutbound(tag, node.URI, cfg.SkipCertVerify)
		if err != nil {
			log.Printf("❌ Failed to build node '%s': %v (skipping)", node.Name, err)
			failedNodes = append(failedNodes, node.Name)
			continue
		}
		usedTags[tag] = struct{}{}
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
			meta.Region = ""
			meta.Country = ""
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
		groupOutboundTag := fmt.Sprintf("group-pool-%d", group.ID)
		groupInboundTag := fmt.Sprintf("group-in-%d", group.ID)
		groupOptions := poolout.Options{Mode: group.DispatchMode, Members: members,
			FailureThreshold: group.FailureThreshold, FailureWindow: group.FailureWindow,
			HealthCheckInterval: group.HealthCheckInterval,
			BlacklistDuration:   100 * 365 * 24 * time.Hour, Metadata: groupMeta,
			GroupID: group.ID, PreferredMember: preferredTag, InitialGroupState: stateByTag,
			RecoveryEnabled: group.RecoveryEnabled, RecoverySuccessThreshold: group.RecoverySuccessThreshold,
			RecoveryCooldown: group.RecoveryCooldown}
		if group.Execute {
			groupOptions.CapacityTarget = group.TargetSize
		}
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
	return BuildBaseVersion(cfg, runtimetag.InitialVersion)
}

// BuildBaseVersion builds the application-wide runtime with an explicit node
// outbound tag version.
func BuildBaseVersion(cfg *config.Config, runtimeTagVersion uint64) (option.Options, error) {
	baseCfg := cfg.Clone()
	baseCfg.Groups = nil
	return BuildVersion(baseCfg, runtimeTagVersion)
}

// BuildGroup builds a self-contained sing-box instance for one enabled group.
// It retains only the target group's inbound, pool, and member
// outbounds on which that pool depends.
func BuildGroup(cfg *config.Config, groupID int64) (option.Options, error) {
	return BuildGroupVersion(cfg, groupID, runtimetag.InitialVersion)
}

// BuildGroupVersion builds one group runtime with an explicit node outbound
// tag version.
func BuildGroupVersion(cfg *config.Config, groupID int64, runtimeTagVersion uint64) (option.Options, error) {
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
	opts, err := BuildVersion(groupCfg, runtimeTagVersion)
	if err != nil {
		return option.Options{}, err
	}
	poolTag := fmt.Sprintf("group-pool-%d", groupID)
	inboundTag := fmt.Sprintf("group-in-%d", groupID)
	keep := map[string]struct{}{poolTag: {}}
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
	filteredOutbounds := make([]option.Outbound, 0, len(keep))
	for _, outbound := range opts.Outbounds {
		if _, ok := keep[outbound.Tag]; ok {
			filteredOutbounds = append(filteredOutbounds, outbound)
		}
	}
	filteredInbounds := make([]option.Inbound, 0, 1)
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

// BuildNodeOutbound builds one dynamic node outbound under an already
// validated concrete runtime tag. Dynamic nodes must remain dependency-free;
// callers verify the created adapter before publishing it into a pool.
func BuildNodeOutbound(tag string, node config.NodeConfig, skipCertVerify bool) (option.Outbound, error) {
	if strings.TrimSpace(tag) == "" {
		return option.Outbound{}, errors.New("runtime tag is empty")
	}
	return buildNodeOutbound(tag, node.URI, skipCertVerify)
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
	spec, err := nodecodec.FromURI(rawURI)
	if err != nil {
		return option.Outbound{}, err
	}
	return buildSpecOutbound(tag, spec, skipCertVerify)
}

// buildSpecOutbound maps one parsed node onto a sing-box outbound.
//
// This is the only place that turns a link into a sing-box option tree. It used
// to be one of five: builder re-decoded the vmess payload, re-read the query with
// its own spellings, and carried its own copies of the TLS and transport
// builders. That is how a node could be identified one way and dialled another.
func buildSpecOutbound(tag string, spec nodecodec.Spec, skipCertVerify bool) (option.Outbound, error) {
	switch spec.Protocol {
	case "http":
		opts := option.HTTPOutboundOptions{
			ServerOptions: serverOptions(spec),
			Username:      spec.Auth.Username,
			Password:      spec.Auth.Password,
			Path:          spec.Transport.Path,
		}
		if spec.Transport.Host != "" {
			opts.Headers = badoption.HTTPHeader{"Host": {spec.Transport.Host}}
		}
		if tls := outboundTLSOptions(spec, skipCertVerify); tls != nil {
			opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tls}
		}
		return option.Outbound{Type: C.TypeHTTP, Tag: tag, Options: &opts}, nil
	case "socks5":
		opts := option.SOCKSOutboundOptions{
			ServerOptions: serverOptions(spec),
			Version:       "5",
			Username:      spec.Auth.Username,
			Password:      spec.Auth.Password,
		}
		if spec.Network != "" {
			opts.Network = option.NetworkList(spec.Network)
		}
		return option.Outbound{Type: C.TypeSOCKS, Tag: tag, Options: &opts}, nil
	case "vless":
		opts := option.VLESSOutboundOptions{
			UUID:          spec.Auth.UUID,
			ServerOptions: serverOptions(spec),
		}
		if spec.Flow != "" {
			opts.Flow = spec.Flow
		}
		if spec.PacketEncoding != "" {
			packetEncoding := spec.PacketEncoding
			opts.PacketEncoding = &packetEncoding
		}
		transport, err := v2rayTransport(spec)
		if err != nil {
			return option.Outbound{}, err
		}
		opts.Transport = transport
		if tls := outboundTLSOptions(spec, skipCertVerify); tls != nil {
			opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tls}
		}
		return option.Outbound{Type: C.TypeVLESS, Tag: tag, Options: &opts}, nil
	case "vmess":
		opts := option.VMessOutboundOptions{
			ServerOptions: serverOptions(spec),
			UUID:          spec.Auth.UUID,
			AlterId:       spec.AlterID,
			Security:      nodecodec.DefaultString(spec.Cipher, "auto"),
		}
		transport, err := v2rayTransport(spec)
		if err != nil {
			return option.Outbound{}, err
		}
		opts.Transport = transport
		if tls := outboundTLSOptions(spec, skipCertVerify); tls != nil {
			opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tls}
		}
		return option.Outbound{Type: C.TypeVMess, Tag: tag, Options: &opts}, nil
	case "trojan":
		opts := option.TrojanOutboundOptions{
			ServerOptions: serverOptions(spec),
			Password:      spec.AuthString(),
		}
		transport, err := v2rayTransport(spec)
		if err != nil {
			return option.Outbound{}, err
		}
		opts.Transport = transport
		if tls := outboundTLSOptions(spec, skipCertVerify); tls != nil {
			opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tls}
		}
		return option.Outbound{Type: C.TypeTrojan, Tag: tag, Options: &opts}, nil
	case "ss":
		opts := option.ShadowsocksOutboundOptions{
			ServerOptions: serverOptions(spec),
			Method:        spec.Cipher,
			Password:      spec.Auth.Password,
		}
		if spec.Plugin.Name != "" {
			opts.Plugin = spec.Plugin.Name
			opts.PluginOptions = spec.Plugin.Options
		}
		return option.Outbound{Type: C.TypeShadowsocks, Tag: tag, Options: &opts}, nil
	case "hysteria2":
		opts := option.Hysteria2OutboundOptions{
			ServerOptions: serverOptions(spec),
			Password:      spec.AuthString(),
			UpMbps:        spec.UpMbps,
			DownMbps:      spec.DownMbps,
		}
		if spec.Obfs != "" {
			opts.Obfs = &option.Hysteria2Obfs{Type: spec.Obfs, Password: spec.ObfsPassword}
		}
		if tls := outboundTLSOptions(spec, skipCertVerify); tls != nil {
			opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tls}
		}
		return option.Outbound{Type: C.TypeHysteria2, Tag: tag, Options: &opts}, nil
	case "anytls":
		opts := option.AnyTLSOutboundOptions{
			ServerOptions: serverOptions(spec),
			Password:      spec.AuthString(),
		}
		if tls := outboundTLSOptions(spec, skipCertVerify); tls != nil {
			opts.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{TLS: tls}
		}
		return option.Outbound{Type: C.TypeAnyTLS, Tag: tag, Options: &opts}, nil
	default:
		return option.Outbound{}, fmt.Errorf("unsupported scheme %q", spec.Protocol)
	}
}

func serverOptions(spec nodecodec.Spec) option.ServerOptions {
	return option.ServerOptions{Server: spec.Server, ServerPort: spec.Port}
}

// outboundTLSOptions maps the spec's TLS view onto sing-box options.
//
// It is the single place that decides whether TLS is on and how the certificate
// is verified. The four per-protocol copies it replaces resolved allowInsecure
// against insecure in opposite orders, and only two of them understood
// skip-cert-verify at all.
func outboundTLSOptions(spec nodecodec.Spec, skipCertVerify bool) *option.OutboundTLSOptions {
	if !spec.TLS.Enabled {
		return nil
	}
	tlsOptions := &option.OutboundTLSOptions{Enabled: true, Insecure: skipCertVerify}
	if spec.TLS.ServerName != "" {
		tlsOptions.ServerName = spec.TLS.ServerName
	}
	if spec.TLS.InsecureStated {
		tlsOptions.Insecure = spec.TLS.Insecure
	}
	if len(spec.TLS.ALPN) > 0 {
		tlsOptions.ALPN = badoption.Listable[string](spec.TLS.ALPN)
	}
	if spec.TLS.Fingerprint != "" {
		tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: spec.TLS.Fingerprint}
	}
	if spec.TLS.Security == "reality" {
		tlsOptions.Reality = &option.OutboundRealityOptions{
			Enabled: true, PublicKey: spec.TLS.PublicKey, ShortID: spec.TLS.ShortID,
		}
		if tlsOptions.UTLS == nil {
			// REALITY requires uTLS; fall back to a default fingerprint.
			tlsOptions.UTLS = &option.OutboundUTLSOptions{Enabled: true, Fingerprint: "chrome"}
		}
	}
	return tlsOptions
}

// v2rayTransport builds the transport block for vless / vmess / trojan.
//
// "h2" is the V2Ray spelling of sing-box's "http" transport, and a link is free
// to use either, so both must land on the same option.
func v2rayTransport(spec nodecodec.Spec) (*option.V2RayTransportOptions, error) {
	transportType := strings.ToLower(strings.TrimSpace(spec.Transport.Type))
	switch transportType {
	case "", "tcp":
		return nil, nil
	case "h2":
		transportType = C.V2RayTransportTypeHTTP
	}
	options := &option.V2RayTransportOptions{Type: transportType}
	switch transportType {
	case C.V2RayTransportTypeWebsocket:
		wsPath := spec.Transport.Path
		// Early data is carried inside the path, e.g. /path?ed=2048.
		if idx := strings.Index(wsPath, "?ed="); idx != -1 {
			edPart := wsPath[idx+4:]
			wsPath = wsPath[:idx]
			edValue := edPart
			if ampIdx := strings.Index(edPart, "&"); ampIdx != -1 {
				edValue = edPart[:ampIdx]
			}
			if earlyData, err := strconv.Atoi(edValue); err == nil && earlyData > 0 {
				options.WebsocketOptions.MaxEarlyData = uint32(earlyData)
				options.WebsocketOptions.EarlyDataHeaderName = "Sec-WebSocket-Protocol"
			}
		}
		options.WebsocketOptions.Path = wsPath
		if spec.Transport.Host != "" {
			options.WebsocketOptions.Headers = badoption.HTTPHeader{"Host": {spec.Transport.Host}}
		}
	case C.V2RayTransportTypeHTTP:
		options.HTTPOptions.Path = spec.Transport.Path
		if spec.Transport.Host != "" {
			options.HTTPOptions.Host = badoption.Listable[string]{spec.Transport.Host}
		}
	case C.V2RayTransportTypeGRPC:
		options.GRPCOptions.ServiceName = spec.Transport.ServiceName
	case C.V2RayTransportTypeHTTPUpgrade:
		options.HTTPUpgradeOptions.Path = spec.Transport.Path
		if spec.Transport.Host != "" {
			options.HTTPUpgradeOptions.Headers = badoption.HTTPHeader{"Host": {spec.Transport.Host}}
		}
	case "xhttp":
		// XHTTP is not supported by sing-box, fall back to HTTPUpgrade.
		log.Printf("⚠️  XHTTP transport not supported by sing-box, falling back to HTTPUpgrade")
		options.Type = C.V2RayTransportTypeHTTPUpgrade
		options.HTTPUpgradeOptions.Path = spec.Transport.Path
		if spec.Transport.Host != "" {
			options.HTTPUpgradeOptions.Headers = badoption.HTTPHeader{"Host": {spec.Transport.Host}}
		}
	default:
		return nil, fmt.Errorf("unsupported transport type %q", spec.Transport.Type)
	}
	return options, nil
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

// maskSecret keeps the first and last character of a credential for
// recognition and hides the rest; empty and single-character secrets render
// as a fixed marker so the log never shows the actual value.
func maskSecret(secret string) string {
	switch len(secret) {
	case 0:
		return ""
	case 1:
		return "*"
	default:
		return secret[:1] + "…" + secret[len(secret)-1:]
	}
}

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

	// Credentials are masked: logs are frequently pasted into issues and
	// chats, and these lines would otherwise leak every entry-point password.
	if showPoolEntry {
		// Pool mode: single entry point for all nodes
		var auth string
		if cfg.Listener.Username != "" {
			auth = fmt.Sprintf("%s:%s@", cfg.Listener.Username, maskSecret(cfg.Listener.Password))
		}
		proxyURL := fmt.Sprintf("http://%s%s:%d", auth, cfg.Listener.Address, cfg.Listener.Port)
		log.Printf("🌐 Pool Entry Point:")
		log.Printf("   %s", proxyURL)
		log.Println("")
		log.Printf("   Nodes in pool: %d", len(metadata))
		if showMultiPort {
			log.Println("")
		}
	}

	if showMultiPort {
		// Multi-port mode: each node has its own port. Listing every node made
		// each reload O(N) log lines; the WebUI is the per-node reference.
		log.Printf("🔌 Multi-Port Entry Points: %d nodes, base address %s, ports %d–%d",
			len(cfg.Nodes), cfg.MultiPort.Address, cfg.MultiPort.BasePort, cfg.MultiPort.BasePort+uint16(len(cfg.Nodes))-1)
	}

	log.Println("═══════════════════════════════════════════════════════════════")
	log.Println("")
}
