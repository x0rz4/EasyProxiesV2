package unlock

import "testing"

type stubChecker struct {
	key, alias, label string
	order             int
}

func (checker stubChecker) Key() string         { return checker.key }
func (checker stubChecker) Aliases() []string   { return []string{checker.alias} }
func (checker stubChecker) DisplayName() string { return checker.label }
func (checker stubChecker) Order() int          { return checker.order }
func (checker stubChecker) Check(Runtime) ServiceResult {
	return ServiceResult{Status: StatusUnlocked}
}

func TestCheckerRegistryResolvesAliasesAndOrdersProviders(t *testing.T) {
	registry := newCheckerRegistry()
	registry.register(stubChecker{key: "later", alias: "late-alias", label: "Later", order: 20})
	registry.register(stubChecker{key: "first", alias: "FIRST ALIAS", label: "First", order: 10})
	resolved, ok := registry.get("first-alias")
	if !ok || resolved.Key() != "first" {
		t.Fatalf("alias resolution returned checker=%v ok=%v", resolved, ok)
	}
	listed := registry.list()
	if len(listed) != 2 || listed[0].Key() != "first" || listed[1].Key() != "later" {
		t.Fatalf("registry order=%v", []string{listed[0].Key(), listed[1].Key()})
	}
}

func TestBuiltInCheckerOrderAndAliases(t *testing.T) {
	want := []string{"netflix", "disney_plus", "chatgpt", "gemini", "claude", "youtube", "bahamut", "tiktok", "amazon", "reddit"}
	got := ListRegisteredServices()
	if len(got) != len(want) {
		t.Fatalf("registered services=%v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("registered services=%v want=%v", got, want)
		}
	}
	if checker, ok := GetChecker("OpenAI"); !ok || checker.Key() != "chatgpt" {
		t.Fatalf("OpenAI alias checker=%v ok=%v", checker, ok)
	}
	if checker, ok := GetChecker("Disney+"); !ok || checker.Key() != "disney_plus" {
		t.Fatalf("Disney alias checker=%v ok=%v", checker, ok)
	}
	metas := ListProviderMetas()
	if len(metas) != len(want) || metas[3].Value != "gemini" || metas[3].Category != "ai" || metas[5].Value != "youtube" || metas[5].Description == "" {
		t.Fatalf("provider metas=%+v", metas)
	}
	if status, ok := GetStatusMeta(StatusPartial); !ok || status.Label == "" {
		t.Fatalf("partial status meta=%+v ok=%v", status, ok)
	}
}
