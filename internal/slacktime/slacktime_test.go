package slacktime

import (
	"testing"
	"time"
)

// fixNow pins the clock so relative and day-based forms are testable.
func fixNow(t *testing.T, iso string, loc *time.Location) time.Time {
	t.Helper()
	n, err := time.ParseInLocation(time.RFC3339, iso, loc)
	if err != nil {
		t.Fatalf("parse fixed now: %v", err)
	}
	orig := now
	now = func() time.Time { return n }
	t.Cleanup(func() { now = orig })
	return n
}

func TestTSRoundTrip(t *testing.T) {
	cases := []string{
		"1725806400.123456",
		"1788887498.013459",
		"1725806400.000001",
		"1725806400.000000",
	}
	for _, want := range cases {
		t.Run(want, func(t *testing.T) {
			ts, err := Parse(want)
			if err != nil {
				t.Fatalf("Parse(%q): %v", want, err)
			}
			if got := ts.String(); got != want {
				t.Errorf("Parse(%q).String() = %q, want %q", want, got, want)
			}
		})
	}
}

// TestTSPrecisionSurvives is the reason TS holds integer microseconds: a ts
// needs 16 significant digits and float64 has about 15.95, so a float
// round-trip silently changes which message a ts identifies.
func TestTSPrecisionSurvives(t *testing.T) {
	const wire = "1788887498.013459"
	ts := MustParse(wire)
	if got := ts.String(); got != wire {
		t.Errorf("String() = %q, want %q", got, wire)
	}
	// One microsecond apart must stay distinguishable.
	a, b := MustParse("1788887498.013459"), MustParse("1788887498.013460")
	if !b.After(a) {
		t.Error("adjacent microsecond timestamps compared equal")
	}
}

func TestTSParseForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		zero bool
	}{
		{"wire", "1725806400.123456", "1725806400.123456", false},
		{"permalink", "p1725806400123456", "1725806400.123456", false},
		{"bare seconds", "1725806400", "1725806400.000000", false},
		{"short fraction", "1725806400.5", "1725806400.500000", false},
		// Slack writes "never read" both ways, and both must read as zero -
		// which is exactly why a ts is parsed rather than string-compared.
		{"never read short", "0", "", true},
		{"never read long", "0000000000.000000", "", true},
		{"empty", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if ts.IsZero() != tc.zero {
				t.Errorf("IsZero() = %v, want %v", ts.IsZero(), tc.zero)
			}
			if !tc.zero && ts.String() != tc.want {
				t.Errorf("String() = %q, want %q", ts.String(), tc.want)
			}
		})
	}
}

func TestPermalinkForm(t *testing.T) {
	if got := MustParse("1725806400.123456").Permalink(); got != "p1725806400123456" {
		t.Errorf("Permalink() = %q, want p1725806400123456", got)
	}
}

// TestBareDateAsymmetry is the rule that makes the obvious query work: without
// end-of-day for --until, `--since D --until D` would return nothing.
func TestBareDateAsymmetry(t *testing.T) {
	loc := time.UTC
	fixNow(t, "2026-09-08T12:00:00Z", loc)

	r, err := ParseRange("2026-09-01", "2026-09-01", "", loc)
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if got := r.Since.Format(time.RFC3339); got != "2026-09-01T00:00:00Z" {
		t.Errorf("Since = %s, want start of day", got)
	}
	if r.Until.Day() != 1 || r.Until.Hour() != 23 {
		t.Errorf("Until = %s, want end of the same day", r.Until.Format(time.RFC3339))
	}
	// A message during that day must land inside the window.
	mid := FromTime(time.Date(2026, 9, 1, 12, 0, 0, 0, loc))
	if !r.Contains(mid) {
		t.Errorf("a midday message on 2026-09-01 is outside %s..%s",
			r.Since.Format(time.RFC3339), r.Until.Format(time.RFC3339))
	}
}

// TestOnIsAWholeDay checks the shorthand, which exists so the common
// single-day question never meets the end-of-day trap.
func TestOnIsAWholeDay(t *testing.T) {
	loc := time.UTC
	fixNow(t, "2026-09-08T12:00:00Z", loc)

	r, err := ParseRange("", "", "2026-09-01", loc)
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if r.Since.Format(time.RFC3339) != "2026-09-01T00:00:00Z" {
		t.Errorf("Since = %s", r.Since.Format(time.RFC3339))
	}
	if r.Until.Format(time.RFC3339) != "2026-09-02T00:00:00Z" {
		t.Errorf("Until = %s, want the next midnight", r.Until.Format(time.RFC3339))
	}
}

// TestHalfOpenBoundaries pins the exclusive upper bound: chained windows must
// neither drop nor duplicate a message sitting exactly on the join.
func TestHalfOpenBoundaries(t *testing.T) {
	loc := time.UTC
	fixNow(t, "2026-09-08T12:00:00Z", loc)

	r, err := ParseRange("", "", "2026-09-01", loc)
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	lower := FromTime(r.Since)
	upper := FromTime(r.Until)

	if !r.Contains(lower) {
		t.Error("the Since boundary is excluded, want included")
	}
	if r.Contains(upper) {
		t.Error("the Until boundary is included, want excluded")
	}
	// And the oldest parameter must be shifted back so Slack's exclusive
	// `oldest` still yields the boundary message.
	if got, want := r.Oldest(), lower.Add(-time.Microsecond).String(); got != want {
		t.Errorf("Oldest() = %q, want %q", got, want)
	}
}

func TestRelativeAndKeywordForms(t *testing.T) {
	loc := time.UTC
	n := fixNow(t, "2026-09-08T12:00:00Z", loc)

	cases := []struct {
		in   string
		want time.Time
	}{
		{"2h", n.Add(-2 * time.Hour)},
		{"90m", n.Add(-90 * time.Minute)},
		{"7d", n.AddDate(0, 0, -7)},
		{"2w", n.AddDate(0, 0, -14)},
		{"1h30m", n.Add(-90 * time.Minute)},
		{"@1725806400", time.Unix(1725806400, 0).In(loc)},
		{"today", time.Date(2026, 9, 8, 0, 0, 0, 0, loc)},
		{"yesterday", time.Date(2026, 9, 7, 0, 0, 0, 0, loc)},
		{"now", n},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			r, err := ParseRange(tc.in, "", "", loc)
			if err != nil {
				t.Fatalf("ParseRange(%q): %v", tc.in, err)
			}
			if !r.Since.Equal(tc.want) {
				t.Errorf("Since = %s, want %s", r.Since.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
}

// TestWeekdayResolvesBackwards checks "mon" means the most recent Monday, not
// the next one - the only reading that makes sense for a log of the past.
func TestWeekdayResolvesBackwards(t *testing.T) {
	loc := time.UTC
	fixNow(t, "2026-09-08T12:00:00Z", loc) // a Tuesday

	r, err := ParseRange("mon", "", "", loc)
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if r.Since.Weekday() != time.Monday {
		t.Errorf("Since is a %s, want Monday", r.Since.Weekday())
	}
	if r.Since.After(now()) {
		t.Error("Since is in the future")
	}
	if got := r.Since.Format("2006-01-02"); got != "2026-09-07" {
		t.Errorf("Since = %s, want 2026-09-07", got)
	}
}

// TestMinuteIsNotAMonth guards the trap: "1m" is a Go minute, and reading it
// as a month would be a 43000-fold surprise.
func TestMinuteIsNotAMonth(t *testing.T) {
	loc := time.UTC
	n := fixNow(t, "2026-09-08T12:00:00Z", loc)

	r, err := ParseRange("1m", "", "", loc)
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if !r.Since.Equal(n.Add(-time.Minute)) {
		t.Errorf("Since = %s, want one minute ago", r.Since.Format(time.RFC3339))
	}
}

// TestDSTTransition is the case the plan singled out: a day-based window across
// a clock change must still cover the whole local day.
func TestDSTTransition(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	fixNow(t, "2026-12-01T12:00:00-03:00", loc)

	// Chile springs forward in early September.
	r, err := ParseRange("", "", "2026-09-06", loc)
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	hours := r.Until.Sub(r.Since).Hours()
	if hours != 23 && hours != 24 && hours != 25 {
		t.Errorf("day length = %v hours, want 23, 24 or 25", hours)
	}
	if r.Since.Hour() != 0 {
		t.Errorf("Since = %s, want local midnight", r.Since.Format(time.RFC3339))
	}
}

func TestRangeRejectsInvertedBounds(t *testing.T) {
	loc := time.UTC
	fixNow(t, "2026-09-08T12:00:00Z", loc)

	if _, err := ParseRange("2026-09-05", "2026-09-01", "", loc); err == nil {
		t.Error("ParseRange(since after until) = nil error, want a failure")
	}
}

func TestOnConflictsWithSinceUntil(t *testing.T) {
	loc := time.UTC
	fixNow(t, "2026-09-08T12:00:00Z", loc)

	if _, err := ParseRange("7d", "", "2026-09-01", loc); err == nil {
		t.Error("--on with --since = nil error, want a failure")
	}
}

// TestFutureUntilIsClamped keeps the echoed window honest: there are no future
// messages, so reporting one would misdescribe what was examined.
func TestFutureUntilIsClamped(t *testing.T) {
	loc := time.UTC
	n := fixNow(t, "2026-09-08T12:00:00Z", loc)

	r, err := ParseRange("", "2027-01-01", "", loc)
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if r.Until.After(n) {
		t.Errorf("Until = %s, want clamped to now", r.Until.Format(time.RFC3339))
	}
}

func TestUnparseableTimeNamesTheAcceptedForms(t *testing.T) {
	_, err := ParseRange("last tuesday-ish", "", "", time.UTC)
	if err == nil {
		t.Fatal("ParseRange = nil error, want a failure")
	}
	for _, want := range []string{"7d", "today", "2026-09-01"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q does not mention the accepted form %q", err, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
