package handlers

import (
	"fmt"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/mapr/logformat"
)

func newHandlerTestAggregate(queryText, defaultLogFormat string) (*mapaggregate.Aggregate, error) {
	logger := logging.NopLogger{}
	query, err := mapr.NewQuery(queryText, logger)
	if err != nil {
		return nil, err
	}
	parser, err := logformat.NewParserWithHostname(
		query.EffectiveLogFormat(defaultLogFormat), query, "handler-test")
	if err != nil {
		parser, err = logformat.NewParserWithHostname("generic", query, "handler-test")
		if err != nil {
			return nil, fmt.Errorf("create handler test parser: %w", err)
		}
	}
	return mapaggregate.New(query, parser, "handler-test", logger)
}
