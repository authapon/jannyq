package trigger

import (
	"strings"
	"testing"
	"time"
)

var bkk = time.FixedZone("ICT", 7*3600)

func at(s string, loc *time.Location) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		panic(err)
	}
	return t
}

func TestCronNext(t *testing.T) {
	for _, tc := range []struct {
		expr, after, want string
	}{
		{"0 7 * * *", "2026-10-09 06:59", "2026-10-09 07:00"},        // every day at 7:00
		{"0 7 * * *", "2026-10-09 07:00", "2026-10-10 07:00"},        // strictly after
		{"0 7 * * *", "2026-10-09 07:00", "2026-10-10 07:00"},        //
		{"*/15 * * * *", "2026-10-09 10:07", "2026-10-09 10:15"},     // steps
		{"*/15 * * * *", "2026-10-09 10:45", "2026-10-09 11:00"},     //
		{"30 9 * * mon-fri", "2026-10-09 10:00", "2026-10-12 09:30"}, // Friday after the time -> Monday
		{"30 9 * * 1-5", "2026-10-09 09:00", "2026-10-09 09:30"},
		{"0 9,17 * * *", "2026-10-09 09:30", "2026-10-09 17:00"}, // lists
		{"0 0 1 * *", "2026-10-09 10:00", "2026-11-01 00:00"},    // monthly
		{"0 12 25 dec *", "2026-10-09 10:00", "2026-12-25 12:00"},
		{"@daily", "2026-10-09 10:00", "2026-10-10 00:00"},
		{"@hourly", "2026-10-09 10:20", "2026-10-09 11:00"},
		{"@weekly", "2026-10-09 10:00", "2026-10-11 00:00"}, // the coming Sunday
		{"@monthly", "2026-12-15 10:00", "2027-01-01 00:00"},
		{"@yearly", "2026-10-09 10:00", "2027-01-01 00:00"},
		{"0 8 * * 0", "2026-10-09 10:00", "2026-10-11 08:00"}, // Sunday as 0
		{"0 8 * * 7", "2026-10-09 10:00", "2026-10-11 08:00"}, // and as 7
		{"0 8 * * sun", "2026-10-09 10:00", "2026-10-11 08:00"},
		{"10-20/5 * * * *", "2026-10-09 10:00", "2026-10-09 10:10"},
		{"5/20 * * * *", "2026-10-09 10:06", "2026-10-09 10:25"}, // 5, 25, 45
		{"0 0 29 2 *", "2026-10-09 10:00", "2028-02-29 00:00"},   // leap day
		{"0 9 * * ?", "2026-10-09 10:00", "2026-10-10 09:00"},
	} {
		c, err := ParseCron(tc.expr)
		if err != nil {
			t.Errorf("%q: %v", tc.expr, err)
			continue
		}
		got, ok := c.Next(at(tc.after, bkk), bkk)
		if !ok || !got.Equal(at(tc.want, bkk)) {
			t.Errorf("%q after %s: got %v (%v), want %s", tc.expr, tc.after, got, ok, tc.want)
		}
	}
}

func TestCronDayOfMonthAndDayOfWeekEitherMatches(t *testing.T) {
	// Vixie cron: with both restricted a day fits if it matches either
	c, _ := ParseCron("0 9 13 * fri")
	got, _ := c.Next(at("2026-10-09 10:00", bkk), bkk) // Fri 9 Oct is past 9:00; next is Tue 13 Oct (day of month)
	if !got.Equal(at("2026-10-13 09:00", bkk)) {
		t.Errorf("got %v", got)
	}
	got, _ = c.Next(at("2026-10-13 10:00", bkk), bkk) // then Fri 16 Oct (day of week)
	if !got.Equal(at("2026-10-16 09:00", bkk)) {
		t.Errorf("got %v", got)
	}
}

func TestCronImpossibleDate(t *testing.T) {
	c, err := ParseCron("0 0 31 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Next(at("2026-10-09 10:00", bkk), bkk); ok {
		t.Error("31 February never comes")
	}
}

func TestCronParseErrors(t *testing.T) {
	for expr, want := range map[string]string{
		"":                "five fields",
		"* * * *":         "five fields",
		"* * * * * *":     "five fields",
		"60 * * * *":      "minute",
		"* 24 * * *":      "hour",
		"* * 0 * *":       "day of month",
		"* * * 13 *":      "month",
		"* * * * 8":       "day of week",
		"*/0 * * * *":     "step",
		"5-1 * * * *":     "backwards",
		"a * * * *":       "minute",
		"1,,2 * * * *":    "empty",
		"@sometimes":      "unknown schedule",
		"0 0 L * *":       "day of month",
		"0 0 * * fri#2":   "day of week",
		"0 0 * * 1-mon-3": "day of week",
	} {
		if _, err := ParseCron(expr); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want one mentioning %q", expr, err, want)
		}
	}
}

func TestCronInATimeZoneWithDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no time zone data")
	}
	// 2026-03-08 02:30 does not exist in New York (clocks go from 02:00 to 03:00)
	c, _ := ParseCron("30 2 * * *")
	got, _ := c.Next(at("2026-03-07 03:00", ny), ny)
	if !got.Equal(at("2026-03-09 02:30", ny)) {
		t.Errorf("a skipped hour is skipped: got %v", got)
	}
	// and the same wall-clock time stays the same across the change
	c, _ = ParseCron("0 7 * * *")
	a, _ := c.Next(at("2026-03-07 06:00", ny), ny)
	b, _ := c.Next(a, ny)
	if a.Hour() != 7 || b.Hour() != 7 || b.Sub(a) != 23*time.Hour {
		t.Errorf("07:00 before and after the change: %v then %v (%v apart)", a, b, b.Sub(a))
	}
	// the same instant in another zone gives another next run
	c, _ = ParseCron("0 7 * * *")
	utc, _ := c.Next(at("2026-10-09 00:00", time.UTC), time.UTC)
	if !utc.Equal(at("2026-10-09 07:00", time.UTC)) {
		t.Errorf("utc: %v", utc)
	}
}

func TestCronMinGap(t *testing.T) {
	from := at("2026-10-09 10:00", bkk)
	for expr, want := range map[string]time.Duration{
		"* * * * *":    time.Minute,
		"*/15 * * * *": 15 * time.Minute,
		"0 7 * * *":    24 * time.Hour,
		"0 9,10 * * *": time.Hour,
		"0 7 * * mon":  7 * 24 * time.Hour,
	} {
		c, err := ParseCron(expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.MinGap(from, bkk, 20); got != want {
			t.Errorf("%q: gap %v, want %v", expr, got, want)
		}
	}
}
