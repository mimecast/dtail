package jobs

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata"
)

func TestFillDatesAtCalendarDays(t *testing.T) {
	t.Parallel()
	sofia, err := time.LoadLocation("Europe/Sofia")
	if err != nil {
		t.Fatal(err)
	}
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		now  time.Time
		want string // Three days ago through tomorrow, in placeholder order.
	}{
		{"spring early", time.Date(2026, 3, 30, 0, 30, 0, 0, sofia), "20260327,20260328,20260329,20260330,20260331"},
		{"spring late", time.Date(2026, 3, 28, 23, 30, 0, 0, sofia), "20260325,20260326,20260327,20260328,20260329"},
		{"spring two days ago", time.Date(2026, 3, 31, 0, 30, 0, 0, sofia), "20260328,20260329,20260330,20260331,20260401"},
		{"spring three days ago", time.Date(2026, 4, 1, 0, 30, 0, 0, sofia), "20260329,20260330,20260331,20260401,20260402"},
		{"autumn early", time.Date(2026, 10, 25, 0, 30, 0, 0, sofia), "20261022,20261023,20261024,20261025,20261026"},
		{"autumn late", time.Date(2026, 10, 25, 23, 30, 0, 0, sofia), "20261022,20261023,20261024,20261025,20261026"},
		{"autumn next day", time.Date(2026, 10, 26, 0, 30, 0, 0, sofia), "20261023,20261024,20261025,20261026,20261027"},
		{"skipped midnight tomorrow", time.Date(2026, 9, 5, 0, 30, 0, 0, santiago), "20260902,20260903,20260904,20260905,20260906"},
		{"skipped midnight yesterday", time.Date(2026, 9, 7, 0, 30, 0, 0, santiago), "20260904,20260905,20260906,20260907,20260908"},
		{"skipped midnight two days ago", time.Date(2026, 9, 8, 0, 30, 0, 0, santiago), "20260905,20260906,20260907,20260908,20260909"},
		{"skipped midnight three days ago", time.Date(2026, 9, 9, 0, 30, 0, 0, santiago), "20260906,20260907,20260908,20260909,20260910"},
		{"local date before UTC date", time.Date(2026, 9, 5, 23, 30, 0, 0, santiago), "20260902,20260903,20260904,20260905,20260906"},
		{"UTC control", time.Date(2026, 3, 30, 0, 30, 0, 0, time.UTC), "20260327,20260328,20260329,20260330,20260331"},
		{"fixed offset control", time.Date(2026, 10, 25, 23, 30, 0, 0, time.FixedZone("fixed", -7*3600)), "20261022,20261023,20261024,20261025,20261026"},
		{"year boundary", time.Date(2027, 1, 1, 0, 30, 0, 0, sofia), "20261229,20261230,20261231,20270101,20270102"},
		{"leap day", time.Date(2024, 2, 29, 23, 30, 0, 0, sofia), "20240226,20240227,20240228,20240229,20240301"},
		{"after leap day", time.Date(2024, 3, 1, 0, 30, 0, 0, sofia), "20240227,20240228,20240229,20240301,20240302"},
	}
	const placeholders = "$yyyesterday,$yyesterday,$yesterday,$today,$tomorrow"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Repeat every placeholder, retaining unrelated text verbatim.
			input := "logs/" + strings.Repeat(placeholders+"/", 2) + "$unknown"
			want := "logs/" + strings.Repeat(tt.want+"/", 2) + "$unknown"
			if got := fillDatesAt(input, tt.now); got != want {
				t.Fatalf("fillDatesAt at %v = %q, want %q", tt.now, got, want)
			}
		})
	}
}
