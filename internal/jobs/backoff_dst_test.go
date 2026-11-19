package jobs

import (
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/config"
)

// On the day daylight saving time ends the end hour of a TimeRange may come
// twice; the range ends at its first occurrence, when the scheduler stops
// running the job by the hour of the clock.
func TestTimeRangeEndUsesFirstOccurrenceOfRepeatedHour(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no time zone database:", err)
	}
	job := &config.Scheduled{TimeRange: [2]int{0, 2}}
	// 2026-10-25: 03:00 CEST becomes 02:00 CET, so 02:xx comes twice.
	now := time.Date(2026, time.October, 25, 1, 3, 0, 0, berlin)
	end := timeRangeEnd(job, now)
	if end.Hour() != 2 || end.Add(-time.Minute).Hour() != 1 {
		t.Fatalf("range end %v is not the first 02:00 of the day", end)
	}

	// An ordinary day and a range ending at midnight are unchanged.
	now = time.Date(2026, time.November, 10, 12, 0, 0, 0, berlin)
	if got, want := timeRangeEnd(job, now), time.Date(2026, time.November, 10, 2, 0, 0, 0, berlin); !got.Equal(want) {
		t.Fatalf("range end = %v, want %v", got, want)
	}
	job.TimeRange = [2]int{0, 24}
	if got, want := timeRangeEnd(job, now), time.Date(2026, time.November, 11, 0, 0, 0, 0, berlin); !got.Equal(want) {
		t.Fatalf("range end = %v, want %v", got, want)
	}
}
