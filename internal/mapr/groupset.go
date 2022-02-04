package mapr

import (
	"context"
	"fmt"
	"sort"
	"strconv"
)

// GroupSet represents a map of aggregate sets. The group sets
// are requierd by the "group by" mapr clause, whereas the
// group set map keys are the values of the "group by" arguments.
// E.g. "group by $cid" would create one aggregate set and one map
// entry per customer id.
type GroupSet struct {
	sets map[string]*AggregateSet
}

// Internal helper type
type result struct {
	groupKey     string
	values       []string
	columnWidths []int
	orderBy      float64
}

// NewGroupSet returns a new empty group set.
func NewGroupSet() *GroupSet {
	g := GroupSet{}
	g.InitSet()
	return &g
}

// String representation of the group set.
func (g *GroupSet) String() string {
	return fmt.Sprintf("GroupSet(%v)", g.sets)
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

// Serialize the group set (e.g. to send it over the wire).
func (g *GroupSet) Serialize(ctx context.Context, ch chan<- string) {
	for groupKey, set := range g.sets {
		set.Serialize(ctx, groupKey, ch)
	}
}

// Return a sorted result slice of the query from the group set.
func (g *GroupSet) result(query *Query, gathercolumnWidths bool) ([]result, []int, error) {
	var err error
	var rows []result

	// Helpers for calculating the ASCII table output (output is the terminal and
	// not a CSV file).
	columnWidths := make([]int, len(query.Select))
	var valueStrLen int

	for groupKey, set := range g.sets {
		result := result{groupKey: groupKey}

		for i, sc := range query.Select {
			if valueStrLen, err = g.resultSelect(query, &sc, set, &result); err != nil {
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
		rows = append(rows, result)
	}

	g.resultOrderBy(query, rows)
	return rows, columnWidths, nil
}

func (*GroupSet) resultSelect(query *Query, sc *selectCondition, set *AggregateSet,
	result *result) (int, error) {

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
		value = set.FValues[sc.FieldStorage] / float64(set.Samples)
		valueStr = fmt.Sprintf("%f", value)
	default:
		return 0, fmt.Errorf("Unknown aggregation method '%v'", sc.Operation)
	}

	if sc.FieldStorage == query.OrderBy {
		result.orderBy = value
	}
	result.values = append(result.values, valueStr)

	return len(valueStr), nil
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
