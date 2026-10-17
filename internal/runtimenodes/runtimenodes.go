// Package runtimenodes decides which stored nodes belong in the running
// sing-box instance.
//
// This used to be assembled in three places that disagreed: app startup, the
// subscription manager's reconcile pass, and boxmgr.TriggerReload. The reload
// copy filtered only on nodes.enabled, so a node belonging to a disabled (or
// displaced) subscription re-entered the runtime after every node CRUD, import,
// settings save or GeoIP update. Keeping the rule in one package is what makes
// those paths agree.
package runtimenodes

import (
	"context"
	"fmt"
	"strings"

	"easy_proxies/internal/config"
	"easy_proxies/internal/store"
)

// Load returns the nodes that may enter the runtime:
//
//   - enabled nodes whose source is not "subscription"
//   - enabled nodes reachable through a membership of an enabled subscription
//
// Anything else — a node of a disabled subscription, a node displaced by an
// active-subscription exclusive activation, a disabled node — is absent.
// Duplicate identities are collapsed; the first occurrence wins.
func Load(ctx context.Context, s store.Store) ([]config.NodeConfig, error) {
	if s == nil {
		return nil, nil
	}
	regular, err := s.ListNodes(ctx, store.NodeFilter{})
	if err != nil {
		return nil, fmt.Errorf("list runtime nodes: %w", err)
	}
	subscriptionNodes, err := s.ListEffectiveSubscriptionNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list effective subscription nodes: %w", err)
	}
	seen := make(map[string]struct{}, len(regular)+len(subscriptionNodes))
	result := make([]config.NodeConfig, 0, len(regular)+len(subscriptionNodes))
	for _, n := range regular {
		if n.Source == store.NodeSourceSubscription {
			continue
		}
		if node, ok := convert(n, seen); ok {
			result = append(result, node)
		}
	}
	for _, n := range subscriptionNodes {
		if node, ok := convert(n, seen); ok {
			result = append(result, node)
		}
	}
	return result, nil
}

// Convert turns stored rows into runtime node configs without any filtering.
// Callers that have already decided which rows belong in the runtime (startup
// bootstrap, tests) use it instead of re-implementing the mapping.
func Convert(nodes []store.Node) []config.NodeConfig {
	result := make([]config.NodeConfig, 0, len(nodes))
	for _, n := range nodes {
		result = append(result, nodeConfig(n))
	}
	return result
}

func convert(n store.Node, seen map[string]struct{}) (config.NodeConfig, bool) {
	uri := strings.TrimSpace(n.URI)
	if !n.Enabled || uri == "" {
		return config.NodeConfig{}, false
	}
	key := IdentityKey(n)
	if _, duplicate := seen[key]; duplicate {
		return config.NodeConfig{}, false
	}
	seen[key] = struct{}{}
	n.URI = uri
	return nodeConfig(n), true
}

// IdentityKey is the deduplication key for one stored node: its semantic
// identity hash when known, otherwise its URI.
func IdentityKey(n store.Node) string {
	if n.IdentityHash != "" {
		return n.IdentityHash
	}
	return strings.TrimSpace(n.URI)
}

func nodeConfig(n store.Node) config.NodeConfig {
	return config.NodeConfig{
		ID:            n.ID,
		Name:          n.Name,
		URI:           n.URI,
		Port:          n.Port,
		Username:      n.Username,
		Password:      n.Password,
		Source:        config.NodeSource(n.Source),
		Region:        n.Region,
		Country:       n.Country,
		Tags:          append([]string(nil), n.Tags...),
		IdentityHash:  n.IdentityHash,
		CanonicalJSON: n.CanonicalJSON,
	}
}
