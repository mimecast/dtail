package mapr

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/mimecast/dtail/internal/logging"
)

// GroupSet represents a map of aggregate sets. The group sets
// are requierd by the "group by" mapr clause, whereas the
// group set map keys are the values of the "group by" arguments.
// E.g. "group by $cid" would create one aggregate set and one map
// entry per customer id.
type GroupSet struct {
	sets   map[string]*AggregateSet
	logger logging.Logger
}

// Internal helper type
type result struct {
	groupKey     string
	values       []string
	columnWidths []int
	orderBy      float64
}

type resultStats struct {
	percentageTotals map[string]float64
	percentileValues map[string][]float64
}

// NewGroupSet returns a new empty group set.
func NewGroupSet(logger logging.Logger) *GroupSet {
	g := GroupSet{logger: logging.OrNop(logger)}
	g.InitSet()
	return &g
}

// String representation of the group set.
func (g *GroupSet) String() string {
	return fmt.Sprintf("GroupSet(%v)", g.sets)
}

// IsEmpty reports whether the group set contains any aggregate rows.
func (g *GroupSet) IsEmpty() bool {
	return len(g.sets) == 0
}

// InitSet makes the group set empty (initialize).
func (g *GroupSet) InitSet() {
	g.sets = make(map[string]*AggregateSet)
}

// GetSet gets a specific aggregate set from the group set.
func (g *GroupSet) GetSet(groupKey string) *AggregateSet {
	set, ok := g.sets[groupKey]
	if !ok {
		set = NewAggregateSet()
		g.sets[groupKey] = set
	}
	return set
}

// Serialize the group set (e.g. to send it over the wire). If the context is
// cancelled mid-iteration, the remaining unsent aggregate sets are returned
// so callers can retry them (for example, by re-merging them into the live
// aggregation state). The returned map is nil when every entry was sent.
func (g *GroupSet) Serialize(ctx context.Context, ch chan<- string) map[string]*AggregateSet {
	var remaining map[string]*AggregateSet
	aborted := false
	for groupKey, set := range g.sets {
		if aborted {
			if remaining == nil {
				remaining = make(map[string]*AggregateSet, len(g.sets))
			}
			remaining[groupKey] = set
			continue
		}
		if !set.Serialize(ctx, groupKey, ch, g.logger) {
			aborted = true
			if remaining == nil {
				remaining = make(map[string]*AggregateSet, len(g.sets))
			}
			remaining[groupKey] = set
		}
	}
	return remaining
}

// ResetWith replaces the underlying sets map. A nil argument is equivalent
// to InitSet. This is the supported way for callers in the same package to
// restore unsent data returned from Serialize without reaching into the
// unexported sets field.
func (g *GroupSet) ResetWith(sets map[string]*AggregateSet) {
	if sets == nil {
		g.InitSet()
		return
	}
	g.sets = sets
}

// Return a sorted result slice of the query from the group set.
//
// Rows are built in lexicographic groupKey order first. This guarantees a
// stable, deterministic base ordering before any OrderBy sort is applied.
// Without the pre-sort, Go's intentionally randomised map iteration would
// make output order non-deterministic when OrderBy is empty, and would
// produce non-deterministic tie-breaks when multiple rows share the same
// OrderBy value (SortStable preserves incoming order, so random map order
// propagated directly into tied rows).
func (g *GroupSet) result(query *Query, gathercolumnWidths bool) ([]result, []int, error) {
	var err error
	var rows []result

	// Helpers for calculating the ASCII table output (output is the terminal and
	// not a CSV file).
	columnWidths := make([]int, len(query.Select))
	var valueStrLen int
	stats := g.makeResultStats(query)

	// Collect and sort group keys lexicographically so that the row slice is
	// built in a deterministic order. SortStable in resultOrderBy then
	// preserves this order for tied OrderBy values.
	keys := sortedGroupKeys(g.sets)

	for _, groupKey := range keys {
		set := g.sets[groupKey]
		row := result{groupKey: groupKey}

		for i, sc := range query.Select {
			if valueStrLen, err = g.resultSelect(query, &sc, set, &row, &stats); err != nil {
				return rows, columnWidths, err
			}

			// Do we want to gather the table withs? This is required to print out a decent
			// ASCII formated table (table output is the terminal and not a CSV file).
			if !gathercolumnWidths {
				continue
			}
			if columnWidths[i] < len(sc.FieldStorage) {
				columnWidths[i] = len(sc.FieldStorage)
			}
			if columnWidths[i] < valueStrLen {
				columnWidths[i] = valueStrLen
			}
		}
		rows = append(rows, row)
	}

	g.resultOrderBy(query, rows)
	return rows, columnWidths, nil
}

// sortedGroupKeys returns the keys of the given sets map sorted
// lexicographically. This helper centralises the deterministic key extraction
// used by result() and makeResultStats() to guarantee consistent iteration
// order regardless of Go's runtime map randomisation.
func sortedGroupKeys(sets map[string]*AggregateSet) []string {
	keys := make([]string, 0, len(sets))
	for k := range sets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (*GroupSet) resultSelect(query *Query, sc *selectCondition, set *AggregateSet,
	result *result, stats *resultStats) (int, error) {

	var valueStr string
	var value float64

	switch sc.Operation {
	case Count:
		value = set.FValues[sc.FieldStorage]
		valueStr = fmt.Sprintf("%d", int(value))
	case Len:
		fallthrough
	case Sum:
		fallthrough
	case Min:
		fallthrough
	case Max:
		value = set.FValues[sc.FieldStorage]
		valueStr = fmt.Sprintf("%f", value)
	case Last:
		valueStr = set.SValues[sc.FieldStorage]
		value, _ = strconv.ParseFloat(valueStr, 64)
	case Avg:
		// Guard against division by zero when an empty aggregate set (Samples==0)
		// is received from the server. Without this guard, 0/0 yields NaN, which
		// propagates as the string "NaN" into CSV/terminal output.
		if set.Samples == 0 {
			value = 0
		} else {
			value = set.FValues[sc.FieldStorage] / float64(set.Samples)
		}
		valueStr = fmt.Sprintf("%f", value)
	case Percentage:
		value = set.FValues[sc.FieldStorage]
		total := stats.percentageTotals[sc.FieldStorage]
		if total == 0 {
			value = 0
		} else {
			value = (value / total) * 100
		}
		valueStr = fmt.Sprintf("%f", value)
	case Percentile:
		value = percentileRank(set.FValues[sc.FieldStorage], stats.percentileValues[sc.FieldStorage])
		valueStr = fmt.Sprintf("%f", value)
	default:
		return 0, fmt.Errorf("unknown aggregation method '%v'", sc.Operation)
	}

	if sc.FieldStorage == query.OrderBy {
		result.orderBy = value
	}
	result.values = append(result.values, valueStr)

	return len(valueStr), nil
}

func (g *GroupSet) makeResultStats(query *Query) resultStats {
	stats := resultStats{
		percentageTotals: make(map[string]float64),
		percentileValues: make(map[string][]float64),
	}

	for _, set := range g.sets {
		for _, sc := range query.Select {
			value := set.FValues[sc.FieldStorage]
			switch sc.Operation {
			case Percentage:
				stats.percentageTotals[sc.FieldStorage] += value
			case Percentile:
				stats.percentileValues[sc.FieldStorage] = append(stats.percentileValues[sc.FieldStorage], value)
			}
		}
	}

	for storage := range stats.percentileValues {
		sort.Float64s(stats.percentileValues[storage])
	}

	return stats
}

func percentileRank(value float64, sortedValues []float64) float64 {
	if len(sortedValues) == 0 {
		return 0
	}

	upperBound := sort.Search(len(sortedValues), func(i int) bool {
		return sortedValues[i] > value
	})
	return (float64(upperBound) / float64(len(sortedValues))) * 100
}

func (*GroupSet) resultOrderBy(query *Query, rows []result) {
	if query.OrderBy == "" {
		return
	}
	if query.ReverseOrder {
		sort.SliceStable(rows, func(i, j int) bool {
			return rows[i].orderBy < rows[j].orderBy
		})
	} else {
		sort.SliceStable(rows, func(i, j int) bool {
			return rows[i].orderBy > rows[j].orderBy
		})
	}
}
