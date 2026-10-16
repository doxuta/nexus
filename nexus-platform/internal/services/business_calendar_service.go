package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"nexus/platform/internal/calendar"
	"nexus/platform/internal/calmeta"
)

// calendarStore is the consumer-side slice of the calendar repository
// (calmeta.SQLRepository in production). Every method is tenant-scoped.
type calendarStore interface {
	Create(ctx context.Context, in calmeta.CreateInput) (string, error)
	GetByID(ctx context.Context, tenantID, id string) (*calmeta.BusinessCalendar, error)
	List(ctx context.Context, tenantID string) ([]calmeta.BusinessCalendar, error)
	Update(ctx context.Context, tenantID, id string, in calmeta.UpdateInput) error
	SoftDelete(ctx context.Context, tenantID, id string) error
	Usages(ctx context.Context, tenantID, id string) ([]calmeta.Usage, error)
}

// Column-width caps for sys_business_calendars. Name is capped at 80 runes (not the full
// VARCHAR(100)) so the soft-delete tombstone suffix ("__deleted_<id8>", 18 chars) always
// fits — same discipline as the app slug cap.
const (
	maxCalendarNameRunes = 80
	maxCalendarTzRunes   = 64  // sys_business_calendars.timezone VARCHAR(64)
	maxCalendarHolidays  = 366 // sanity cap: one year of dates per calendar is plenty
)

// BusinessCalendarService implements the admin CRUD over sys_business_calendars
// (CALENDAR-1). Config-time validation goes through calendar.FromJSON — the SAME parse
// body the run-time consumers (SLA/timer engine, next slice) use on stored rows, so a
// calendar that saves can never fail to parse later (validator parity by construction).
//
// Fallback contract (documented for slice 1b): a tenant with NO calendar — or none flagged
// default — makes the SLA engine fall back to plain WALL-CLOCK arithmetic. This service
// never seeds calendars; existing tenants are unaffected until an admin creates one.
type BusinessCalendarService struct {
	repo   calendarStore
	logger *zap.Logger
}

// NewBusinessCalendarService wires the calendar service.
func NewBusinessCalendarService(repo calendarStore, logger *zap.Logger) *BusinessCalendarService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &BusinessCalendarService{repo: repo, logger: logger}
}

// CalendarInput is the create/update payload (full-replace on update).
type CalendarInput struct {
	Name      string
	Timezone  string
	Hours     map[string][]calendar.Interval
	Holidays  []string
	IsDefault bool
	ActorID   string
}

// validateCalendarInput normalizes and validates the payload, returning the canonical
// hours/holidays JSON to store. All shape/semantics checks ride calendar.FromJSON (one
// body with the run-time parser); the sentinel errors map to the CALENDAR_* codes.
func validateCalendarInput(in *CalendarInput) (hoursJSON, holidaysJSON []byte, err error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Timezone = strings.TrimSpace(in.Timezone)
	if in.Name == "" {
		return nil, nil, invalid("CALENDAR_NAME_REQUIRED", "tên lịch làm việc là bắt buộc")
	}
	if len([]rune(in.Name)) > maxCalendarNameRunes {
		return nil, nil, invalid("CALENDAR_FIELD_TOO_LONG", "tên lịch vượt quá %d ký tự", maxCalendarNameRunes)
	}
	if len([]rune(in.Timezone)) > maxCalendarTzRunes {
		return nil, nil, invalid("CALENDAR_FIELD_TOO_LONG", "timezone vượt quá %d ký tự", maxCalendarTzRunes)
	}
	if len(in.Holidays) > maxCalendarHolidays {
		return nil, nil, invalid("CALENDAR_HOLIDAYS_INVALID", "tối đa %d ngày lễ mỗi lịch", maxCalendarHolidays)
	}
	if in.Hours == nil {
		in.Hours = map[string][]calendar.Interval{}
	}
	hoursJSON, err = json.Marshal(in.Hours)
	if err != nil {
		return nil, nil, fmt.Errorf("services.validateCalendarInput: marshal hours: %w", err)
	}
	if len(in.Holidays) > 0 {
		if holidaysJSON, err = json.Marshal(in.Holidays); err != nil {
			return nil, nil, fmt.Errorf("services.validateCalendarInput: marshal holidays: %w", err)
		}
	}
	sched, err := calendar.FromJSON(in.Timezone, hoursJSON, holidaysJSON)
	if err != nil {
		switch {
		case errors.Is(err, calendar.ErrTimezoneInvalid):
			return nil, nil, invalid("CALENDAR_TIMEZONE_INVALID", "timezone %q không hợp lệ (IANA, vd Asia/Ho_Chi_Minh)", in.Timezone)
		case errors.Is(err, calendar.ErrHolidaysInvalid):
			return nil, nil, invalid("CALENDAR_HOLIDAYS_INVALID", "ngày lễ không hợp lệ: %v", err)
		default: // ErrHoursInvalid and any parse fault
			return nil, nil, invalid("CALENDAR_HOURS_INVALID", "giờ làm việc không hợp lệ: %v", err)
		}
	}
	if !sched.HasWorkingHours() {
		return nil, nil, invalid("CALENDAR_HOURS_EMPTY", "lịch phải có ít nhất một khoảng giờ làm việc")
	}
	return hoursJSON, holidaysJSON, nil
}

// CreateCalendar validates and inserts a calendar. A taken (tenant, name) slot is
// CALENDAR_ALREADY_EXISTS; setting isDefault clears the tenant's previous default in the
// same repo transaction.
func (s *BusinessCalendarService) CreateCalendar(ctx context.Context, tenantID string, in CalendarInput) (*calmeta.BusinessCalendar, error) {
	hoursJSON, holidaysJSON, err := validateCalendarInput(&in)
	if err != nil {
		return nil, err
	}
	id, err := s.repo.Create(ctx, calmeta.CreateInput{
		TenantID:  tenantID,
		Name:      in.Name,
		Timezone:  in.Timezone,
		Hours:     hoursJSON,
		Holidays:  holidaysJSON,
		IsDefault: in.IsDefault,
		CreatedBy: strings.TrimSpace(in.ActorID),
	})
	if err != nil {
		if errors.Is(err, calmeta.ErrCalendarDuplicateName) {
			return nil, invalid("CALENDAR_ALREADY_EXISTS", "lịch %q đã tồn tại", in.Name)
		}
		return nil, fmt.Errorf("services.BusinessCalendarService.CreateCalendar: %w", err)
	}
	cal, err := s.repo.GetByID(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("services.BusinessCalendarService.CreateCalendar: read back: %w", err)
	}
	return cal, nil
}

// ListCalendars returns every calendar in the tenant (default first).
func (s *BusinessCalendarService) ListCalendars(ctx context.Context, tenantID string) ([]calmeta.BusinessCalendar, error) {
	cals, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.BusinessCalendarService.ListCalendars: %w", err)
	}
	return cals, nil
}

// GetCalendar returns one tenant-scoped calendar. Unknown/cross-tenant id → CALENDAR_NOT_FOUND.
func (s *BusinessCalendarService) GetCalendar(ctx context.Context, tenantID, id string) (*calmeta.BusinessCalendar, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("CALENDAR_NOT_FOUND", "không tìm thấy lịch làm việc")
	}
	cal, err := s.repo.GetByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, calmeta.ErrCalendarNotFound) {
			return nil, invalid("CALENDAR_NOT_FOUND", "không tìm thấy lịch làm việc")
		}
		return nil, fmt.Errorf("services.BusinessCalendarService.GetCalendar: %w", err)
	}
	return cal, nil
}

// UpdateCalendar full-replaces the mutable fields of a tenant-scoped calendar. Setting
// isDefault clears the tenant's previous default in the same repo transaction; clearing it
// simply leaves the tenant with no default (SLA falls back to wall-clock).
func (s *BusinessCalendarService) UpdateCalendar(ctx context.Context, tenantID, id string, in CalendarInput) (*calmeta.BusinessCalendar, error) {
	id = strings.TrimSpace(id)
	hoursJSON, holidaysJSON, err := validateCalendarInput(&in)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Update(ctx, tenantID, id, calmeta.UpdateInput{
		Name:      in.Name,
		Timezone:  in.Timezone,
		Hours:     hoursJSON,
		Holidays:  holidaysJSON,
		IsDefault: in.IsDefault,
		UpdatedBy: strings.TrimSpace(in.ActorID),
	}); err != nil {
		switch {
		case errors.Is(err, calmeta.ErrCalendarNotFound):
			return nil, invalid("CALENDAR_NOT_FOUND", "không tìm thấy lịch làm việc")
		case errors.Is(err, calmeta.ErrCalendarDuplicateName):
			return nil, invalid("CALENDAR_ALREADY_EXISTS", "lịch %q đã tồn tại", in.Name)
		}
		return nil, fmt.Errorf("services.BusinessCalendarService.UpdateCalendar: %w", err)
	}
	cal, err := s.repo.GetByID(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("services.BusinessCalendarService.UpdateCalendar: read back: %w", err)
	}
	return cal, nil
}

// DeleteCalendar soft-deletes a tenant-scoped calendar. If it was the default, the tenant
// is left with no default — the SLA engine's wall-clock fallback, never a dangling ref.
func (s *BusinessCalendarService) DeleteCalendar(ctx context.Context, tenantID, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return invalid("CALENDAR_NOT_FOUND", "không tìm thấy lịch làm việc")
	}
	// Refuse while a live rule pins this calendar (#326, specs/037 §6 #13): deleting it would leave
	// the rule on wall-clock and 422 its next save. Same shape as CREDENTIAL_IN_USE (specs/050).
	// An unknown/cross-tenant id has no usages, so it falls through to the 404 below.
	// ponytail: check-then-delete — a rule pinned between the two statements keeps a dangling id,
	// which run time reports as a wall-clock fallback with a warning; a reference table closes it.
	usages, err := s.repo.Usages(ctx, tenantID, id)
	if err != nil {
		return fmt.Errorf("services.BusinessCalendarService.DeleteCalendar: %w", err)
	}
	if len(usages) > 0 {
		names := make([]string, len(usages))
		for i, u := range usages {
			names[i] = u.WorkflowName
		}
		return &ValidationError{
			Code:    "CALENDAR_IN_USE",
			Msg:     "Lịch đang được các luật sau dùng: " + strings.Join(names, ", ") + " — gỡ lịch khỏi các luật đó trước",
			Details: map[string]any{"usages": usages},
		}
	}
	if err := s.repo.SoftDelete(ctx, tenantID, id); err != nil {
		if errors.Is(err, calmeta.ErrCalendarNotFound) {
			return invalid("CALENDAR_NOT_FOUND", "không tìm thấy lịch làm việc")
		}
		return fmt.Errorf("services.BusinessCalendarService.DeleteCalendar: %w", err)
	}
	return nil
}
