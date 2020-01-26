package mapr

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"sort"
	"strconv"
	"strings"
)

// GroupSet represents a map of aggregate sets. The group sets
// are requierd by the "group by" mapr clause, whereas the
// group set map keys are the values of the "group by" arguments.
// E.g. "group by $cid" would create one aggregate set and one map
// entry per customer id.
type GroupSet struct {
	sets map[string]*AggregateSet
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

// Result returns a nicely formated result of the query from the group set.
func (g *GroupSet) Result(query *Query) (string, int, error) {
	return g.limitedResult(query, query.Limit, "\t", " ", false)
}

// WriteResult writes the result to an outfile.
func (g *GroupSet) WriteResult(query *Query) error {
	if query.Outfile == "" {
		return errors.New("No outfile specified")
	}

	// -1: Don't limit the result, include all data sets
	result, _, err := g.limitedResult(query, -1, "", ",", true)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(query.Outfile, []byte(result), 0644)
}

// Return a nicely formated result of the query from the group set.
func (g *GroupSet) limitedResult(query *Query, limit int, lineStarter, fieldSeparator string, addHeader bool) (string, int, error) {
	type result struct {
		groupKey  string
		resultStr string
		orderBy   float64
	}

	var resultSlice []result

	for groupKey, set := range g.sets {
		var sb strings.Builder
		r := result{groupKey: groupKey}

		lastIndex := len(query.Select) - 1
		for i, sc := range query.Select {
			storage := sc.FieldStorage
			orderByThis := storage == query.OrderBy

			switch sc.Operation {
			case Count:
				value := set.FValues[storage]
				sb.WriteString(fmt.Sprintf("%d", int(value)))
				if orderByThis {
					r.orderBy = value
				}
			case Len:
				fallthrough
			case Sum:
				fallthrough
			case Min:
				fallthrough
			case Max:
				value := set.FValues[storage]
				sb.WriteString(fmt.Sprintf("%f", value))
				if orderByThis {
					r.orderBy = value
				}
			case Last:
				value := set.SValues[storage]
				if orderByThis {
					f, err := strconv.ParseFloat(value, 64)
					if err == nil {
						r.orderBy = f
					}
				}
				sb.WriteString(value)
			case Avg:
				value := set.FValues[storage] / float64(set.Samples)
				sb.WriteString(fmt.Sprintf("%f", value))
				if orderByThis {
					r.orderBy = value
				}
			default:
				return "", 0, fmt.Errorf("Unknown aggregation method '%v'", sc.Operation)
			}
			if i != lastIndex {
				sb.WriteString(fieldSeparator)
			}
		}

		r.resultStr = sb.String()
		resultSlice = append(resultSlice, r)
	}

	if query.OrderBy != "" {
		if query.ReverseOrder {
			sort.SliceStable(resultSlice, func(i, j int) bool {
				return resultSlice[i].orderBy < resultSlice[j].orderBy
			})
		} else {
			sort.SliceStable(resultSlice, func(i, j int) bool {
				return resultSlice[i].orderBy > resultSlice[j].orderBy
			})
		}
	}

	var sb strings.Builder

	// Write header first
	if addHeader {
		lastIndex := len(query.Select) - 1
		sb.WriteString(lineStarter)
		for i, sc := range query.Select {
			sb.WriteString(sc.FieldStorage)
			if i != lastIndex {
				sb.WriteString(fieldSeparator)
			}
		}
		sb.WriteString("\n")
	}

	// And now write the data
	for i, r := range resultSlice {
		if i == limit {
			break
		}
		sb.WriteString(lineStarter)
		sb.WriteString(r.resultStr)
		sb.WriteString("\n")
	}

	return sb.String(), len(resultSlice), nil
}
