// Package trigger keeps scheduled reminders and tasks: what to do, when
// (once, or by a cron expression), and for which chat. They are stored in a
// SQLite file, so they survive a restart, and a Scheduler fires them.
package trigger

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed cron expression: minute, hour, day of month, month and day
// of week, as in crontab(5), evaluated in a time zone.
//
// Supported: "*", lists ("1,15"), ranges ("9-17"), steps ("*/10", "0-30/5"),
// month names (jan..dec) and day names (sun..sat), "?" for "*", and the macros
// @yearly (@annually), @monthly, @weekly, @daily (@midnight) and @hourly. When
// both the day of month and the day of week are restricted a day matches if it
// fits either, as in Vixie cron. Not supported: seconds, L, W and #.
type Cron struct {
	minute, hour, dom, month, dow uint64 // bit sets
	domAny, dowAny                bool   // the field was "*" (or "?")
	expr                          string
}

// String returns the expression as written.
func (c *Cron) String() string { return c.expr }

var macros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var (
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dayNames   = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
)

// ParseCron parses a cron expression.
func ParseCron(expr string) (*Cron, error) {
	orig := strings.TrimSpace(expr)
	if m, ok := macros[strings.ToLower(orig)]; ok {
		expr = m
	} else if strings.HasPrefix(orig, "@") {
		return nil, fmt.Errorf("unknown schedule %q (use @hourly, @daily, @weekly, @monthly, @yearly or five fields)", orig)
	} else {
		expr = orig
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("a cron expression has five fields (minute hour day-of-month month day-of-week), got %d in %q", len(f), orig)
	}
	c := &Cron{expr: orig}
	var err error
	if c.minute, _, err = parseField(f[0], 0, 59, nil, "minute"); err != nil {
		return nil, err
	}
	if c.hour, _, err = parseField(f[1], 0, 23, nil, "hour"); err != nil {
		return nil, err
	}
	if c.dom, c.domAny, err = parseField(f[2], 1, 31, nil, "day of month"); err != nil {
		return nil, err
	}
	if c.month, _, err = parseField(f[3], 1, 12, monthNames, "month"); err != nil {
		return nil, err
	}
	if c.dow, c.dowAny, err = parseField(f[4], 0, 7, dayNames, "day of week"); err != nil {
		return nil, err
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow |= 1
	}
	c.dow &^= 1 << 7
	return c, nil
}

// parseField reads one field into a bit set; any is true for "*" and "?".
func parseField(s string, lo, hi int, names map[string]int, what string) (set uint64, any bool, err error) {
	if s == "*" || s == "?" {
		for i := lo; i <= hi; i++ {
			set |= 1 << uint(i)
		}
		if what == "day of week" {
			set &^= 1 << 7
		}
		return set, true, nil
	}
	value := func(v string) (int, error) {
		if n, ok := names[strings.ToLower(v)]; ok {
			return n, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return 0, fmt.Errorf("%s: %q is not a value from %d to %d", what, v, lo, hi)
		}
		return n, nil
	}
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			return 0, false, fmt.Errorf("%s: empty item in %q", what, s)
		}
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 0, false, fmt.Errorf("%s: bad step in %q", what, part)
			}
			step = n
		}
		from, to := lo, hi
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var e1, e2 error
			from, e1 = value(a)
			to, e2 = value(b)
			if e1 != nil {
				return 0, false, e1
			}
			if e2 != nil {
				return 0, false, e2
			}
			if from > to {
				return 0, false, fmt.Errorf("%s: the range %q goes backwards", what, rng)
			}
		default:
			v, err := value(rng)
			if err != nil {
				return 0, false, err
			}
			from, to = v, v
			if hasStep { // "5/15" means from 5 to the end
				to = hi
			}
		}
		for i := from; i <= to; i += step {
			set |= 1 << uint(i)
		}
	}
	if what == "day of week" {
		// "*/2" and the like can set bit 7 (Sunday): fold it into 0
		if set&(1<<7) != 0 {
			set |= 1
		}
	}
	return set, false, nil
}

// matchesDay reports whether the date is one the expression allows.
func (c *Cron) matchesDay(t time.Time) bool {
	if c.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	domOK := c.dom&(1<<uint(t.Day())) != 0
	dowOK := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dowOK
	case c.dowAny:
		return domOK
	default:
		return domOK || dowOK
	}
}

// Next returns the first time after "after" that the expression allows, in
// loc, or false when there is none within five years (an impossible date such
// as 31 February). Times that do not exist in loc (the hour skipped when the
// clocks go forward) are skipped.
func (c *Cron) Next(after time.Time, loc *time.Location) (time.Time, bool) {
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	limit := after.AddDate(5, 0, 0)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	for first := true; day.Before(limit); first = false {
		if c.matchesDay(day) {
			for h := 0; h < 24; h++ {
				if c.hour&(1<<uint(h)) == 0 {
					continue
				}
				for m := 0; m < 60; m++ {
					if c.minute&(1<<uint(m)) == 0 {
						continue
					}
					cand := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
					if cand.Hour() != h || cand.Minute() != m { // a time the clocks skip
						continue
					}
					if first && cand.Before(t) {
						continue
					}
					if cand.After(after) {
						return cand, true
					}
				}
			}
		}
		day = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc)
	}
	return time.Time{}, false
}

// MinGap returns the shortest time between consecutive runs among the next n.
func (c *Cron) MinGap(from time.Time, loc *time.Location, n int) time.Duration {
	prev, ok := c.Next(from, loc)
	if !ok {
		return 0
	}
	gap := time.Duration(1<<62 - 1)
	for i := 0; i < n; i++ {
		next, ok := c.Next(prev, loc)
		if !ok {
			break
		}
		if d := next.Sub(prev); d < gap {
			gap = d
		}
		prev = next
	}
	return gap
}
