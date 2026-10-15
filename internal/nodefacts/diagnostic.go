package nodefacts

import (
	"strings"
	"time"

	"easy_proxies/internal/store"
)

const (
	FieldUnlockState     = "unlock.state"
	FieldSpeedStatus     = "speed.status"
	FieldQualityStatuses = "ipq.statuses"
)

func registerDiagnosticFields(registry *Registry) {
	for _, field := range []Field{
		{Name: FieldUnlockState, Label: "解锁汇总状态", Group: GroupUnlock, Kind: KindString, SupportsMaxAge: true, Source: "node_unlock_results"},
		{Name: FieldSpeedStatus, Label: "测速状态", Group: GroupPerf, Kind: KindString, SupportsMaxAge: true, Source: "node_detection_results.speed_status"},
		{Name: FieldQualityStatuses, Label: "质量提供商状态", Group: GroupIP, Kind: KindSet, SupportsMaxAge: true, Source: "node_ip_quality_results.status"},
	} {
		registry.Register(field)
	}
}

func applyDiagnosticFields(facts NodeFacts, detection *store.NodeDetectionResult, unlock *store.UnlockResult, quality []store.NodeIPQualityResult) {
	if detection != nil {
		facts.Set(FieldSpeedStatus, StringValue(knownTextAt(detection.SpeedStatus, detection.SpeedCheckedAt)))
	}
	if unlock != nil {
		state := "failed"
		unlocked, limited := 0, 0
		for _, service := range unlock.Services {
			switch strings.ToLower(service.Status) {
			case "unlocked":
				unlocked++
			case "locked", "partial", "originals_only":
				limited++
			}
		}
		switch {
		case unlock.Error != "":
			state = "failed"
		case len(unlock.Services) > 0 && unlocked == len(unlock.Services):
			state = "unlocked"
		case unlocked > 0 && limited > 0:
			state = "mixed"
		case limited > 0:
			state = "locked"
		}
		facts.Set(FieldUnlockState, StringValue(KnownAt(state, unlock.CheckedAt)))
	}
	var statuses []string
	var checkedAt time.Time
	for _, row := range quality {
		if row.Status == "" {
			continue
		}
		statuses = append(statuses, row.Status)
		if checkedAt.IsZero() || row.CheckedAt.Before(checkedAt) {
			checkedAt = row.CheckedAt
		}
	}
	if len(statuses) > 0 {
		facts.Set(FieldQualityStatuses, SetValue(KnownAt(statuses, checkedAt)))
	}
}
