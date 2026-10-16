package dynrepo

import "testing"

// TestClampPageLimit locks the pagination guard (GD1 bug #11). ClampPageLimit is an exported,
// reachable security/DoS guard (a raw ?limit/?page from the query string flows into the SQL
// LIMIT/OFFSET via ListParams.normalize), but every test that exercises it lives behind the
// //go:build integration tag, so the clamp branches never ran in the default `go test ./...`
// gate. This pure-function table test runs in the default gate and turns any silent regression
// (e.g. ">100" → ">=100", or dropping the default) into a red test.
func TestClampPageLimit(t *testing.T) {
	cases := []struct {
		name                             string
		page, limit, wantPage, wantLimit int
	}{
		{"zero defaults", 0, 0, 1, 20},
		{"negative defaults", -5, -5, 1, 20},
		{"over max clamps", 1, 101, 1, 100},
		{"dos huge clamps", 2, 1000000, 2, 100},
		{"in bounds passthrough", 1, 50, 1, 50},
		{"neg page zero limit", -1, 0, 1, 20},
		{"max boundary stays", 1, 100, 1, 100},
		{"limit one ok", 3, 1, 3, 1},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			gp, gl := ClampPageLimit(c.page, c.limit)
			if gp != c.wantPage || gl != c.wantLimit {
				t.Fatalf("ClampPageLimit(%d,%d)=(%d,%d), want (%d,%d)", c.page, c.limit, gp, gl, c.wantPage, c.wantLimit)
			}
		})
	}
}

// TestListParams_normalize locks the ORDER BY sort-column allowlist (GD1 bug #21 + RM-09).
// Because an ORDER BY column name cannot be parameterized with ?, it is interpolated into SQL,
// so the allowlist check is the ONLY defense against SQL injection on ?sort. RM-09 changed the
// behavior for unknown columns: instead of silently falling back to created_at, normalize now
// returns ErrUnsortableField so the service can return 422 (never a silent wrong-order response).
// When SortAllowlist is nil, the built-in system-column allowlist (created_at/updated_at/id) is
// used. Dynamic entity columns are added by the service via SortAllowlist.
func TestListParams_normalize(t *testing.T) {
	t.Run("system columns allowed (no SortAllowlist)", func(t *testing.T) {
		cases := []struct {
			name, sort, wantCol string
		}{
			{"created_at allowed", "created_at", "created_at"},
			{"updated_at allowed", "updated_at", "updated_at"},
			{"id allowed", "id", "id"},
			{"empty falls back to created_at", "", "created_at"},
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				_, _, col, _, err := ListParams{Sort: c.sort, Page: 1, Limit: 20}.normalize()
				if err != nil {
					t.Fatalf("normalize(sort=%q): unexpected err: %v", c.sort, err)
				}
				if col != c.wantCol {
					t.Fatalf("normalize(sort=%q): col=%q, want %q", c.sort, col, c.wantCol)
				}
			})
		}
	})

	t.Run("unknown column returns ErrUnsortableField (RM-09: no silent fallback)", func(t *testing.T) {
		cases := []struct {
			name, sort string
		}{
			{"business field not in allowlist", "name"},
			{"json expr injection attempt", "data->>$.x"},
			{"sql injection", "id; DROP TABLE obj_x"},
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				_, _, _, _, err := ListParams{Sort: c.sort, Page: 1, Limit: 20}.normalize()
				if err != ErrUnsortableField {
					t.Fatalf("normalize(sort=%q): err=%v, want ErrUnsortableField", c.sort, err)
				}
			})
		}
	})

	t.Run("custom SortAllowlist extends allowed columns", func(t *testing.T) {
		allowlist := map[string]string{
			"created_at": "created_at",
			"code":       "code",       // unique generated column
			"account":    "account_id", // relation generated column
		}
		cases := []struct {
			name, sort, wantCol string
		}{
			{"system col still allowed", "created_at", "created_at"},
			{"unique field col allowed", "code", "code"},
			{"relation field translates to _id", "account", "account_id"},
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				_, _, col, _, err := ListParams{Sort: c.sort, Page: 1, Limit: 20, SortAllowlist: allowlist}.normalize()
				if err != nil {
					t.Fatalf("normalize(sort=%q): unexpected err: %v", c.sort, err)
				}
				if col != c.wantCol {
					t.Fatalf("normalize(sort=%q): col=%q, want %q", c.sort, col, c.wantCol)
				}
			})
		}
	})

	t.Run("order direction", func(t *testing.T) {
		cases := []struct {
			name, order, want string
		}{
			{"asc lower", "asc", "ASC"},
			{"asc upper", "ASC", "ASC"},
			{"desc lower", "desc", "DESC"},
			{"desc upper", "DESC", "DESC"},
			{"empty defaults desc", "", "DESC"},
			{"garbage defaults desc", "garbage", "DESC"},
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				_, _, _, ord, err := ListParams{Sort: "created_at", Order: c.order, Page: 1, Limit: 20}.normalize()
				if err != nil {
					t.Fatalf("normalize(order=%q): unexpected err: %v", c.order, err)
				}
				if ord != c.want {
					t.Fatalf("normalize(order=%q): order=%q, want %q", c.order, ord, c.want)
				}
			})
		}
	})

	t.Run("pagination and offset math", func(t *testing.T) {
		cases := []struct {
			name                               string
			page, limit, wantLimit, wantOffset int
		}{
			{"first page", 1, 20, 20, 0},
			{"second page", 2, 20, 20, 20},
			{"third page custom limit", 3, 50, 50, 100},
			{"zero page clamps to 1 (offset 0)", 0, 20, 20, 0},
			{"over-max limit clamps then offset uses 100", 2, 1000000, 100, 100},
		}
		for _, c := range cases {
			c := c
			t.Run(c.name, func(t *testing.T) {
				limit, offset, _, _, err := ListParams{Sort: "created_at", Page: c.page, Limit: c.limit}.normalize()
				if err != nil {
					t.Fatalf("normalize pagination: unexpected err: %v", err)
				}
				if limit != c.wantLimit || offset != c.wantOffset {
					t.Fatalf("normalize(page=%d,limit=%d): limit=%d offset=%d, want limit=%d offset=%d",
						c.page, c.limit, limit, offset, c.wantLimit, c.wantOffset)
				}
			})
		}
	})
}
