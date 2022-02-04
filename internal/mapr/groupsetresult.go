package mapr

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/protocol"
)

// Result returns a nicely formated result of the query from the group set.
func (g *GroupSet) Result(query *Query, rowsLimit int) (string, int, error) {
	rows, columnWidths, err := g.result(query, true)
	if err != nil {
		return "", 0, err
	}
	if query.Limit != -1 {
		rowsLimit = query.Limit
	}
	lastColumn := len(query.Select) - 1

	sb := pool.BuilderBuffer.Get().(*strings.Builder)
	defer pool.RecycleBuilderBuffer(sb)

	g.resultWriteFormattedHeader(query, sb, lastColumn, rowsLimit, columnWidths)
	g.resultWriteFormattedHeaderRowSeparator(query, sb, lastColumn, columnWidths)
	g.resultWriteFormattedData(query, sb, lastColumn, rowsLimit, columnWidths, rows)

	return sb.String(), len(rows), nil
}

// Write a nicely formatted header for the result data.
func (g *GroupSet) resultWriteFormattedHeader(query *Query, sb *strings.Builder,
	lastColumn, rowsLimit int, columnWidths []int) {

	for i, sc := range query.Select {
		format := fmt.Sprintf(" %%%ds ", columnWidths[i])
		str := fmt.Sprintf(format, sc.FieldStorage)

		g.resultWriteFormattedHeaderEntry(query, sb, sc, str)
		if i == lastColumn {
			continue
		}
		g.resultWriteFormattedHeaderEntrySeparator(query, sb)

	}
	sb.WriteString("\n")
}

func (g *GroupSet) resultWriteFormattedHeaderEntry(query *Query, sb *strings.Builder,
	sc selectCondition, str string) {

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
}

func (g *GroupSet) resultWriteFormattedHeaderEntrySeparator(query *Query, sb *strings.Builder) {
	if config.Client.TermColorsEnable {
		color.PaintWithAttr(sb, protocol.FieldDelimiter,
			config.Client.TermColors.MaprTable.HeaderDelimiterFg,
			config.Client.TermColors.MaprTable.HeaderDelimiterBg,
			config.Client.TermColors.MaprTable.HeaderDelimiterAttr)
	} else {
		sb.WriteString(protocol.FieldDelimiter)
	}
}

// This writes a nicely formatted line separating the header and the data.
func (g *GroupSet) resultWriteFormattedHeaderRowSeparator(query *Query, sb *strings.Builder,
	lastColumn int, columnWidths []int) {

	for i := 0; i < len(query.Select); i++ {
		str := fmt.Sprintf("-%s-", strings.Repeat("-", columnWidths[i]))
		if config.Client.TermColorsEnable {
			color.PaintWithAttr(sb, str,
				config.Client.TermColors.MaprTable.HeaderDelimiterFg,
				config.Client.TermColors.MaprTable.HeaderDelimiterBg,
				config.Client.TermColors.MaprTable.HeaderDelimiterAttr)
		} else {
			sb.WriteString(str)
		}
		if i == lastColumn {
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
}

// Write the result data nicely formatted.
func (g *GroupSet) resultWriteFormattedData(query *Query, sb *strings.Builder,
	lastColumn, rowsLimit int, columnWidths []int, rows []result) {

	for i, r := range rows {
		if i == rowsLimit {
			break
		}
		for j, value := range r.values {
			g.resultWriteFormattedDataEntry(query, sb, columnWidths, j, value)
			if j == lastColumn {
				continue
			}
			// Now, write the data entry separator.
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
}

func (g *GroupSet) resultWriteFormattedDataEntry(query *Query, sb *strings.Builder,
	columnWidths []int, j int, value string) {

	format := fmt.Sprintf(" %%%ds ", columnWidths[j])
	str := fmt.Sprintf(format, value)
	if config.Client.TermColorsEnable {
		color.PaintWithAttr(sb, str,
			config.Client.TermColors.MaprTable.DataFg,
			config.Client.TermColors.MaprTable.DataBg,
			config.Client.TermColors.MaprTable.DataAttr)
	} else {
		sb.WriteString(str)
	}
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

	return g.resultWriteUnformatted(query, rows, tmpOutfile, fd)
}

func (g *GroupSet) resultWriteUnformatted(query *Query, rows []result, tmpOutfile string,
	fd *os.File) error {

	// Generate header now
	lastColumn := len(query.Select) - 1
	for i, sc := range query.Select {
		fd.WriteString(sc.FieldStorage)
		if i == lastColumn {
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
			if j == lastColumn {
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
