package store

import (
	"fmt"
	"time"

	// Embedded so tenant time zones resolve in minimal container images.
	_ "time/tzdata"
)

// LoadLocation resolves a tenant time zone by IANA name. "Local" is rejected because it
// would depend on the server's configuration.
func LoadLocation(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("invalid time zone %q", name)
	}
	return time.LoadLocation(name)
}

// location resolves a stored time zone, falling back to UTC if it no longer resolves.
func location(name string) *time.Location {
	loc, err := LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// startOfDay returns local midnight of the day containing t.
func startOfDay(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}

// startOfWeek returns local midnight of the Monday of the ISO week containing t.
func startOfWeek(t time.Time, loc *time.Location) time.Time {
	d := startOfDay(t, loc)
	back := (int(d.Weekday()) + 6) % 7
	return time.Date(d.Year(), d.Month(), d.Day()-back, 0, 0, 0, 0, loc)
}

// startOfMonth returns local midnight of the first day of the month containing t.
func startOfMonth(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, loc)
}

// periodStart returns the start of the limit period (day, week or month) containing t.
func periodStart(t time.Time, loc *time.Location, period string) time.Time {
	switch period {
	case PeriodWeek:
		return startOfWeek(t, loc)
	case PeriodMonth:
		return startOfMonth(t, loc)
	}
	return startOfDay(t, loc)
}

// calendarPeriod returns the bounds [start, end) of the fixed period of months months that
// contains t. Periods are aligned to 1 January, so months must divide 12.
func calendarPeriod(t time.Time, loc *time.Location, months int) (start, end time.Time) {
	l := t.In(loc)
	first := (int(l.Month()) - 1) / months * months
	start = time.Date(l.Year(), time.Month(first+1), 1, 0, 0, 0, 0, loc)
	return start, start.AddDate(0, months, 0)
}

// expiresAt returns when points credited at t expire under the given policy, or nil if
// they never do. Both modes expire at a local midnight, so everything earned on one day (days)
// or in one month (end_of_month) shares an expiry and lands in the same lot: days keeps points
// through the value-th day after t; end_of_month keeps them through the last day of the
// value-th month, counting t's month as the first.
func expiresAt(t time.Time, loc *time.Location, mode string, value *int) *time.Time {
	if value == nil {
		return nil
	}
	var e time.Time
	switch mode {
	case ExpiryDays:
		e = startOfDay(t, loc).AddDate(0, 0, *value+1)
	case ExpiryEndOfMonth:
		e = startOfMonth(t, loc).AddDate(0, *value, 0)
	default:
		return nil
	}
	return &e
}

// civilDate represents the local calendar date of t as midnight UTC, the form Postgres
// date values are scanned into.
func civilDate(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
}
