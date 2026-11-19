package jobs

import (
	"strings"
	"time"
)

// fillDatesAt replaces date placeholders with calendar dates in now's location.
func fillDatesAt(str string, now time.Time) string {
	// Keep the local calendar date, but do date arithmetic in UTC so a
	// skipped local wall time (including midnight) cannot change the day.
	year, month, day := now.Date()
	date := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	yyyesterday := date.AddDate(0, 0, -3).Format("20060102")
	str = strings.ReplaceAll(str, "$yyyesterday", yyyesterday)

	yyesterday := date.AddDate(0, 0, -2).Format("20060102")
	str = strings.ReplaceAll(str, "$yyesterday", yyesterday)

	yesterday := date.AddDate(0, 0, -1).Format("20060102")
	str = strings.ReplaceAll(str, "$yesterday", yesterday)

	today := date.Format("20060102")
	str = strings.ReplaceAll(str, "$today", today)

	tomorrow := date.AddDate(0, 0, 1).Format("20060102")
	return strings.ReplaceAll(str, "$tomorrow", tomorrow)
}
