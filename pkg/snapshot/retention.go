package snapshot

import (
	"fmt"
	"sort"
	"time"

	"github.com/thanhvo-agy/Telegram-Drive/pkg/config"
)

// PrunePlan indicates which snapshots to keep and which to delete
type PrunePlan struct {
	Keep   []SnapshotSummary `json:"keep"`
	Delete []SnapshotSummary `json:"delete"`
}

// CalculatePrunePlan determines which snapshots to delete based on the retention policy
func CalculatePrunePlan(snapshots []SnapshotSummary, policy config.RetentionConfig) PrunePlan {
	return CalculatePrunePlanAt(snapshots, policy, time.Now())
}

// CalculatePrunePlanAt determines which snapshots to delete based on the retention policy at a specific point in time
func CalculatePrunePlanAt(snapshots []SnapshotSummary, policy config.RetentionConfig, now time.Time) PrunePlan {
	if len(snapshots) <= 1 {
		return PrunePlan{Keep: snapshots, Delete: nil}
	}

	// Sort snapshots newest to oldest
	sorted := make([]SnapshotSummary, len(snapshots))
	copy(sorted, snapshots)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})

	keepMap := make(map[string]bool)

	// Always keep the very latest snapshot
	keepMap[sorted[0].SnapshotID] = true

	// WORM & Tamper Lock Enforcement:
	// Any snapshot that is WORM-locked or marked Tampered MUST be kept and NEVER deleted.
	for _, s := range sorted {
		if s.IsWORMLocked(now) || s.Tampered {
			keepMap[s.SnapshotID] = true
		}
	}

	// Hourly buckets
	hourlyBuckets := make(map[string]bool)
	hourlyCount := 0

	// Daily buckets
	dailyBuckets := make(map[string]bool)
	dailyCount := 0

	// Weekly buckets
	weeklyBuckets := make(map[string]bool)
	weeklyCount := 0

	// Monthly buckets
	monthlyBuckets := make(map[string]bool)
	monthlyCount := 0

	for _, s := range sorted {
		t := s.CreatedAt

		// Check hourly
		hourKey := t.Format("2006-01-02-15")
		if !hourlyBuckets[hourKey] && hourlyCount < policy.KeepHourly {
			hourlyBuckets[hourKey] = true
			hourlyCount++
			keepMap[s.SnapshotID] = true
		}

		// Check daily
		dayKey := t.Format("2006-01-02")
		if !dailyBuckets[dayKey] && dailyCount < policy.KeepDaily {
			dailyBuckets[dayKey] = true
			dailyCount++
			keepMap[s.SnapshotID] = true
		}

		// Check weekly
		year, week := t.ISOWeek()
		weekKey := fmt.Sprintf("%d-W%02d", year, week)
		if !weeklyBuckets[weekKey] && weeklyCount < policy.KeepWeekly {
			weeklyBuckets[weekKey] = true
			weeklyCount++
			keepMap[s.SnapshotID] = true
		}

		// Check monthly
		monthKey := t.Format("2006-01")
		if !monthlyBuckets[monthKey] && monthlyCount < policy.KeepMonthly {
			monthlyBuckets[monthKey] = true
			monthlyCount++
			keepMap[s.SnapshotID] = true
		}
	}

	var plan PrunePlan
	for _, s := range sorted {
		if keepMap[s.SnapshotID] {
			plan.Keep = append(plan.Keep, s)
		} else {
			plan.Delete = append(plan.Delete, s)
		}
	}

	return plan
}
