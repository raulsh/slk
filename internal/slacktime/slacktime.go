// Package slacktime converts between Slack timestamps, the day-based strings
// people and agents actually type, and time.Time.
//
// It is a package rather than inline code because three layers need the same
// answers - the transport builds `oldest`/`latest`, the renderer prints local
// times, and the commands parse --since - and because the two genuinely
// subtle rules here are easy to reinvent differently in each: a Slack ts must
// never round-trip through a float, and a bare date means the start of a day
// for --since but the end of one for --until.
package slacktime

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TS is a Slack timestamp: unix seconds and microseconds, e.g.
// "1725806400.123456".
//
// It holds integer microseconds rather than a float64 because the wire form
// needs 16 significant digits and float64 provides about 15.95, so
// ParseFloat/FormatFloat round-trips are not reliably exact. A ts is also a
// message's identity, not just its time, so an off-by-one-microsecond value is
// a wrong message rather than a slightly wrong clock.
type TS struct{ micros int64 }

// now is a variable so tests can fix the clock.
var now = time.Now

// Parse reads a Slack ts.
//
// It accepts the wire form, a bare integer, the permalink form (p1725806400123456)
// and the all-zero form Slack uses for "never", which is why a ts must be
// parsed rather than string-compared.
func Parse(s string) (TS, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return TS{}, nil
	}
	// Permalink form: p<10 digits><6 digits>, no separator.
	if len(s) == 17 && s[0] == 'p' {
		if _, err := strconv.ParseInt(s[1:], 10, 64); err == nil {
			s = s[1:11] + "." + s[11:]
		}
	}

	secStr, fracStr, hasFrac := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(secStr, 10, 64)
	if err != nil {
		return TS{}, fmt.Errorf("%q is not a slack timestamp", s)
	}

	var micros int64
	if hasFrac {
		// Pad or truncate to exactly six digits, so ".5" is half a second and
		// a nanosecond-precision value does not overflow into seconds.
		if len(fracStr) > 6 {
			fracStr = fracStr[:6]
		}
		fracStr += strings.Repeat("0", 6-len(fracStr))
		micros, err = strconv.ParseInt(fracStr, 10, 64)
		if err != nil {
			return TS{}, fmt.Errorf("%q has an invalid fractional part", s)
		}
	}
	return TS{micros: sec*1e6 + micros}, nil
}

// MustParse is for tests and constants.
func MustParse(s string) TS {
	t, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return t
}

// FromTime converts a wall-clock time.
func FromTime(t time.Time) TS {
	return TS{micros: t.Unix()*1e6 + int64(t.Nanosecond())/1e3}
}

// String renders the wire form. Never use FormatFloat for this.
func (t TS) String() string {
	if t.micros == 0 {
		return ""
	}
	return fmt.Sprintf("%d.%06d", t.micros/1e6, t.micros%1e6)
}

// Time converts back to wall-clock.
func (t TS) Time() time.Time {
	return time.Unix(t.micros/1e6, (t.micros%1e6)*1e3)
}

// IsZero reports whether this is the unset or all-zero ts. Slack writes "never
// read" as "0" or "0000000000.000000", and both must read as zero.
func (t TS) IsZero() bool { return t.micros == 0 }

// After reports whether t is strictly later than o, which is the definition of
// "unread" once o is a last_read marker.
func (t TS) After(o TS) bool { return t.micros > o.micros }

// Before reports whether t is strictly earlier than o.
func (t TS) Before(o TS) bool { return t.micros < o.micros }

// Add shifts by a duration, truncated to microseconds.
func (t TS) Add(d time.Duration) TS {
	return TS{micros: t.micros + int64(d/time.Microsecond)}
}

// Permalink renders the p-prefixed form a Slack archive URL uses.
func (t TS) Permalink() string {
	if t.micros == 0 {
		return ""
	}
	return fmt.Sprintf("p%d%06d", t.micros/1e6, t.micros%1e6)
}

// Range is a half-open window [Since, Until). A zero bound is unbounded.
//
// Half-open is the only choice that makes `--since D --until D+1` mean exactly
// "all of day D", so chained windows neither drop nor duplicate a message.
type Range struct {
	Since time.Time
	Until time.Time
	Loc   *time.Location
}

// Oldest renders the `oldest` parameter for conversations.history.
//
// Slack's default is inclusive=false, meaning it returns messages strictly
// after `oldest`. To include a message sitting exactly on the Since boundary,
// the bound is shifted back one microsecond - the finest resolution a ts has,
// so it cannot skip a real message.
func (r Range) Oldest() string {
	if r.Since.IsZero() {
		return ""
	}
	return FromTime(r.Since).Add(-time.Microsecond).String()
}

// Latest renders the `latest` parameter, which Slack treats as inclusive of
// the boundary - so passing Until directly gives the half-open exclusion we
// want, with no shift.
func (r Range) Latest() string {
	if r.Until.IsZero() {
		return ""
	}
	return FromTime(r.Until).String()
}

// Contains reports whether a ts falls in the window, for the client-side
// filtering that search needs because Slack's search modifiers are only
// day-granular.
func (r Range) Contains(t TS) bool {
	if !r.Since.IsZero() && t.Before(FromTime(r.Since)) {
		return false
	}
	if !r.Until.IsZero() && !FromTime(r.Until).After(t) {
		return false
	}
	return true
}

// IsZero reports whether the window is unbounded on both ends.
func (r Range) IsZero() bool { return r.Since.IsZero() && r.Until.IsZero() }

// edge selects which end of a day a bare date means.
type edge int

const (
	startOfDay edge = iota
	endOfDay
)

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "sun": time.Sunday,
	"monday": time.Monday, "mon": time.Monday,
	"tuesday": time.Tuesday, "tue": time.Tuesday, "tues": time.Tuesday,
	"wednesday": time.Wednesday, "wed": time.Wednesday,
	"thursday": time.Thursday, "thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday,
	"friday": time.Friday, "fri": time.Friday,
	"saturday": time.Saturday, "sat": time.Saturday,
}

// ParseRange resolves --since, --until and --on into an absolute window.
//
// on is shorthand for a whole single day, which is the common case and removes
// the end-of-day trap from it entirely.
func ParseRange(since, until, on string, loc *time.Location) (Range, error) {
	if loc == nil {
		loc = time.Local
	}
	r := Range{Loc: loc}

	if on != "" {
		if since != "" || until != "" {
			return r, errors.New("--on cannot be combined with --since or --until")
		}
		start, err := parseOne(on, startOfDay, loc)
		if err != nil {
			return r, err
		}
		r.Since = start
		r.Until = start.AddDate(0, 0, 1)
		return r, nil
	}

	var err error
	if since != "" {
		if r.Since, err = parseOne(since, startOfDay, loc); err != nil {
			return r, err
		}
	}
	if until != "" {
		if r.Until, err = parseOne(until, endOfDay, loc); err != nil {
			return r, err
		}
	}

	// A future bound is not an error - "until tomorrow" is a reasonable thing
	// to type - but there are no future messages, so clamping keeps the
	// echoed window honest about what was actually examined.
	if n := now(); !r.Until.IsZero() && r.Until.After(n) {
		r.Until = n
	}
	if !r.Since.IsZero() && !r.Until.IsZero() && r.Since.After(r.Until) {
		return r, fmt.Errorf("--since (%s) is after --until (%s)",
			r.Since.Format(time.RFC3339), r.Until.Format(time.RFC3339))
	}
	return r, nil
}

// parseOne resolves a single bound.
func parseOne(s string, e edge, loc *time.Location) (time.Time, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	n := now().In(loc)

	switch s {
	case "now":
		return n, nil
	case "today":
		return dayEdge(n, e, loc), nil
	case "yesterday":
		return dayEdge(n.AddDate(0, 0, -1), e, loc), nil
	}

	if wd, ok := weekdays[s]; ok {
		// The most recent occurrence, today included.
		d := n
		for range 8 {
			if d.Weekday() == wd {
				return dayEdge(d, e, loc), nil
			}
			d = d.AddDate(0, 0, -1)
		}
	}

	// @unix
	if strings.HasPrefix(s, "@") {
		secs, err := strconv.ParseInt(s[1:], 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q is not a unix timestamp", s)
		}
		return time.Unix(secs, 0).In(loc), nil
	}

	// A bare Slack ts, so a value copied out of previous output works.
	if strings.Count(s, ".") == 1 {
		if ts, err := Parse(s); err == nil && !ts.IsZero() {
			return ts.Time().In(loc), nil
		}
	}

	// Absolute forms, most specific first.
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	} {
		if t, err := time.ParseInLocation(layout, strings.ToUpper(s), loc); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return dayEdge(t, e, loc), nil
	}

	// Relative: always into the past, for both bounds.
	if d, err := parseRelative(s); err == nil {
		return n.Add(-d), nil
	}

	return time.Time{}, fmt.Errorf("%q is not a time; write it like 7d, 2h, today, yesterday, mon, "+
		"2026-09-01, 2026-09-01T14:30 or @1725806400", s)
}

// dayEdge snaps to the start or the end of a calendar day in loc.
//
// The end is the last representable nanosecond rather than the next midnight,
// so that --until 2026-09-01 includes everything that happened that day.
func dayEdge(t time.Time, e edge, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	if e == endOfDay {
		return time.Date(y, m, d, 23, 59, 59, 999999999, loc)
	}
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// parseRelative handles durations including the day and week units
// time.ParseDuration rejects.
//
// Months and years are deliberately absent: "1m" already means a minute to
// anyone who has used a Go duration, and silently reading it as a month would
// be a 43000-fold surprise.
func parseRelative(s string) (time.Duration, error) {
	if len(s) > 1 {
		unit := s[len(s)-1]
		if unit == 'd' || unit == 'w' {
			nStr := s[:len(s)-1]
			v, err := strconv.ParseFloat(nStr, 64)
			if err != nil {
				return 0, fmt.Errorf("%q is not a duration", s)
			}
			hours := v * 24
			if unit == 'w' {
				hours *= 7
			}
			return time.Duration(hours * float64(time.Hour)), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("%q is negative; --since and --until both look backwards already", s)
	}
	return d, nil
}
