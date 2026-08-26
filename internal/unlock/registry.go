package unlock

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Checker is one independently testable unlock provider module.
type Checker interface {
	Key() string
	Aliases() []string
	DisplayName() string
	Order() int
	Check(Runtime) ServiceResult
}

// CheckerMeta is optional so externally registered detectors using the basic
// interface remain source-compatible.
type CheckerMeta interface {
	Meta() ProviderMeta
}

// ProviderMeta describes one detector independently from a live result.
type ProviderMeta struct {
	Value       string   `json:"value"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
	Order       int      `json:"order"`
}

type checkerRegistry struct {
	mu              sync.RWMutex
	checkers        map[string]Checker
	canonicalByName map[string]string
}

var globalCheckerRegistry = newCheckerRegistry()

// Services preserves the historical public service list. Runtime execution is
// driven by ListRegisteredServices so third-party checkers can be added.
var Services = []string{"netflix", "disney_plus", "chatgpt", "gemini", "claude", "youtube", "bahamut", "tiktok", "amazon", "reddit"}

func newCheckerRegistry() *checkerRegistry {
	return &checkerRegistry{
		checkers:        make(map[string]Checker),
		canonicalByName: make(map[string]string),
	}
}

// RegisterChecker registers a detector and its aliases. Duplicate canonical
// keys or conflicting aliases fail fast during package initialization.
func RegisterChecker(checker Checker) {
	globalCheckerRegistry.register(checker)
}

func (r *checkerRegistry) register(checker Checker) {
	if checker == nil {
		panic("unlock checker is nil")
	}
	key := normalizeProviderName(checker.Key())
	if key == "" {
		panic("unlock checker key is empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.checkers[key]; exists {
		panic(fmt.Sprintf("unlock checker already registered: %s", key))
	}
	r.checkers[key] = checker
	r.canonicalByName[key] = key
	for _, alias := range checker.Aliases() {
		normalized := normalizeProviderName(alias)
		if normalized == "" {
			continue
		}
		if existing, exists := r.canonicalByName[normalized]; exists && existing != key {
			panic(fmt.Sprintf("unlock checker alias conflict: %s", alias))
		}
		r.canonicalByName[normalized] = key
	}
}

func (r *checkerRegistry) get(name string) (Checker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	canonical, exists := r.canonicalByName[normalizeProviderName(name)]
	if !exists {
		return nil, false
	}
	checker, ok := r.checkers[canonical]
	return checker, ok
}

func (r *checkerRegistry) list() []Checker {
	r.mu.RLock()
	checkers := make([]Checker, 0, len(r.checkers))
	for _, checker := range r.checkers {
		checkers = append(checkers, checker)
	}
	r.mu.RUnlock()
	sort.Slice(checkers, func(i, j int) bool {
		if checkers[i].Order() != checkers[j].Order() {
			return checkers[i].Order() < checkers[j].Order()
		}
		return normalizeProviderName(checkers[i].Key()) < normalizeProviderName(checkers[j].Key())
	})
	return checkers
}

// GetChecker resolves a canonical provider key or alias.
func GetChecker(name string) (Checker, bool) {
	return globalCheckerRegistry.get(name)
}

// ListRegisteredServices returns canonical keys in deterministic display order.
func ListRegisteredServices() []string {
	checkers := globalCheckerRegistry.list()
	services := make([]string, 0, len(checkers))
	for _, checker := range checkers {
		services = append(services, normalizeProviderName(checker.Key()))
	}
	return services
}

// DisplayName returns registry metadata for a provider or the input as fallback.
func DisplayName(name string) string {
	return GetProviderMeta(name).Label
}

// GetProviderMeta resolves detector metadata by key or alias.
func GetProviderMeta(name string) ProviderMeta {
	checker, ok := GetChecker(name)
	if !ok {
		return ProviderMeta{Value: normalizeProviderName(name), Label: name, Category: "custom"}
	}
	return checkerProviderMeta(checker)
}

// ListProviderMetas returns metadata in the same deterministic order as checks.
func ListProviderMetas() []ProviderMeta {
	checkers := globalCheckerRegistry.list()
	metas := make([]ProviderMeta, 0, len(checkers))
	for _, checker := range checkers {
		metas = append(metas, checkerProviderMeta(checker))
	}
	return metas
}

func checkerProviderMeta(checker Checker) ProviderMeta {
	meta := ProviderMeta{}
	if provider, ok := checker.(CheckerMeta); ok {
		meta = provider.Meta()
	}
	if meta.Value == "" {
		meta.Value = normalizeProviderName(checker.Key())
	}
	if meta.Label == "" {
		meta.Label = checker.DisplayName()
	}
	if meta.Category == "" {
		meta.Category = "custom"
	}
	if meta.Order == 0 {
		meta.Order = checker.Order()
	}
	if len(meta.Aliases) == 0 {
		meta.Aliases = append([]string(nil), checker.Aliases()...)
	}
	return meta
}

func normalizeProviderName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("-", "_", " ", "_", "+", "_plus").Replace(value)
	return strings.Trim(value, "_")
}
