package controlplane

import (
	"context"
	"testing"

	"easy_proxies/internal/config"
)

func TestSettingsPersistsExplicitTwoChoiceMode(t *testing.T) {
	f := newSettingsFixture(t)
	request := validAllSettings()
	request.PoolMode = "balance_p2c"
	if _, err := f.svc.UpdateAllSettings(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadForReload(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Pool.Mode != "balance_p2c" || f.svc.Snapshot().Pool.Mode != "balance_p2c" {
		t.Fatal("two-choice mode did not persist")
	}
}
