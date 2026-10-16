// usage_gauge.go — specs/066 L5: the instant half of "Mức dùng" (active users, sign-ins in 30 days,
// live rows per table, attachment bytes). The roller stores one snapshot per tick under today's day;
// the service reads the latest one, or counts live when none exists yet.
package services

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/usage"
)

// usageSeenWindowDays is the "sign-ins in the last N days" window, today included.
const usageSeenWindowDays = 30

// usageGaugeStore is the consumer-side view of the gauge methods of *usage.Repository.
type usageGaugeStore interface {
	Gauges(ctx context.Context, tenantID string, seenFromUTC time.Time, entities []string, logger *zap.Logger) (*usage.Gauge, error)
	LatestGauge(ctx context.Context, tenantID, day string) (*usage.Gauge, *time.Time, error)
}

// usageEntityLister reads the tenant's entity metadata (name + business label). Satisfied by
// *metadata.Repository.
type usageEntityLister interface {
	ListEntities(tenantID string) ([]metadata.EntityDefinition, error)
}

// UsageGaugeSource bundles what both the roller and the service need to read gauges.
type UsageGaugeSource struct {
	Store    usageGaugeStore
	Entities usageEntityLister
	Logger   *zap.Logger
}

// take counts the tenant's gauges live. The table names come from the tenant's own metadata.
func (g *UsageGaugeSource) take(ctx context.Context, tenantID string, now time.Time) (*usage.Gauge, error) {
	defs, err := g.Entities.ListEntities(tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.UsageGaugeSource.take: %w", err)
	}
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	_, todayFrom, _ := usageDayBounds(now)
	got, err := g.Store.Gauges(ctx, tenantID, todayFrom.AddDate(0, 0, -(usageSeenWindowDays-1)), names, g.Logger)
	if err != nil {
		return nil, fmt.Errorf("services.UsageGaugeSource.take: %w", err)
	}
	return got, nil
}

// UsageRecordCount is one table of the "records" list, with its business label.
type UsageRecordCount struct {
	Entity string `json:"entity"`
	Label  string `json:"label"`
	Count  int64  `json:"count"`
}

// UsageCurrent is the "Hiện tại" block of GET /admin/usage.
type UsageCurrent struct {
	UsersActive  int64              `json:"usersActive"`
	UsersSeen30d int64              `json:"usersSeen30d"`
	RecordsTotal int64              `json:"recordsTotal"`
	Records      []UsageRecordCount `json:"records"`
	FileBytes    int64              `json:"fileBytes"`
	FileCount    int64              `json:"fileCount"`
	ComputedAt   time.Time          `json:"computedAt"`
}

// current builds the block from a stored or live gauge. An entity deleted since the snapshot is
// dropped (the label comes from the live metadata, so a missing one is a gone table).
func (g *UsageGaugeSource) current(ctx context.Context, tenantID string, now time.Time) (*UsageCurrent, error) {
	day, _, _ := usageDayBounds(now)
	gauge, at, err := g.Store.LatestGauge(ctx, tenantID, day)
	if err != nil {
		return nil, fmt.Errorf("services.UsageGaugeSource.current: %w", err)
	}
	computed := now
	if gauge == nil {
		if gauge, err = g.take(ctx, tenantID, now); err != nil {
			return nil, fmt.Errorf("services.UsageGaugeSource.current: %w", err)
		}
	} else if at != nil {
		computed = *at
	}
	defs, err := g.Entities.ListEntities(tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.UsageGaugeSource.current: %w", err)
	}
	labels := make(map[string]string, len(defs))
	for _, d := range defs {
		labels[d.Name] = d.Label
	}
	out := &UsageCurrent{
		UsersActive: gauge.UsersActive, UsersSeen30d: gauge.UsersSeen30d, Records: []UsageRecordCount{},
		FileBytes: gauge.FileBytes, FileCount: gauge.FileCount, ComputedAt: computed,
	}
	for _, r := range gauge.Records {
		label, ok := labels[r.Entity]
		if !ok {
			continue
		}
		if label == "" {
			label = r.Entity
		}
		out.Records = append(out.Records, UsageRecordCount{Entity: r.Entity, Label: label, Count: r.Count})
		out.RecordsTotal += r.Count
	}
	return out, nil
}
