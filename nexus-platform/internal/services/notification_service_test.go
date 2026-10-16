package services

import (
	"context"
	"testing"

	"nexus/platform/internal/notification"
)

// --- test doubles -----------------------------------------------------------

type fakeNotificationRepo struct {
	rows      []notification.Notification
	total     int
	unread    int
	existsFor map[string]bool // key: tenantID+"\x00"+userID+"\x00"+id
	marked    []string        // ids passed to MarkRead
	markedAll int             // count returned by MarkAllRead
}

func (f *fakeNotificationRepo) List(_ context.Context, tenantID, userID string, unreadOnly bool, page, limit int) ([]notification.Notification, int, error) {
	return f.rows, f.total, nil
}

func (f *fakeNotificationRepo) CountUnread(_ context.Context, tenantID, userID string) (int, error) {
	return f.unread, nil
}

func (f *fakeNotificationRepo) ExistsForUser(_ context.Context, tenantID, userID, id string) (bool, error) {
	key := tenantID + "\x00" + userID + "\x00" + id
	return f.existsFor[key], nil
}

func (f *fakeNotificationRepo) MarkRead(_ context.Context, tenantID, userID, id string) error {
	f.marked = append(f.marked, id)
	return nil
}

func (f *fakeNotificationRepo) MarkAllRead(_ context.Context, tenantID, userID string) (int, error) {
	return f.markedAll, nil
}

// --- tests ------------------------------------------------------------------

func TestNotificationService_ListNotifications(t *testing.T) {
	tests := []struct {
		name      string
		rows      []notification.Notification
		total     int
		page      int
		limit     int
		wantPages int
	}{
		{
			name:      "returns paginated result",
			rows:      []notification.Notification{{ID: "n1"}, {ID: "n2"}},
			total:     45,
			page:      1,
			limit:     20,
			wantPages: 3, // ceil(45/20)
		},
		{
			name:      "zero total → 1 page",
			rows:      []notification.Notification{},
			total:     0,
			page:      1,
			limit:     20,
			wantPages: 1,
		},
		{
			name:      "default page/limit applied when zero",
			rows:      []notification.Notification{},
			total:     0,
			page:      0,
			limit:     0,
			wantPages: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeNotificationRepo{rows: tt.rows, total: tt.total}
			svc := NewNotificationService(repo, nil)

			res, err := svc.ListNotifications(context.Background(), "t1", "u1", false, tt.page, tt.limit)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.TotalPages != tt.wantPages {
				t.Errorf("TotalPages = %d, want %d", res.TotalPages, tt.wantPages)
			}
			if res.Total != tt.total {
				t.Errorf("Total = %d, want %d", res.Total, tt.total)
			}
		})
	}
}

func TestNotificationService_UnreadCount(t *testing.T) {
	repo := &fakeNotificationRepo{unread: 7}
	svc := NewNotificationService(repo, nil)

	n, err := svc.UnreadCount(context.Background(), "t1", "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 7 {
		t.Errorf("count = %d, want 7", n)
	}
}

func TestNotificationService_MarkOneRead(t *testing.T) {
	tests := []struct {
		name     string
		exists   bool
		wantErr  bool
		wantCode string
		wantMark bool // whether MarkRead should be called
	}{
		{
			name:     "marks existing notification as read",
			exists:   true,
			wantMark: true,
		},
		{
			name:     "returns NOT_FOUND when notification does not belong to caller",
			exists:   false,
			wantErr:  true,
			wantCode: "RECORD_NOT_FOUND",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existsFor := map[string]bool{}
			if tt.exists {
				existsFor["t1\x00u1\x00n-abc"] = true
			}
			repo := &fakeNotificationRepo{existsFor: existsFor}
			svc := NewNotificationService(repo, nil)

			err := svc.MarkOneRead(context.Background(), "t1", "u1", "n-abc")
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				ve, ok := err.(*ValidationError)
				if !ok {
					t.Fatalf("expected *ValidationError, got %T: %v", err, err)
				}
				if ve.Code != tt.wantCode {
					t.Errorf("code = %q, want %q", ve.Code, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantMark && (len(repo.marked) == 0 || repo.marked[0] != "n-abc") {
				t.Error("MarkRead was not called with expected id")
			}
		})
	}
}

func TestNotificationService_MarkOneRead_UserIsolation(t *testing.T) {
	// Notification n-abc belongs to user u1 in tenant t1.
	// User u2 (same tenant) must NOT be able to mark it.
	existsFor := map[string]bool{
		"t1\x00u1\x00n-abc": true,
		// u2 does NOT have this key → ExistsForUser returns false for (t1, u2, n-abc)
	}
	repo := &fakeNotificationRepo{existsFor: existsFor}
	svc := NewNotificationService(repo, nil)

	err := svc.MarkOneRead(context.Background(), "t1", "u2", "n-abc")
	if err == nil {
		t.Fatal("expected NOT_FOUND for wrong user, got nil")
	}
	ve, ok := err.(*ValidationError)
	if !ok || ve.Code != "RECORD_NOT_FOUND" {
		t.Errorf("expected RECORD_NOT_FOUND, got %v", err)
	}
	if len(repo.marked) > 0 {
		t.Error("MarkRead must not be called when notification does not belong to caller")
	}
}

func TestNotificationService_MarkAllRead(t *testing.T) {
	repo := &fakeNotificationRepo{markedAll: 5}
	svc := NewNotificationService(repo, nil)

	n, err := svc.MarkAllRead(context.Background(), "t1", "u1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 5 {
		t.Errorf("updated = %d, want 5", n)
	}
}
