package notification

import "testing"

func sp(s string) *string { return &s }

// A notification you cannot click through to the thing it is about is half a notification. Before
// this, entityName/recordId shipped but no href was computed, so every row linked to the generic
// inbox — an assignment notice never took you to the assigned record.
func TestNotification_DeepLink(t *testing.T) {
	cases := []struct {
		name string
		n    Notification
		want string
	}{
		{"record link when entity+record present", Notification{EntityName: sp("crm_leads"), RecordID: sp("r1"), Type: sp("assignment")}, "/crm_leads/r1"},
		{"approval type with no record → inbox", Notification{Type: sp("approval")}, "/approvals"},
		{"assignment type with no record → inbox", Notification{Type: sp("assignment")}, "/approvals"},
		{"nothing to point at → empty (FE falls back)", Notification{Type: sp("system")}, ""},
		{"record wins over type", Notification{EntityName: sp("tk_tickets"), RecordID: sp("t9"), Type: sp("approval")}, "/tk_tickets/t9"},
		{"entity without record does not build a broken link", Notification{EntityName: sp("crm_leads"), Type: sp("system")}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.n.DeepLink(); got != tc.want {
				t.Errorf("DeepLink() = %q, want %q", got, tc.want)
			}
		})
	}
}
