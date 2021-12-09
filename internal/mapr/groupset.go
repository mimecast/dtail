package mapr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/protocol"
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
	groupKey string
	values   []string
	widths   []int
	orderBy  float64
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
func (g *GroupSet) Result(query *Query, rowsLimit int) (string, int, error) {
	rows, widths, err := g.result(query, true)
	if err != nil {
		return "", 0, err
	}
	if query.Limit != -1 {
		rowsLimit = query.Limit
	}

	sb := pool.BuilderBuffer.Get().(*strings.Builder)
	defer pool.RecycleBuilderBuffer(sb)

	// Generate header now
	lastIndex := len(query.Select) - 1
	for i, sc := range query.Select {
		format := fmt.Sprintf(" %%%ds ", widths[i])
		str := fmt.Sprintf(format, sc.FieldStorage)
		if config.Client.TermColorsEnable {
			attrs := []color.Attribute{config.Client.TermColors.MaprTable.HeaderAttr}
			if sc.FieldStorage == query.OrderBy {
				attrs = append(attrs, config.Client.TermColors.MaprTable.HeaderSortKeyAttr)
			}

			for _, groupBy := range query.GroupBy {
				if sc.FieldStorage == groupBy {
					attrs = append(attrs, config.Client.TermColors.MaprTable.HeaderGroupKeyAttr)
					break
				}
			}

			color.PaintWithAttrs(sb, str,
				config.Client.TermColors.MaprTable.HeaderFg,
				config.Client.TermColors.MaprTable.HeaderBg,
				attrs)
		} else {
			sb.WriteString(str)
		}

		if i == lastIndex {
			continue
		}
		if config.Client.TermColorsEnable {
			color.PaintWithAttr(sb, protocol.FieldDelimiter,
				config.Client.TermColors.MaprTable.HeaderDelimiterFg,
				config.Client.TermColors.MaprTable.HeaderDelimiterBg,
				config.Client.TermColors.MaprTable.HeaderDelimiterAttr)
		} else {
			sb.WriteString(protocol.FieldDelimiter)
		}
	}
	sb.WriteString("\n")

	for i := 0; i < len(query.Select); i++ {
		str := fmt.Sprintf("-%s-", strings.Repeat("-", widths[i]))
		if config.Client.TermColorsEnable {
			color.PaintWithAttr(sb, str,
				config.Client.TermColors.MaprTable.HeaderDelimiterFg,
				config.Client.TermColors.MaprTable.HeaderDelimiterBg,
				config.Client.TermColors.MaprTable.HeaderDelimiterAttr)
		} else {
			sb.WriteString(str)
		}
		if i == lastIndex {
			continue
		}
		if config.Client.TermColorsEnable {
			color.PaintWithAttr(sb, protocol.FieldDelimiter,
				config.Client.TermColors.MaprTable.HeaderDelimiterFg,
				config.Client.TermColors.MaprTable.HeaderDelimiterBg,
				config.Client.TermColors.MaprTable.HeaderDelimiterAttr)
		} else {
			sb.WriteString(protocol.FieldDelimiter)
		}
	}
	sb.WriteString("\n")

	// And now write the data
	for i, r := range rows {
		if i == rowsLimit {
			break
		}
		for j, value := range r.values {
			format := fmt.Sprintf(" %%%ds ", widths[j])
			str := fmt.Sprintf(format, value)
			if config.Client.TermColorsEnable {
				color.PaintWithAttr(sb, str,
					config.Client.TermColors.MaprTable.DataFg,
					config.Client.TermColors.MaprTable.DataBg,
					config.Client.TermColors.MaprTable.DataAttr)
			} else {
				sb.WriteString(str)
			}

			if j == lastIndex {
				continue
			}
			if config.Client.TermColorsEnable {
				color.PaintWithAttr(sb, protocol.FieldDelimiter,
					config.Client.TermColors.MaprTable.DelimiterFg,
					config.Client.TermColors.MaprTable.DelimiterBg,
					config.Client.TermColors.MaprTable.DelimiterAttr)
			} else {
				sb.WriteString(protocol.FieldDelimiter)
			}
		}
		sb.WriteString("\n")
	}

	return sb.String(), len(rows), nil
}

func (*GroupSet) writeQueryFile(query *Query) error {
	queryFile := fmt.Sprintf("%s.query", query.Outfile)
	tmpQueryFile := fmt.Sprintf("%s.tmp", queryFile)
	dlog.Common.Debug("Writing query file", queryFile)

	fd, err := os.Create(tmpQueryFile)
	if err != nil {
		return err
	}
	defer fd.Close()

	fd.WriteString(query.RawQuery)
	os.Rename(tmpQueryFile, queryFile)
	return nil
}

// WriteResult writes the result to an CSV outfile.
func (g *GroupSet) WriteResult(query *Query) error {
	if !query.HasOutfile() {
		return errors.New("No outfile specified")
	}
	if err := g.writeQueryFile(query); err != nil {
		return err
	}

	rows, _, err := g.result(query, false)
	if err != nil {
		return err
	}

	dlog.Common.Info("Writing outfile", query.Outfile)
	tmpOutfile := fmt.Sprintf("%s.tmp", query.Outfile)

	fd, err := os.Create(tmpOutfile)
	if err != nil {
		return err
	}
	defer fd.Close()

	return g.writeResult(query, rows, tmpOutfile, fd)
}

func (g *GroupSet) writeResult(query *Query, rows []result, tmpOutfile string,
	fd *os.File) error {

	// Generate header now
	lastIndex := len(query.Select) - 1
	for i, sc := range query.Select {
		fd.WriteString(sc.FieldStorage)
		if i == lastIndex {
			continue
		}
		fd.WriteString(protocol.CSVDelimiter)
	}
	fd.WriteString("\n")

	// And now write the data
	for i, r := range rows {
		if i == query.Limit {
			break
		}
		for j, value := range r.values {
			fd.WriteString(value)
			if j == lastIndex {
				continue
			}
			fd.WriteString(protocol.CSVDelimiter)
		}
		fd.WriteString("\n")
	}

	if err := os.Rename(tmpOutfile, query.Outfile); err != nil {
		os.Remove(tmpOutfile)
		return err
	}

	return nil
}

// Return a sorted result slice of the query from the group set.
func (g *GroupSet) result(query *Query, gatherWidths bool) ([]result, []int, error) {
	var err error
	var rows []result

	// Helpers for calculating the ASCII table output (output is the terminal and
	// not a CSV file).
	widths := make([]int, len(query.Select))
	var valueStrLen int

	for groupKey, set := range g.sets {
		result := result{groupKey: groupKey}

		for i, sc := range query.Select {
			if valueStrLen, err = g.resultSelect(query, &sc, set, &result); err != nil {
				return rows, widths, err
			}

			// Do we want to gather the table withs? This is required to print out a decent
			// ASCII formated table (table output is the terminal and not a CSV file).
			if !gatherWidths {
				continue
			}
			if widths[i] < len(sc.FieldStorage) {
				widths[i] = len(sc.FieldStorage)
			}
			if widths[i] < valueStrLen {
				widths[i] = valueStrLen
			}
		}
		rows = append(rows, result)
	}

	if query.OrderBy != "" {
		g.resultOrderBy(query, rows)
	}

	return rows, widths, nil
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
