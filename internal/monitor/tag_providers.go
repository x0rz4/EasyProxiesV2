package monitor

import (
	"easy_proxies/internal/nodefacts"
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
