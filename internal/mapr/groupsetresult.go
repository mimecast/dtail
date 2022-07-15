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
	queryFile := fmt.Sprintf("%s.query", query.Outfile.FilePath)
	tmpQueryFile := fmt.Sprintf("%s.tmp", queryFile)
	dlog.Common.Debug("Writing query file", queryFile)

	fd, err := os.OpenFile(tmpQueryFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
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

	// By default, also write the CSV header.
	writeHeader := true

	// In append mode, only write CSV header when file doesn't exist yet or is empty.
	if query.Outfile.AppendMode {
		if info, err := os.Stat(query.Outfile.FilePath); err == nil && info.Size() > 0 {
			writeHeader = false
		}
	}

	fd, err := g.getOutfileFD(query)
	if err != nil {
		return err
	}
	defer fd.Close()

	return g.resultWriteUnformatted(query, rows, fd, writeHeader)
}

func (g *GroupSet) getOutfileFD(query *Query) (*os.File, error) {
	if !query.Outfile.AppendMode {
		dlog.Common.Info("Writing to outfile", query.Outfile.FilePath)
		tmpOutfile := fmt.Sprintf("%s.tmp", query.Outfile.FilePath)
		return os.OpenFile(tmpOutfile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	}

	dlog.Common.Info("Appending to outfile", query.Outfile.FilePath)
	return os.OpenFile(query.Outfile.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
}

func (g *GroupSet) resultWriteUnformatted(query *Query, rows []result, fd *os.File, writeHeader bool) error {
	lastColumn := len(query.Select) - 1

	if writeHeader {
		g.resultWriteUnformattedHeader(query, fd, lastColumn)
	}

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

	if !query.Outfile.AppendMode {
		tmpOutfile := fmt.Sprintf("%s.tmp", query.Outfile.FilePath)
		if err := os.Rename(tmpOutfile, query.Outfile.FilePath); err != nil {
			os.Remove(tmpOutfile)
			return err
		}
	}

	return nil
}

func (g *GroupSet) resultWriteUnformattedHeader(query *Query, fd *os.File, lastColumn int) {
	for i, sc := range query.Select {
		fd.WriteString(sc.FieldStorage)
		if i == lastColumn {
			continue
		}
		fd.WriteString(protocol.CSVDelimiter)
	}
	fd.WriteString("\n")
}
