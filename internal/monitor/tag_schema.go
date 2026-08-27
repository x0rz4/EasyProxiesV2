package monitor

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"easy_proxies/internal/geoip"
	"easy_proxies/internal/nodefacts"
	"easy_proxies/internal/store"
	"easy_proxies/internal/unlock"
)

// TagUnlockFactProviders is the unlock provider set shared by the tag schema,
// rule registry, and builtin templates. Keeping the conversion here means a
// newly registered checker appears in all three places at once.
func TagUnlockFactProviders() []nodefacts.ProviderInfo {
	metas := unlock.ListProviderMetas()
	providers := make([]nodefacts.ProviderInfo, 0, len(metas))
	for _, meta := range metas {
		providers = append(providers, nodefacts.ProviderInfo{Name: meta.Value, Label: meta.Label})
	}
	return providers
}

// TagIPQualityFactProviders lists the providers written by nodecheck.runQuality.
func TagIPQualityFactProviders() []nodefacts.ProviderInfo {
	return []nodefacts.ProviderInfo{
		{Name: "ippure", Label: "IPPure"},
		{Name: "ip-api", Label: "ip-api"},
	}
}

// NewTagFactRegistry builds the registry used by both the HTTP schema and the
// tagging service.
func NewTagFactRegistry() *nodefacts.Registry {
	return nodefacts.DefaultRegistry(
		nodefacts.WithUnlockProviders(TagUnlockFactProviders()),
		nodefacts.WithIPQualityProviders(TagIPQualityFactProviders()),
	)
}

type tagSchemaLimits struct {
	MaxConditions  int `json:"max_conditions"`
	MaxDepth       int `json:"max_depth"`
	MaxValueItems  int `json:"max_value_items"`
	MaxRegexLength int `json:"max_regex_length"`
	MaxRuleBytes   int `json:"max_rule_bytes"`
}

type tagEnumOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type tagEnumDefinition struct {
	Options   []tagEnumOption `json:"options"`
	FreeInput bool            `json:"free_input"`
}

type tagSchemaResponse struct {
	Version     int                          `json:"version"`
	Limits      tagSchemaLimits              `json:"limits"`
	Operators   []nodefacts.OperatorDef      `json:"operators"`
	FieldGroups []string                     `json:"field_groups"`
	Fields      []nodefacts.Field            `json:"fields"`
	Enums       map[string]tagEnumDefinition `json:"enums"`
}

func (s *Server) handleTagSchema(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	registry := NewTagFactRegistry()
	limits := nodefacts.DefaultLimits()
	if s.tagSvc != nil {
		registry = s.tagSvc.Registry()
		limits = s.tagSvc.Limits()
	}
	enums, err := s.tagSchemaEnums(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, tagSchemaResponse{
		Version: 1,
		Limits: tagSchemaLimits{
			MaxConditions: limits.MaxConditions, MaxDepth: limits.MaxDepth,
			MaxValueItems: limits.MaxValueItems, MaxRegexLength: limits.MaxRegexLength,
			MaxRuleBytes: limits.MaxRuleBytes,
		},
		Operators: nodefacts.Operators(), FieldGroups: registry.Groups(),
		Fields: registry.Fields(), Enums: enums,
	})
}

func (s *Server) tagSchemaEnums(ctx context.Context) (map[string]tagEnumDefinition, error) {
	enums := map[string]tagEnumDefinition{
		nodefacts.EnumRegion:           {Options: regionEnumOptions(nil), FreeInput: true},
		nodefacts.EnumCountryCode:      {Options: []tagEnumOption{}, FreeInput: true},
		nodefacts.EnumProtocol:         {Options: protocolEnumOptions(nil)},
		nodefacts.EnumNodeSource:       {Options: nodeSourceEnumOptions()},
		nodefacts.EnumIPFamily:         {Options: valueOptions("ipv4", "ipv6")},
		nodefacts.EnumRiskLevel:        {Options: valueOptions("High", "Medium", "Low"), FreeInput: true},
		nodefacts.EnumUnlockStatus:     {Options: unlockStatusEnumOptions()},
		nodefacts.EnumUnlockProvider:   {Options: unlockProviderEnumOptions()},
		nodefacts.EnumTagName:          {Options: []tagEnumOption{}},
		nodefacts.EnumSubscriptionID:   {Options: []tagEnumOption{}},
		nodefacts.EnumSubscriptionName: {Options: []tagEnumOption{}},
	}
	if s == nil || s.store == nil {
		return enums, nil
	}

	nodes, err := s.store.ListNodes(ctx, store.NodeFilter{})
	if err != nil {
		return nil, err
	}
	regions := make([]string, 0, len(nodes))
	protocols := make([]string, 0, len(nodes))
	for _, node := range nodes {
		regions = append(regions, strings.ToLower(strings.TrimSpace(node.Region)))
		if scheme := tagURIScheme(node.URI); scheme != "" {
			protocols = append(protocols, scheme)
		}
	}
	enums[nodefacts.EnumRegion] = tagEnumDefinition{Options: regionEnumOptions(regions), FreeInput: true}
	enums[nodefacts.EnumProtocol] = tagEnumDefinition{Options: protocolEnumOptions(protocols)}

	detections, err := s.store.ListNodeDetectionResultsByIDs(ctx, nil)
	if err != nil {
		return nil, err
	}
	countryCodes := make([]string, 0, len(detections))
	for _, detection := range detections {
		if detection != nil {
			countryCodes = append(countryCodes, strings.ToUpper(strings.TrimSpace(detection.ExitCountryCode)))
		}
	}
	enums[nodefacts.EnumCountryCode] = tagEnumDefinition{
		Options: stringEnumOptions(countryCodes, nil), FreeInput: true,
	}

	tags, err := s.store.ListTags(ctx)
	if err != nil {
		return nil, err
	}
	tagOptions := make([]tagEnumOption, 0, len(tags))
	for _, tag := range tags {
		tagOptions = append(tagOptions, tagEnumOption{Value: tag.Name, Label: tag.Name})
	}
	enums[nodefacts.EnumTagName] = tagEnumDefinition{Options: tagOptions}

	subscriptions, err := s.store.ListSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	idOptions := make([]tagEnumOption, 0, len(subscriptions))
	nameOptions := make([]tagEnumOption, 0, len(subscriptions))
	for _, subscription := range subscriptions {
		id := strconv.FormatInt(subscription.ID, 10)
		label := strings.TrimSpace(subscription.Name)
		if label == "" {
			label = id
		}
		idOptions = append(idOptions, tagEnumOption{Value: id, Label: label})
		nameOptions = append(nameOptions, tagEnumOption{Value: subscription.Name, Label: label})
	}
	enums[nodefacts.EnumSubscriptionID] = tagEnumDefinition{Options: idOptions}
	enums[nodefacts.EnumSubscriptionName] = tagEnumDefinition{Options: nameOptions}
	return enums, nil
}

func regionEnumOptions(extra []string) []tagEnumOption {
	regions := append(geoip.AllRegions(), extra...)
	return stringEnumOptions(regions, func(value string) string {
		return strings.TrimSpace(geoip.RegionEmoji(value) + " " + geoip.RegionName(value))
	})
}

func protocolEnumOptions(extra []string) []tagEnumOption {
	fixed := []string{"vmess", "ss", "vless", "trojan", "hysteria2", "anytls", "http", "socks5"}
	return stringEnumOptions(append(fixed, extra...), nil)
}

func nodeSourceEnumOptions() []tagEnumOption {
	return []tagEnumOption{
		{Value: store.NodeSourceInline, Label: "内联配置"},
		{Value: store.NodeSourceFile, Label: "节点文件"},
		{Value: store.NodeSourceSubscription, Label: "订阅"},
		{Value: store.NodeSourceManual, Label: "手动"},
	}
}

func unlockStatusEnumOptions() []tagEnumOption {
	metas := unlock.ListStatusMetas()
	options := make([]tagEnumOption, 0, len(metas))
	for _, meta := range metas {
		options = append(options, tagEnumOption{Value: meta.Value, Label: meta.Label})
	}
	return options
}

func unlockProviderEnumOptions() []tagEnumOption {
	metas := unlock.ListProviderMetas()
	options := make([]tagEnumOption, 0, len(metas))
	for _, meta := range metas {
		options = append(options, tagEnumOption{Value: meta.Value, Label: meta.Label})
	}
	return options
}

func valueOptions(values ...string) []tagEnumOption { return stringEnumOptions(values, nil) }

func stringEnumOptions(values []string, label func(string) string) []tagEnumOption {
	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	options := make([]tagEnumOption, 0, len(normalized))
	for _, value := range normalized {
		display := value
		if label != nil {
			display = label(value)
		}
		options = append(options, tagEnumOption{Value: value, Label: display})
	}
	return options
}

func tagURIScheme(uri string) string {
	scheme, _, found := strings.Cut(strings.TrimSpace(uri), "://")
	if !found {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(scheme))
}
