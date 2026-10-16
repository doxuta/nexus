package calendar

// Unit tests — PURE math, no DB, machine-tz independent (every instant is constructed with
// an explicit *time.Location). Fixed 2026 anchors used throughout:
//
//	2026-07-13 Mon · 2026-07-15 Wed · 2026-07-17 Fri · 2026-07-18 Sat · 2026-07-20 Mon
//	2026-09-02 Wed (VN National Day — the holiday fixture)
//	America/New_York DST 2026: spring-forward Sun Mar 8 (02:00→03:00), fall-back Sun Nov 1.

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// vnConfig is the reference calendar: Asia/Ho_Chi_Minh (UTC+7, no DST), Mon–Fri
// 08:00–12:00 + 13:00–17:00 (8h/day with lunch break), sat/sun off.
func vnConfig(holidays ...string) Config {
	day := []Interval{{Start: "08:00", End: "12:00"}, {Start: "13:00", End: "17:00"}}
	return Config{
		Timezone: "Asia/Ho_Chi_Minh",
		Hours: map[string][]Interval{
			"mon": day, "tue": day, "wed": day, "thu": day, "fri": day,
		},
		Holidays: holidays,
	}
}

// fullWeekConfig is a 24/7 calendar (all days 00:00–24:00) in the given tz.
func fullWeekConfig(tz string) Config {
	full := []Interval{{Start: "00:00", End: "24:00"}}
	return Config{Timezone: tz, Hours: map[string][]Interval{
		"mon": full, "tue": full, "wed": full, "thu": full, "fri": full, "sat": full, "sun": full,
	}}
}

func mustSchedule(t *testing.T, cfg Config) *Schedule {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	return s
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%s): %v", name, err)
	}
	return loc
}

func TestAddBusinessDuration_ReferenceCalendar(t *testing.T) {
	vn := mustLoc(t, "Asia/Ho_Chi_Minh")
	sched := mustSchedule(t, vnConfig("2026-09-02"))

	tests := []struct {
		name  string
		start time.Time
		d     time.Duration
		want  time.Time
	}{
		{
			name:  "mid-morning +2h stays same day",
			start: time.Date(2026, 7, 15, 9, 0, 0, 0, vn),
			d:     2 * time.Hour,
			want:  time.Date(2026, 7, 15, 11, 0, 0, 0, vn),
		},
		{
			name:  "crossing lunch skips the break",
			start: time.Date(2026, 7, 15, 10, 0, 0, 0, vn),
			d:     3 * time.Hour,
			want:  time.Date(2026, 7, 15, 14, 0, 0, 0, vn),
		},
		{
			name:  "exactly consuming the morning lands on interval end",
			start: time.Date(2026, 7, 15, 9, 0, 0, 0, vn),
			d:     3 * time.Hour,
			want:  time.Date(2026, 7, 15, 12, 0, 0, 0, vn),
		},
		{
			name:  "Friday 16:00 +4h lands Monday 11:00",
			start: time.Date(2026, 7, 17, 16, 0, 0, 0, vn),
			d:     4 * time.Hour,
			want:  time.Date(2026, 7, 20, 11, 0, 0, 0, vn),
		},
		{
			name:  "start on a holiday advances to the next working day",
			start: time.Date(2026, 9, 2, 9, 0, 0, 0, vn),
			d:     time.Hour,
			want:  time.Date(2026, 9, 3, 9, 0, 0, 0, vn),
		},
		{
			name:  "start after hours (20:00) advances to next morning",
			start: time.Date(2026, 7, 14, 20, 0, 0, 0, vn),
			d:     time.Hour,
			want:  time.Date(2026, 7, 15, 9, 0, 0, 0, vn),
		},
		{
			name:  "start before hours (06:00) advances to 08:00 same day",
			start: time.Date(2026, 7, 15, 6, 0, 0, 0, vn),
			d:     30 * time.Minute,
			want:  time.Date(2026, 7, 15, 8, 30, 0, 0, vn),
		},
		{
			name:  "Saturday start advances to Monday",
			start: time.Date(2026, 7, 18, 10, 0, 0, 0, vn),
			d:     time.Hour,
			want:  time.Date(2026, 7, 20, 9, 0, 0, 0, vn),
		},
		{
			name:  "multi-week: 80 business hours = 10 working days",
			start: time.Date(2026, 7, 13, 8, 0, 0, 0, vn),
			d:     80 * time.Hour,
			want:  time.Date(2026, 7, 24, 17, 0, 0, 0, vn),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sched.AddBusinessDuration(tt.start, tt.d)
			if err != nil {
				t.Fatalf("AddBusinessDuration: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("got %v; want %v", got.In(sched.Location()), tt.want)
			}
		})
	}
}

func TestAddBusinessDuration_FullWeekEqualsWallClock(t *testing.T) {
	vn := mustLoc(t, "Asia/Ho_Chi_Minh")
	sched := mustSchedule(t, fullWeekConfig("Asia/Ho_Chi_Minh"))
	start := time.Date(2026, 7, 15, 21, 37, 0, 0, vn)
	got, err := sched.AddBusinessDuration(start, 100*time.Hour)
	if err != nil {
		t.Fatalf("AddBusinessDuration: %v", err)
	}
	if want := start.Add(100 * time.Hour); !got.Equal(want) {
		t.Fatalf("24/7 calendar must equal wall-clock: got %v; want %v", got, want)
	}
}

func TestAddBusinessDuration_DST(t *testing.T) {
	ny := mustLoc(t, "America/New_York")

	t.Run("deadline crossing spring-forward lands on correct local wall-clock", func(t *testing.T) {
		// 09:00–17:00 all 7 days. Sat Mar 7 16:00 EST + 2h: 1h consumed Saturday,
		// the rest resumes Sunday 09:00 EDT → 10:00 EDT (NOT an offset-shifted 09:00/11:00).
		day := []Interval{{Start: "09:00", End: "17:00"}}
		sched := mustSchedule(t, Config{Timezone: "America/New_York", Hours: map[string][]Interval{
			"mon": day, "tue": day, "wed": day, "thu": day, "fri": day, "sat": day, "sun": day,
		}})
		start := time.Date(2026, 3, 7, 16, 0, 0, 0, ny) // EST
		got, err := sched.AddBusinessDuration(start, 2*time.Hour)
		if err != nil {
			t.Fatalf("AddBusinessDuration: %v", err)
		}
		want := time.Date(2026, 3, 8, 10, 0, 0, 0, ny) // EDT
		if !got.Equal(want) {
			t.Fatalf("got %v; want %v", got.In(ny), want)
		}
	})

	t.Run("window spanning the spring-forward gap is physically one hour shorter", func(t *testing.T) {
		// sun 01:00–04:00: on Mar 8 the physical window is 2h (02:00–03:00 does not exist).
		sched := mustSchedule(t, Config{Timezone: "America/New_York", Hours: map[string][]Interval{
			"sun": {{Start: "01:00", End: "04:00"}},
		}})
		start := time.Date(2026, 3, 8, 1, 0, 0, 0, ny) // 01:00 EST
		got, err := sched.AddBusinessDuration(start, 2*time.Hour)
		if err != nil {
			t.Fatalf("AddBusinessDuration(2h): %v", err)
		}
		if want := time.Date(2026, 3, 8, 4, 0, 0, 0, ny); !got.Equal(want) { // 04:00 EDT
			t.Fatalf("2h: got %v; want %v (window end)", got.In(ny), want)
		}
		// 2h30m does NOT fit the 2h physical window → spills to next Sunday 01:00 EDT + 30m.
		got, err = sched.AddBusinessDuration(start, 2*time.Hour+30*time.Minute)
		if err != nil {
			t.Fatalf("AddBusinessDuration(2h30m): %v", err)
		}
		if want := time.Date(2026, 3, 15, 1, 30, 0, 0, ny); !got.Equal(want) {
			t.Fatalf("2h30m: got %v; want %v", got.In(ny), want)
		}
	})

	t.Run("window spanning fall-back is physically one hour longer", func(t *testing.T) {
		// sun 00:00–04:00: on Nov 1 the physical window is 5h (01:00–02:00 EDT repeats as EST).
		sched := mustSchedule(t, Config{Timezone: "America/New_York", Hours: map[string][]Interval{
			"sun": {{Start: "00:00", End: "04:00"}},
		}})
		start := time.Date(2026, 11, 1, 0, 0, 0, 0, ny) // 00:00 EDT
		got, err := sched.AddBusinessDuration(start, 3*time.Hour)
		if err != nil {
			t.Fatalf("AddBusinessDuration(3h): %v", err)
		}
		if want := start.Add(3 * time.Hour); !got.Equal(want) {
			t.Fatalf("3h: got %v; want %v", got, want)
		}
		if wall := got.In(ny).Format("15:04"); wall != "02:00" {
			t.Fatalf("3h: local wall = %s; want 02:00 (EST, after the repeated hour)", wall)
		}
		// The full 5h physical window ends exactly at 04:00 EST.
		got, err = sched.AddBusinessDuration(start, 5*time.Hour)
		if err != nil {
			t.Fatalf("AddBusinessDuration(5h): %v", err)
		}
		if want := time.Date(2026, 11, 1, 4, 0, 0, 0, ny); !got.Equal(want) {
			t.Fatalf("5h: got %v; want %v (window end)", got.In(ny), want)
		}
	})

	t.Run("24/7 across spring-forward equals physical wall-clock", func(t *testing.T) {
		sched := mustSchedule(t, fullWeekConfig("America/New_York"))
		start := time.Date(2026, 3, 7, 12, 0, 0, 0, ny) // Sat noon EST
		got, err := sched.AddBusinessDuration(start, 24*time.Hour)
		if err != nil {
			t.Fatalf("AddBusinessDuration: %v", err)
		}
		if want := start.Add(24 * time.Hour); !got.Equal(want) { // Sun 13:00 EDT
			t.Fatalf("got %v; want %v", got.In(ny), want.In(ny))
		}
	})
}

func TestAddBusinessDuration_Guards(t *testing.T) {
	vn := mustLoc(t, "Asia/Ho_Chi_Minh")
	start := time.Date(2026, 7, 15, 9, 0, 0, 0, vn)

	t.Run("zero duration returns start unchanged", func(t *testing.T) {
		sched := mustSchedule(t, vnConfig())
		got, err := sched.AddBusinessDuration(start, 0)
		if err != nil || !got.Equal(start) {
			t.Fatalf("got %v, %v; want start unchanged", got, err)
		}
	})
	t.Run("negative duration is rejected", func(t *testing.T) {
		sched := mustSchedule(t, vnConfig())
		if _, err := sched.AddBusinessDuration(start, -time.Hour); !errors.Is(err, ErrInvalidDuration) {
			t.Fatalf("err = %v; want ErrInvalidDuration", err)
		}
	})
	t.Run("no-working-hours schedule errors fast, never hangs", func(t *testing.T) {
		sched := mustSchedule(t, Config{Timezone: "UTC", Hours: map[string][]Interval{}})
		if sched.HasWorkingHours() {
			t.Fatal("HasWorkingHours = true; want false")
		}
		if _, err := sched.AddBusinessDuration(start, time.Hour); !errors.Is(err, ErrNoWorkingTime) {
			t.Fatalf("err = %v; want ErrNoWorkingTime", err)
		}
	})
	t.Run("holiday wall longer than the walk horizon errors, never hangs", func(t *testing.T) {
		// Every date for > maxWalkDays is a holiday → the walk hits the cap.
		holidays := make([]string, 0, maxWalkDays+40)
		d := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i < maxWalkDays+40; i++ {
			holidays = append(holidays, d.Format("2006-01-02"))
			d = d.AddDate(0, 0, 1)
		}
		sched := mustSchedule(t, vnConfig(holidays...))
		if _, err := sched.AddBusinessDuration(time.Date(2026, 1, 5, 9, 0, 0, 0, vn), time.Hour); !errors.Is(err, ErrNoWorkingTime) {
			t.Fatalf("err = %v; want ErrNoWorkingTime", err)
		}
	})
}

func TestBusinessDurationBetween(t *testing.T) {
	vn := mustLoc(t, "Asia/Ho_Chi_Minh")
	sched := mustSchedule(t, vnConfig("2026-09-02"))

	tests := []struct {
		name     string
		from, to time.Time
		want     time.Duration
	}{
		{
			name: "[t,t) is zero",
			from: time.Date(2026, 7, 15, 9, 0, 0, 0, vn),
			to:   time.Date(2026, 7, 15, 9, 0, 0, 0, vn),
			want: 0,
		},
		{
			name: "reversed range is zero",
			from: time.Date(2026, 7, 15, 17, 0, 0, 0, vn),
			to:   time.Date(2026, 7, 15, 8, 0, 0, 0, vn),
			want: 0,
		},
		{
			name: "full working day counts 8h (lunch excluded)",
			from: time.Date(2026, 7, 15, 8, 0, 0, 0, vn),
			to:   time.Date(2026, 7, 15, 17, 0, 0, 0, vn),
			want: 8 * time.Hour,
		},
		{
			name: "spanning the weekend",
			from: time.Date(2026, 7, 17, 16, 0, 0, 0, vn), // Fri
			to:   time.Date(2026, 7, 20, 9, 0, 0, 0, vn),  // Mon
			want: 2 * time.Hour,
		},
		{
			name: "holiday inside the range contributes nothing",
			from: time.Date(2026, 9, 1, 13, 0, 0, 0, vn), // Tue afternoon
			to:   time.Date(2026, 9, 3, 9, 0, 0, 0, vn),  // Thu morning (Wed 2/9 is holiday)
			want: 5 * time.Hour,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sched.BusinessDurationBetween(tt.from, tt.to); got != tt.want {
				t.Fatalf("got %v; want %v", got, tt.want)
			}
		})
	}

	t.Run("24/7 equals wall-clock difference", func(t *testing.T) {
		full := mustSchedule(t, fullWeekConfig("Asia/Ho_Chi_Minh"))
		from := time.Date(2026, 7, 15, 3, 15, 0, 0, vn)
		to := from.Add(53*time.Hour + 27*time.Minute)
		if got := full.BusinessDurationBetween(from, to); got != 53*time.Hour+27*time.Minute {
			t.Fatalf("got %v; want wall-clock 53h27m", got)
		}
	})

	t.Run("round-trip with AddBusinessDuration", func(t *testing.T) {
		for _, c := range []struct {
			start time.Time
			d     time.Duration
		}{
			{time.Date(2026, 7, 15, 9, 0, 0, 0, vn), 2 * time.Hour},
			{time.Date(2026, 7, 17, 16, 0, 0, 0, vn), 4 * time.Hour},
			{time.Date(2026, 7, 18, 10, 0, 0, 0, vn), 9 * time.Hour}, // Sat start (non-working prefix)
		} {
			end, err := sched.AddBusinessDuration(c.start, c.d)
			if err != nil {
				t.Fatalf("AddBusinessDuration(%v, %v): %v", c.start, c.d, err)
			}
			if got := sched.BusinessDurationBetween(c.start, end); got != c.d {
				t.Fatalf("Between(%v, %v) = %v; want %v", c.start, end, got, c.d)
			}
		}
	})
}

func TestIsWorkingTime(t *testing.T) {
	vn := mustLoc(t, "Asia/Ho_Chi_Minh")
	sched := mustSchedule(t, vnConfig("2026-09-02"))

	tests := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"inside morning interval", time.Date(2026, 7, 15, 9, 0, 0, 0, vn), true},
		{"interval start is inclusive", time.Date(2026, 7, 15, 8, 0, 0, 0, vn), true},
		{"interval end is exclusive", time.Date(2026, 7, 15, 17, 0, 0, 0, vn), false},
		{"lunch break", time.Date(2026, 7, 15, 12, 30, 0, 0, vn), false},
		{"weekend", time.Date(2026, 7, 18, 10, 0, 0, 0, vn), false},
		{"holiday", time.Date(2026, 9, 2, 10, 0, 0, 0, vn), false},
		// The instant is expressed in UTC; 02:00Z = 09:00+07 → working. IsWorkingTime must
		// evaluate in the CALENDAR's tz, not the value's.
		{"UTC-expressed instant inside VN hours", time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sched.IsWorkingTime(tt.t); got != tt.want {
				t.Fatalf("IsWorkingTime(%v) = %v; want %v", tt.t, got, tt.want)
			}
		})
	}
}

func TestNew_Validation(t *testing.T) {
	valid := []Interval{{Start: "08:00", End: "17:00"}}
	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{"unknown IANA name", Config{Timezone: "Mars/Olympus", Hours: map[string][]Interval{"mon": valid}}, ErrTimezoneInvalid},
		{"empty timezone", Config{Timezone: "", Hours: map[string][]Interval{"mon": valid}}, ErrTimezoneInvalid},
		{"Local is machine-dependent, rejected", Config{Timezone: "Local", Hours: map[string][]Interval{"mon": valid}}, ErrTimezoneInvalid},
		{"bad weekday key", Config{Timezone: "UTC", Hours: map[string][]Interval{"monday": valid}}, ErrHoursInvalid},
		{"unpadded time", Config{Timezone: "UTC", Hours: map[string][]Interval{"mon": {{Start: "8:00", End: "17:00"}}}}, ErrHoursInvalid},
		{"hours out of range", Config{Timezone: "UTC", Hours: map[string][]Interval{"mon": {{Start: "25:00", End: "26:00"}}}}, ErrHoursInvalid},
		{"minutes out of range", Config{Timezone: "UTC", Hours: map[string][]Interval{"mon": {{Start: "08:00", End: "12:60"}}}}, ErrHoursInvalid},
		{"start equals end", Config{Timezone: "UTC", Hours: map[string][]Interval{"mon": {{Start: "12:00", End: "12:00"}}}}, ErrHoursInvalid},
		{"start after end", Config{Timezone: "UTC", Hours: map[string][]Interval{"mon": {{Start: "14:00", End: "12:00"}}}}, ErrHoursInvalid},
		{"overlapping intervals", Config{Timezone: "UTC", Hours: map[string][]Interval{"mon": {
			{Start: "08:00", End: "12:00"}, {Start: "11:00", End: "15:00"},
		}}}, ErrHoursInvalid},
		{"24:00 as start", Config{Timezone: "UTC", Hours: map[string][]Interval{"mon": {{Start: "24:00", End: "24:00"}}}}, ErrHoursInvalid},
		{"bad holiday month", Config{Timezone: "UTC", Hours: map[string][]Interval{"mon": valid}, Holidays: []string{"2026-13-01"}}, ErrHolidaysInvalid},
		{"holiday wrong format", Config{Timezone: "UTC", Hours: map[string][]Interval{"mon": valid}, Holidays: []string{"02/09/2026"}}, ErrHolidaysInvalid},
		{"holiday unpadded", Config{Timezone: "UTC", Hours: map[string][]Interval{"mon": valid}, Holidays: []string{"2026-2-1"}}, ErrHolidaysInvalid},
		{"holiday day out of range", Config{Timezone: "UTC", Hours: map[string][]Interval{"mon": valid}, Holidays: []string{"2026-02-30"}}, ErrHolidaysInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v; want %v", err, tt.wantErr)
			}
		})
	}

	t.Run("valid config incl touching intervals and 24:00 end", func(t *testing.T) {
		sched, err := New(Config{
			Timezone: "Asia/Ho_Chi_Minh",
			Hours: map[string][]Interval{
				"mon": {{Start: "08:00", End: "12:00"}, {Start: "12:00", End: "17:00"}}, // touching = OK
				"sat": {{Start: "00:00", End: "24:00"}},
				"sun": {}, // explicit empty day = OK
			},
			Holidays: []string{"2026-09-02", "2026-09-02"}, // duplicate = deduped, OK
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !sched.HasWorkingHours() {
			t.Fatal("HasWorkingHours = false; want true")
		}
	})
}

func TestFromJSON(t *testing.T) {
	hours := []byte(`{"mon":[{"start":"08:00","end":"12:00"},{"start":"13:00","end":"17:00"}],"sun":[]}`)

	t.Run("valid stored shape parses", func(t *testing.T) {
		for _, holidays := range [][]byte{nil, []byte(`null`), []byte(`["2026-09-02"]`)} {
			if _, err := FromJSON("Asia/Ho_Chi_Minh", hours, holidays); err != nil {
				t.Fatalf("FromJSON(holidays=%s): %v", holidays, err)
			}
		}
	})
	t.Run("garbage hours JSON", func(t *testing.T) {
		if _, err := FromJSON("UTC", []byte(`{broken`), nil); !errors.Is(err, ErrHoursInvalid) {
			t.Fatalf("err = %v; want ErrHoursInvalid", err)
		}
	})
	t.Run("garbage holidays JSON", func(t *testing.T) {
		if _, err := FromJSON("UTC", hours, []byte(`{broken`)); !errors.Is(err, ErrHolidaysInvalid) {
			t.Fatalf("err = %v; want ErrHolidaysInvalid", err)
		}
	})
	t.Run("holiday matching agrees with FromJSON round-trip", func(t *testing.T) {
		vn := mustLoc(t, "Asia/Ho_Chi_Minh")
		sched, err := FromJSON("Asia/Ho_Chi_Minh",
			[]byte(`{"wed":[{"start":"08:00","end":"17:00"}]}`), []byte(`["2026-09-02"]`))
		if err != nil {
			t.Fatalf("FromJSON: %v", err)
		}
		if sched.IsWorkingTime(time.Date(2026, 9, 2, 10, 0, 0, 0, vn)) {
			t.Fatal("2026-09-02 (holiday) reported as working time")
		}
		if !sched.IsWorkingTime(time.Date(2026, 9, 9, 10, 0, 0, 0, vn)) {
			t.Fatal("following Wednesday should be working time")
		}
	})
}

// TestParseHHMM_Table pins the strict HH:MM contract on its own (the format is API surface —
// hours_json documents it).
func TestParseHHMM_Table(t *testing.T) {
	tests := []struct {
		in       string
		allowEOD bool
		want     int
		wantErr  bool
	}{
		{"00:00", false, 0, false},
		{"08:30", false, 510, false},
		{"23:59", false, 1439, false},
		{"24:00", true, 1440, false},
		{"24:00", false, 0, true},
		{"24:01", true, 0, true},
		{"8:00", false, 0, true},
		{"08:0", false, 0, true},
		{"ab:cd", false, 0, true},
		{"08-00", false, 0, true},
		{"", false, 0, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q_eod=%v", tt.in, tt.allowEOD), func(t *testing.T) {
			got, err := parseHHMM(tt.in, tt.allowEOD)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v; wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("got %d; want %d", got, tt.want)
			}
		})
	}
}

// TestCalendar_MidnightDSTZone pins the BLOCKER B1 fix: a timezone that springs forward AT
// midnight (America/Santiago 2026-09-06 00:00→01:00) must not stall the day-walk. Before the fix,
// the naive next-midnight time.Date normalized BACKWARD to 23:00 of the prior day, freezing the
// cursor → AddBusinessDuration false-errored and BusinessDurationBetween looped forever.
func TestCalendar_MidnightDSTZone(t *testing.T) {
	loc := mustLoc(t, "America/Santiago")
	// Mon–Fri 09:00–17:00, no holidays.
	wk := []Interval{{Start: "09:00", End: "17:00"}}
	sched := mustSchedule(t, Config{Timezone: "America/Santiago", Hours: map[string][]Interval{
		"mon": wk, "tue": wk, "wed": wk, "thu": wk, "fri": wk,
	}})

	// A) Add across the Sep 6 (Sunday) midnight transition: Fri Sep 4 16:00 + 2h business.
	//    1h left Friday (16:00–17:00) + 1h Monday morning (09:00–10:00) → Mon Sep 7 10:00.
	start := time.Date(2026, 9, 4, 16, 0, 0, 0, loc)
	got, err := sched.AddBusinessDuration(start, 2*time.Hour)
	if err != nil {
		t.Fatalf("AddBusinessDuration across midnight-DST zone errored: %v", err)
	}
	want := time.Date(2026, 9, 7, 10, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("AddBusinessDuration = %s; want %s", got.In(loc), want.In(loc))
	}

	// B) Between across the same transition must TERMINATE (not hang) and count the working days.
	//    Fri Sep 4 09:00 → Tue Sep 8 09:00 = Fri(8h) + Mon(8h) = 16h (Sat/Sun off, DST day is Sun).
	from := time.Date(2026, 9, 4, 9, 0, 0, 0, loc)
	to := time.Date(2026, 9, 8, 9, 0, 0, 0, loc)
	done := make(chan time.Duration, 1)
	go func() { done <- sched.BusinessDurationBetween(from, to) }()
	select {
	case d := <-done:
		if d != 16*time.Hour {
			t.Errorf("BusinessDurationBetween = %v; want 16h", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BusinessDurationBetween HUNG across a midnight-DST transition (B1 regression)")
	}

	// C) A 24/7 calendar in this zone: the DST day is physically 23h — sanity that Add of 24h from
	//    that midnight lands 24 physical hours later, never stalls.
	full := mustSchedule(t, fullWeekConfig("America/Santiago"))
	d0 := time.Date(2026, 9, 5, 12, 0, 0, 0, loc)
	if _, err := full.AddBusinessDuration(d0, 24*time.Hour); err != nil {
		t.Fatalf("24/7 Add across midnight-DST errored: %v", err)
	}
}
