package services

import (
	"context"
	"fmt"
	"math"

	"go.uber.org/zap"

	"nexus/platform/internal/notification"
)

// notificationRepo is the consumer-side slice of notification.SQLRepository the service needs.
// Declared here (consumer-side) to keep the service unit-testable with an in-memory fake.
type notificationRepo interface {
	List(ctx context.Context, tenantID, userID string, unreadOnly bool, page, limit int) ([]notification.Notification, int, error)
	CountUnread(ctx context.Context, tenantID, userID string) (int, error)
	ExistsForUser(ctx context.Context, tenantID, userID, id string) (bool, error)
	MarkRead(ctx context.Context, tenantID, userID, id string) error
	MarkAllRead(ctx context.Context, tenantID, userID string) (int, error)
}

// NotificationService implements the P0 notification-read use cases:
//   - List (paged, optional unread filter)
//   - UnreadCount
//   - MarkOneRead (idempotent — already-read returns nil)
//   - MarkAllRead
//
// Every method is scoped to (tenant_id, user_id) so no cross-tenant or cross-user data escapes.
type NotificationService struct {
	repo   notificationRepo
	logger *zap.Logger
}

// NewNotificationService wires the service over the notification repository.
func NewNotificationService(repo notificationRepo, logger *zap.Logger) *NotificationService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &NotificationService{repo: repo, logger: logger}
}

// NotificationListResult is the paginated result returned by ListNotifications.
type NotificationListResult struct {
	Items      []notification.Notification
	Page       int
	Limit      int
	Total      int
	TotalPages int
}

// ListNotifications returns the caller's notifications, newest first.
// page/limit default to 1/20 if zero. unreadOnly filters to unread only.
func (s *NotificationService) ListNotifications(
	ctx context.Context, tenantID, userID string,
	unreadOnly bool, page, limit int,
) (*NotificationListResult, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	items, total, err := s.repo.List(ctx, tenantID, userID, unreadOnly, page, limit)
	if err != nil {
		return nil, fmt.Errorf("notification_service.ListNotifications: %w", err)
	}
	totalPages := int(math.Ceil(float64(total) / float64(limit)))
	if totalPages < 1 {
		totalPages = 1
	}
	return &NotificationListResult{
		Items:      items,
		Page:       page,
		Limit:      limit,
		Total:      total,
		TotalPages: totalPages,
	}, nil
}

// UnreadCount returns the number of unread notifications for the caller.
func (s *NotificationService) UnreadCount(ctx context.Context, tenantID, userID string) (int, error) {
	n, err := s.repo.CountUnread(ctx, tenantID, userID)
	if err != nil {
		return 0, fmt.Errorf("notification_service.UnreadCount: %w", err)
	}
	return n, nil
}

// MarkOneRead marks notification id as read for the caller. Returns ErrNotificationNotFound when the
// notification does not exist or does not belong to (tenant, user). Idempotent: already-read → nil.
func (s *NotificationService) MarkOneRead(ctx context.Context, tenantID, userID, id string) error {
	exists, err := s.repo.ExistsForUser(ctx, tenantID, userID, id)
	if err != nil {
		return fmt.Errorf("notification_service.MarkOneRead: %w", err)
	}
	if !exists {
		return ErrNotificationNotFound
	}
	// ExistsForUser confirmed ownership — now mark read (idempotent; 0 rows is fine).
	if err := s.repo.MarkRead(ctx, tenantID, userID, id); err != nil {
		return fmt.Errorf("notification_service.MarkOneRead: %w", err)
	}
	return nil
}

// MarkAllRead marks every unread notification of the caller as read.
// Returns the count of rows updated (0 is fine — no-op).
func (s *NotificationService) MarkAllRead(ctx context.Context, tenantID, userID string) (int, error) {
	n, err := s.repo.MarkAllRead(ctx, tenantID, userID)
	if err != nil {
		return 0, fmt.Errorf("notification_service.MarkAllRead: %w", err)
	}
	return n, nil
}

// ErrNotificationNotFound is returned by MarkOneRead when the notification id is not found for
// the caller in the tenant. The handler maps it to 404 RECORD_NOT_FOUND.
var ErrNotificationNotFound = &ValidationError{Code: "RECORD_NOT_FOUND", Msg: "Thông báo không tồn tại hoặc không thuộc về bạn"}
