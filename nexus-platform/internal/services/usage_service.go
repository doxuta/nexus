package services

import (
	"context"
	"fmt"
	"time"

	"nexus/platform/internal/usage"
)

const (
	usageDefaultDays = 30
	usageMaxDays     = 366
	usageDayLayout   = "2006-01-02"
)

// usageReadStore is the service's consumer-side view of *usage.Repository.
type usageReadStore interface {
	usageStore
	Range(ctx context.Context, tenantID, fromDay, toDay string) ([]usage.DayValue, error)
	LastRollupAt(ctx context.Context, tenantID, sinceDay string) (*time.Time, error)
}

// UsageCounts holds the eight counters of one day (or of the whole range).
type UsageCounts struct {
	WorkflowRuns       int64 `json:"workflowRuns"`
	WorkflowRunsFailed int64 `json:"workflowRunsFailed"`
	EmailsSent         int64 `json:"emailsSent"`
	EmailsFailed       int64 `json:"emailsFailed"`
	WebhooksDelivered  int64 `json:"webhooksDelivered"`
	WebhooksFailed     int64 `json:"webhooksFailed"`
	HooksReceived      int64 `json:"hooksReceived"`
	HooksFailed        int64 `json:"hooksFailed"`
	// UsersSeen is distinct people per day; it is NOT summed into Totals (a person seen on two days
	// would count twice) — the range total of people is current.usersSeen30d.
	UsersSeen int64 `json:"usersSeen"`
}

func (c *UsageCounts) add(metric string, v int64) {
	switch metric {
	case usage.WorkflowRuns:
		c.WorkflowRuns += v
	case usage.WorkflowRunsFailed:
		c.WorkflowRunsFailed += v
	case usage.EmailsSent:
		c.EmailsSent += v
	case usage.EmailsFailed:
		c.EmailsFailed += v
	case usage.WebhooksDelivered:
		c.WebhooksDelivered += v
	case usage.WebhooksFailed:
		c.WebhooksFailed += v
	case usage.HooksReceived:
		c.HooksReceived += v
	case usage.HooksFailed:
		c.HooksFailed += v
	case usage.UsersSeen:
		c.UsersSeen += v
	}
}

func (c *UsageCounts) plus(o UsageCounts) {
	c.WorkflowRuns += o.WorkflowRuns
	c.WorkflowRunsFailed += o.WorkflowRunsFailed
	c.EmailsSent += o.EmailsSent
	c.EmailsFailed += o.EmailsFailed
	c.WebhooksDelivered += o.WebhooksDelivered
	c.WebhooksFailed += o.WebhooksFailed
	c.HooksReceived += o.HooksReceived
	c.HooksFailed += o.HooksFailed
}

// UsageDay is one calendar day of the response.
type UsageDay struct {
	Day string `json:"day"`
	UsageCounts
}

// UsageReport is the body of GET /admin/usage.
type UsageReport struct {
	Timezone      string        `json:"timezone"`
	From          string        `json:"from"`
	To            string        `json:"to"`
	RollupEnabled bool          `json:"rollupEnabled"`
	LastRollupAt  *time.Time    `json:"lastRollupAt"`
	Days          []UsageDay    `json:"days"`
	Totals        UsageCounts   `json:"totals"`
	Current       *UsageCurrent `json:"current,omitempty"`
}

// UsageService answers the "Mức dùng" screen: stored days from sys_usage_daily, today counted live.
type UsageService struct {
	store         usageReadStore
	rollupEnabled bool
	now           func() time.Time
	gauges        *UsageGaugeSource
}

// WithGauges turns on the "current" block (specs/066 L5). Without it the report has none.
func (s *UsageService) WithGauges(g *UsageGaugeSource) *UsageService {
	s.gauges = g
	return s
}

// NewUsageService wires the service. rollupEnabled mirrors USAGE_ROLLUP_INTERVAL_SEC > 0.
func NewUsageService(store usageReadStore, rollupEnabled bool) *UsageService {
	return &UsageService{store: store, rollupEnabled: rollupEnabled, now: time.Now}
}

func parseUsageDay(label, s string) (time.Time, error) {
	t, err := time.Parse(usageDayLayout, s)
	if err != nil {
		return time.Time{}, invalid("USAGE_RANGE_INVALID", "%s phải có dạng YYYY-MM-DD", label)
	}
	return t, nil
}

// Daily returns the dense per-day list for [from, to] (YYYY-MM-DD, "" = default 30 days ending
// today). A day with nothing stored is a row of zeros. The tenant is the caller's verified one.
func (s *UsageService) Daily(ctx context.Context, tenantID, from, to string) (*UsageReport, error) {
	todayStr, todayFrom, todayTo := usageDayBounds(s.now())
	today, _ := time.Parse(usageDayLayout, todayStr)

	toDay := today
	if to != "" {
		t, err := parseUsageDay("to", to)
		if err != nil {
			return nil, err
		}
		toDay = t
	}
	fromDay := toDay.AddDate(0, 0, -(usageDefaultDays - 1))
	if from != "" {
		f, err := parseUsageDay("from", from)
		if err != nil {
			return nil, err
		}
		fromDay = f
	}
	switch {
	case toDay.After(today):
		return nil, invalid("USAGE_RANGE_INVALID", "to không được sau hôm nay")
	case fromDay.After(toDay):
		return nil, invalid("USAGE_RANGE_INVALID", "from không được sau to")
	case int(toDay.Sub(fromDay).Hours()/24)+1 > usageMaxDays:
		return nil, invalid("USAGE_RANGE_INVALID", "khoảng tối đa %d ngày", usageMaxDays)
	}

	byDay := map[string]*UsageCounts{}
	yesterday := today.AddDate(0, 0, -1)
	if !fromDay.After(yesterday) {
		stored := toDay
		if stored.After(yesterday) {
			stored = yesterday
		}
		vals, err := s.store.Range(ctx, tenantID, fromDay.Format(usageDayLayout), stored.Format(usageDayLayout))
		if err != nil {
			return nil, fmt.Errorf("services.UsageService.Daily: %w", err)
		}
		for _, v := range vals {
			c := byDay[v.Day]
			if c == nil {
				c = &UsageCounts{}
				byDay[v.Day] = c
			}
			c.add(v.Metric, v.Value)
		}
	}
	if toDay.Equal(today) {
		live, err := s.store.CountersForDay(ctx, tenantID, todayFrom, todayTo)
		if err != nil {
			return nil, fmt.Errorf("services.UsageService.Daily: today: %w", err)
		}
		c := &UsageCounts{}
		for k, v := range live {
			c.add(k.Metric, v)
		}
		byDay[todayStr] = c
	}

	rep := &UsageReport{
		Timezone: appLocation().String(), From: fromDay.Format(usageDayLayout), To: toDay.Format(usageDayLayout),
		RollupEnabled: s.rollupEnabled, Days: []UsageDay{},
	}
	for d := fromDay; !d.After(toDay); d = d.AddDate(0, 0, 1) {
		key := d.Format(usageDayLayout)
		row := UsageDay{Day: key}
		if c := byDay[key]; c != nil {
			row.UsageCounts = *c
		}
		rep.Totals.plus(row.UsageCounts)
		rep.Days = append(rep.Days, row)
	}
	last, err := s.store.LastRollupAt(ctx, tenantID, yesterday.Format(usageDayLayout))
	if err != nil {
		return nil, fmt.Errorf("services.UsageService.Daily: %w", err)
	}
	rep.LastRollupAt = last
	if s.gauges != nil {
		if rep.Current, err = s.gauges.current(ctx, tenantID, s.now()); err != nil {
			return nil, fmt.Errorf("services.UsageService.Daily: %w", err)
		}
	}
	return rep, nil
}
