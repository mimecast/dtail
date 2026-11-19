package clients

import (
	"fmt"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

func maprRegexForQuery(query *mapr.Query) string {
	if query == nil {
		return "."
	}

	switch query.Table {
	case "", ".":
		return "."
	case "*":
		return "\\|MAPREDUCE:\\|"
	default:
		return fmt.Sprintf("\\|MAPREDUCE:%s\\|", query.Table)
	}
}

func maprRegexFromQueryString(queryStr string, logger logging.Logger) (*mapr.Query, string, error) {
	query, err := mapr.NewQuery(queryStr, logging.OrNop(logger))
	if err != nil {
		return nil, "", err
	}
	return query, maprRegexForQuery(query), nil
}
