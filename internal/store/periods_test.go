package store

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestLoadLocation(t *testing.T) {
	for _, name := range []string{"UTC", "Asia/Dhaka", "Asia/Karachi", "Asia/Singapore", "America/New_York"} {
		if _, err := LoadLocation(name); err != nil {
			t.Errorf("LoadLocation(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", "Local", "Mars/Olympus", "utc+6"} {
		if _, err := LoadLocation(name); err == nil {
			t.Errorf("LoadLocation(%q) should fail", name)
		}
	}
}

func TestPeriodStarts(t *testing.T) {
	dhaka := mustLoc(t, "Asia/Dhaka") // UTC+6, no DST
	// 2026-03-04 20:30 UTC is Thursday 2026-03-05 02:30 in Dhaka.
	at := time.Date(2026, 3, 4, 20, 30, 0, 0, time.UTC)
	cases := []struct {
		period string
		want   time.Time
	}{
		{PeriodDay, time.Date(2026, 3, 5, 0, 0, 0, 0, dhaka)},
		{PeriodWeek, time.Date(2026, 3, 2, 0, 0, 0, 0, dhaka)},
		{PeriodMonth, time.Date(2026, 3, 1, 0, 0, 0, 0, dhaka)},
	}
	for _, tc := range cases {
		if got := periodStart(at, dhaka, tc.period); !got.Equal(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.period, got, tc.want)
		}
	}
	// A Monday is its own week start; a Sunday belongs to the week that began six days earlier.
	monday := time.Date(2026, 3, 9, 10, 0, 0, 0, dhaka)
	if got := startOfWeek(monday, dhaka); !got.Equal(time.Date(2026, 3, 9, 0, 0, 0, 0, dhaka)) {
		t.Errorf("monday week start: %v", got)
	}
	sunday := time.Date(2026, 3, 8, 23, 0, 0, 0, dhaka)
	if got := startOfWeek(sunday, dhaka); !got.Equal(time.Date(2026, 3, 2, 0, 0, 0, 0, dhaka)) {
		t.Errorf("sunday week start: %v", got)
	}
}

func TestCalendarPeriod(t *testing.T) {
	sg := mustLoc(t, "Asia/Singapore")
	cases := []struct {
		at         time.Time
		months     int
		start, end time.Time
	}{
		{time.Date(2026, 5, 20, 12, 0, 0, 0, sg), 6, time.Date(2026, 1, 1, 0, 0, 0, 0, sg), time.Date(2026, 7, 1, 0, 0, 0, 0, sg)},
		{time.Date(2026, 7, 1, 0, 0, 0, 0, sg), 6, time.Date(2026, 7, 1, 0, 0, 0, 0, sg), time.Date(2027, 1, 1, 0, 0, 0, 0, sg)},
		{time.Date(2026, 12, 31, 23, 59, 0, 0, sg), 6, time.Date(2026, 7, 1, 0, 0, 0, 0, sg), time.Date(2027, 1, 1, 0, 0, 0, 0, sg)},
		{time.Date(2026, 11, 15, 0, 0, 0, 0, sg), 3, time.Date(2026, 10, 1, 0, 0, 0, 0, sg), time.Date(2027, 1, 1, 0, 0, 0, 0, sg)},
		{time.Date(2026, 2, 28, 0, 0, 0, 0, sg), 1, time.Date(2026, 2, 1, 0, 0, 0, 0, sg), time.Date(2026, 3, 1, 0, 0, 0, 0, sg)},
		{time.Date(2026, 8, 8, 0, 0, 0, 0, sg), 12, time.Date(2026, 1, 1, 0, 0, 0, 0, sg), time.Date(2027, 1, 1, 0, 0, 0, 0, sg)},
		// 30 June 17:00 UTC is already 1 July in Singapore.
		{time.Date(2026, 6, 30, 17, 0, 0, 0, time.UTC), 6, time.Date(2026, 7, 1, 0, 0, 0, 0, sg), time.Date(2027, 1, 1, 0, 0, 0, 0, sg)},
	}
	for _, tc := range cases {
		start, end := calendarPeriod(tc.at, sg, tc.months)
		if !start.Equal(tc.start) || !end.Equal(tc.end) {
			t.Errorf("calendarPeriod(%v, %d) = [%v, %v), want [%v, %v)", tc.at, tc.months, start, end, tc.start, tc.end)
		}
	}
}

func TestExpiresAt(t *testing.T) {
	sg := mustLoc(t, "Asia/Singapore")
	ny := mustLoc(t, "America/New_York")
	three, ninety, six := 3, 90, 6
	earned := time.Date(2026, 1, 15, 14, 0, 0, 0, sg)

	if got := expiresAt(earned, sg, ExpiryNever, nil); got != nil {
		t.Errorf("never: got %v", got)
	}
	// Shopee: earned in January, valid through 31 March, so expires at 1 April 00:00 local.
	if got := expiresAt(earned, sg, ExpiryEndOfMonth, &three); !got.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, sg)) {
		t.Errorf("end_of_month: got %v", got)
	}
	// Earned on the last evening of a month still counts that month as the first.
	lastDay := time.Date(2026, 1, 31, 23, 30, 0, 0, sg)
	if got := expiresAt(lastDay, sg, ExpiryEndOfMonth, &three); !got.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, sg)) {
		t.Errorf("end_of_month from the last day: got %v", got)
	}
	// 90 days after 15 January is 15 April; the points last through that day.
	if got := expiresAt(earned, sg, ExpiryDays, &ninety); !got.Equal(time.Date(2026, 4, 16, 0, 0, 0, 0, sg)) {
		t.Errorf("days: got %v", got)
	}
	// Everything earned on one local day shares an expiry.
	morning := time.Date(2026, 1, 15, 0, 5, 0, 0, sg)
	if a, b := expiresAt(morning, sg, ExpiryDays, &ninety), expiresAt(earned, sg, ExpiryDays, &ninety); !a.Equal(*b) {
		t.Errorf("same-day credits should share an expiry: %v vs %v", a, b)
	}
	// Local midnight, across a DST change.
	winter := time.Date(2026, 1, 10, 9, 0, 0, 0, ny)
	if got := expiresAt(winter, ny, ExpiryDays, &ninety); !got.Equal(time.Date(2026, 4, 11, 0, 0, 0, 0, ny)) {
		t.Errorf("days across DST: got %v", got.In(ny))
	}
	if got := expiresAt(winter, ny, ExpiryEndOfMonth, &six); !got.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, ny)) {
		t.Errorf("end_of_month across DST: got %v", got)
	}
}

func TestCivilDate(t *testing.T) {
	dhaka := mustLoc(t, "Asia/Dhaka")
	at := time.Date(2026, 3, 4, 20, 30, 0, 0, time.UTC) // 5 March in Dhaka
	if got := civilDate(at, dhaka); !got.Equal(time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("civilDate: got %v", got)
	}
}
