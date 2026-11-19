package jobs

import (
	"strings"
	"time"
)

func fillDates(str string) string {
	return fillDatesAt(str, time.Now())
}

// fillDatesAt replaces the date placeholders in str with the dates at now.
func fillDatesAt(str string, now time.Time) string {
	yyyesterday := now.Add(3 * -24 * time.Hour).Format("20060102")
	str = strings.ReplaceAll(str, "$yyyesterday", yyyesterday)

	yyesterday := now.Add(2 * -24 * time.Hour).Format("20060102")
	str = strings.ReplaceAll(str, "$yyesterday", yyesterday)

	yesterday := now.Add(1 * -24 * time.Hour).Format("20060102")
	str = strings.ReplaceAll(str, "$yesterday", yesterday)

	today := now.Format("20060102")
	str = strings.ReplaceAll(str, "$today", today)

	tomorrow := now.Add(1 * 24 * time.Hour).Format("20060102")
	return strings.ReplaceAll(str, "$tomorrow", tomorrow)
}
