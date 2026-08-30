package main

import (
	"testing"
	"time"
)

// Period bounds are the commonest place for a reporting error: if `to` is
// taken as midnight, the last day of the export is lost entirely, and that is
// only noticed from an incomplete file in the customer's hands.
func TestParseDayBounds(t *testing.T) {
	from, ok, err := parseDay("2026-08-01", false)
	if err != nil || !ok {
		t.Fatalf("from: ok=%v err=%v", ok, err)
	}
	if h, m, s := from.Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("the period start must be 00:00:00, got %02d:%02d:%02d", h, m, s)
	}

	to, ok, err := parseDay("2026-08-31", true)
	if err != nil || !ok {
		t.Fatalf("to: ok=%v err=%v", ok, err)
	}
	if h, m, s := to.Clock(); h != 23 || m != 59 || s != 59 {
		t.Errorf("the period end must cover the whole day, got %02d:%02d:%02d", h, m, s)
	}
	if to.Format("2006-01-02") != "2026-08-31" {
		t.Errorf("the period end drifted to another date: %s", to.Format("2006-01-02"))
	}
}

func TestParseDayAcceptsRFC3339(t *testing.T) {
	got, ok, err := parseDay("2026-08-15T13:45:00Z", false)
	if err != nil || !ok {
		t.Fatalf("RFC3339 must be accepted: ok=%v err=%v", ok, err)
	}
	if !got.Equal(time.Date(2026, 8, 15, 13, 45, 0, 0, time.UTC)) {
		t.Errorf("parsed incorrectly: %v", got)
	}
}

// An empty string means "no bound", not an error: the handler substitutes its
// own default. If parseDay returned an error, a request with no parameters
// would fail with 400 instead of returning the last 30 days.
func TestParseDayEmptyIsNotAnError(t *testing.T) {
	_, ok, err := parseDay("", false)
	if err != nil {
		t.Fatalf("an empty string must not be an error: %v", err)
	}
	if ok {
		t.Error("an empty string must not count as a bound that was set")
	}
}

func TestParseDayRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"yesterday", "2026-13-45", "31/08/2026", "20260831"} {
		if _, _, err := parseDay(bad, false); err == nil {
			t.Errorf("the rubbish %q must be rejected", bad)
		}
	}
}

// The export sorts by string, which is only correct because RFC3339 in UTC has
// fixed-width fields. This test pins the assumption itself: if the time format
// is ever changed to local time or to no leading zeros, the report order will
// silently drift and it will not be noticed at once.
func TestRFC3339UTCSortsChronologically(t *testing.T) {
	base := time.Date(2026, 1, 9, 9, 5, 5, 0, time.UTC)
	prev := ""
	for i := 0; i < 40; i++ {
		cur := base.Add(time.Duration(i) * 37 * time.Minute).UTC().Format(time.RFC3339)
		if prev != "" && !(cur > prev) {
			t.Fatalf("string order diverged from chronological: %q is not greater than %q", cur, prev)
		}
		prev = cur
	}
}
