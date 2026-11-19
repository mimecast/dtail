package mapr

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/protocol"
)

// Result returns a nicely formated result of the query from the group set.
func (g *GroupSet) Result(query *Query, rowsLimit int, renderer ResultRenderer) (string, int, error) {
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

	if renderer == nil {
		renderer = PlainResultRenderer()
	}

	g.resultWriteFormattedHeader(query, renderer, sb, lastColumn, columnWidths)
	g.resultWriteFormattedHeaderRowSeparator(query, renderer, sb, lastColumn, columnWidths)
	g.resultWriteFormattedData(renderer, sb, lastColumn, rowsLimit, columnWidths, rows)

	return sb.String(), len(rows), nil
}

// Write a nicely formatted header for the result data.
func (g *GroupSet) resultWriteFormattedHeader(query *Query, renderer ResultRenderer, sb *strings.Builder,
	lastColumn int, columnWidths []int) {

	for i, sc := range query.Select {
		format := fmt.Sprintf(" %%%ds ", columnWidths[i])
		str := fmt.Sprintf(format, sc.FieldStorage)

		g.resultWriteFormattedHeaderEntry(query, renderer, sb, sc, str)
		if i == lastColumn {
			continue
		}
		g.resultWriteFormattedHeaderEntrySeparator(renderer, sb)

	}
	sb.WriteString("\n")
}

func (g *GroupSet) resultWriteFormattedHeaderEntry(query *Query, renderer ResultRenderer, sb *strings.Builder,
	sc selectCondition, str string) {

	isGroupKey := false
	for _, groupBy := range query.GroupBy {
		if sc.FieldStorage == groupBy {
			isGroupKey = true
			break
		}
	}
	renderer.WriteHeaderEntry(sb, str, sc.FieldStorage == query.OrderBy, isGroupKey)
}

func (g *GroupSet) resultWriteFormattedHeaderEntrySeparator(renderer ResultRenderer, sb *strings.Builder) {
	renderer.WriteHeaderDelimiter(sb, protocol.FieldDelimiter)
}

// This writes a nicely formatted line separating the header and the data.
func (g *GroupSet) resultWriteFormattedHeaderRowSeparator(query *Query, renderer ResultRenderer, sb *strings.Builder,
	lastColumn int, columnWidths []int) {

	for i := 0; i < len(query.Select); i++ {
		str := fmt.Sprintf("-%s-", strings.Repeat("-", columnWidths[i]))
		renderer.WriteHeaderDelimiter(sb, str)
		if i == lastColumn {
			continue
		}
		renderer.WriteHeaderDelimiter(sb, protocol.FieldDelimiter)
	}
	sb.WriteString("\n")
}

// Write the result data nicely formatted.
func (g *GroupSet) resultWriteFormattedData(renderer ResultRenderer, sb *strings.Builder,
	lastColumn, rowsLimit int, columnWidths []int, rows []result) {

	for i, r := range rows {
		if i == rowsLimit {
			break
		}
		for j, value := range r.values {
			g.resultWriteFormattedDataEntry(renderer, sb, columnWidths, j, value)
			if j == lastColumn {
				continue
			}
			renderer.WriteDataDelimiter(sb, protocol.FieldDelimiter)
		}
		sb.WriteString("\n")
	}
}

func (g *GroupSet) resultWriteFormattedDataEntry(renderer ResultRenderer, sb *strings.Builder,
	columnWidths []int, j int, value string) {

	format := fmt.Sprintf(" %%%ds ", columnWidths[j])
	str := fmt.Sprintf(format, value)
	renderer.WriteDataEntry(sb, str)
}

func (g *GroupSet) writeQueryFile(query *Query) error {
	queryFile := fmt.Sprintf("%s.query", query.Outfile.FilePath)
	tmpQueryFile := fmt.Sprintf("%s.tmp", queryFile)
	g.logger.Debug("Writing query file", queryFile)

	fd, err := os.OpenFile(tmpQueryFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return fmt.Errorf("open temporary query file %q: %w", tmpQueryFile, err)
	}

	_, writeErr := fd.WriteString(query.RawQuery)
	if err := closeAndRenameWrittenFile(fd, writeErr, tmpQueryFile, queryFile, os.Rename, os.Remove); err != nil {
		return fmt.Errorf("write query file %q: %w", queryFile, err)
	}
	return nil
}

// WriteResult writes the result to an CSV outfile.
func (g *GroupSet) WriteResult(query *Query, finalResult bool) error {
	if !query.HasOutfile() {
		return errors.New("no outfile specified")
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
		if info, statErr := os.Stat(query.Outfile.FilePath); statErr == nil && info.Size() > 0 {
			writeHeader = false
		}
	}

	fd, err := g.getOutfileFD(query)
	if err != nil {
		return err
	}

	writeErr := g.resultWriteUnformatted(query, rows, fd, writeHeader)
	if query.Outfile.AppendMode {
		return closeWrittenFile(fd, writeErr, query.Outfile.FilePath)
	}

	tmpOutfile := fmt.Sprintf("%s.tmp", query.Outfile.FilePath)
	g.logger.Debug("Renaming outfile", tmpOutfile, "to", query.Outfile.FilePath)
	if err := closeAndRenameWrittenFile(fd, writeErr, tmpOutfile, query.Outfile.FilePath, os.Rename, os.Remove); err != nil {
		return fmt.Errorf("commit outfile %q: %w", query.Outfile.FilePath, err)
	}
	g.logger.Info("Successfully renamed outfile to", query.Outfile.FilePath)
	return nil
}

func (g *GroupSet) getOutfileFD(query *Query) (*os.File, error) {
	if !query.Outfile.AppendMode {
		g.logger.Info("Writing to outfile", query.Outfile.FilePath)
		tmpOutfile := fmt.Sprintf("%s.tmp", query.Outfile.FilePath)
		return os.OpenFile(tmpOutfile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	}

	g.logger.Info("Appending to outfile", query.Outfile.FilePath)
	return os.OpenFile(query.Outfile.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
}

func (g *GroupSet) resultWriteUnformatted(query *Query, rows []result, fd io.StringWriter, writeHeader bool) error {
	lastColumn := len(query.Select) - 1

	if writeHeader {
		if err := g.resultWriteUnformattedHeader(query, fd, lastColumn); err != nil {
			return err
		}
	}

	// And now write the data
	for i, r := range rows {
		if i == query.Limit {
			break
		}
		for j, value := range r.values {
			if _, err := fd.WriteString(value); err != nil {
				return err
			}
			if j == lastColumn {
				continue
			}
			if _, err := fd.WriteString(protocol.CSVDelimiter); err != nil {
				return err
			}
		}
		if _, err := fd.WriteString("\n"); err != nil {
			return err
		}
	}

	return nil
}

func closeWrittenFile(fd io.Closer, writeErr error, path string) error {
	closeErr := fd.Close()
	var errs []error
	if writeErr != nil {
		errs = append(errs, fmt.Errorf("write %q: %w", path, writeErr))
	}
	if closeErr != nil {
		errs = append(errs, fmt.Errorf("close %q: %w", path, closeErr))
	}
	return errors.Join(errs...)
}

func closeAndRenameWrittenFile(fd io.Closer, writeErr error, tmpPath, finalPath string,
	rename func(string, string) error, remove func(string) error) error {

	if err := closeWrittenFile(fd, writeErr, tmpPath); err != nil {
		return errors.Join(err, removeTemporaryFile(tmpPath, remove))
	}
	if err := rename(tmpPath, finalPath); err != nil {
		return errors.Join(
			fmt.Errorf("rename %q to %q: %w", tmpPath, finalPath, err),
			removeTemporaryFile(tmpPath, remove),
		)
	}
	return nil
}

func removeTemporaryFile(path string, remove func(string) error) error {
	if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove temporary file %q: %w", path, err)
	}
	return nil
}

func (g *GroupSet) resultWriteUnformattedHeader(query *Query, fd io.StringWriter, lastColumn int) (err error) {
	for i, sc := range query.Select {
		if _, err = fd.WriteString(sc.FieldStorage); err != nil {
			return
		}
		if i == lastColumn {
			continue
		}
		if _, err = fd.WriteString(protocol.CSVDelimiter); err != nil {
			return
		}
	}
	_, err = fd.WriteString("\n")
	return
}
