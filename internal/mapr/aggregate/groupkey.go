package aggregate

import (
	"strings"

	"github.com/mimecast/dtail/internal/protocol"
)

func buildGroupKey(groupBy []string, fields map[string]string) string {
	if len(groupBy) == 0 {
		return ""
	}

	total := 0
	for _, field := range groupBy {
		total += len(fields[field])
	}
	total += (len(groupBy) - 1) * len(protocol.AggregateGroupKeyCombinator)

	var sb strings.Builder
	sb.Grow(total)

	for i, field := range groupBy {
		if i > 0 {
			sb.WriteString(protocol.AggregateGroupKeyCombinator)
		}
		sb.WriteString(fields[field])
	}

	return sb.String()
}
