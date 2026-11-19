package aggregate

import (
	"github.com/mimecast/dtail/internal/protocol"
)

// buildGroupKey appends the values of the group-by fields to dst and returns
// the extended buffer. Callers pass a reusable buffer (typically scratch[:0])
// so that building a key allocates nothing on the per-line path; the returned
// bytes borrow that buffer, hence whoever stores the key must copy it (see
// serializer.aggregate).
// Compound keys escape commas and percent signs within each component, so
// tuple boundaries cannot collide with field contents. Ordinary compound keys
// and all single-field keys retain their legacy spelling. Clients treat keys
// as opaque; the escape introduces no aggregate or transport delimiters.
func buildGroupKey(dst []byte, groupBy []string, fields map[string]string) []byte {
	if len(groupBy) == 0 {
		return dst
	}
	if len(groupBy) == 1 {
		return append(dst, fields[groupBy[0]]...)
	}

	for i, field := range groupBy {
		if i > 0 {
			dst = append(dst, protocol.AggregateGroupKeyCombinator...)
		}
		value := fields[field]
		start := 0
		for j := 0; j < len(value); j++ {
			if value[j] != ',' && value[j] != '%' {
				continue
			}
			dst = append(dst, value[start:j]...)
			if value[j] == ',' {
				dst = append(dst, "%2C"...)
			} else {
				dst = append(dst, "%25"...)
			}
			start = j + 1
		}
		dst = append(dst, value[start:]...)
	}

	return dst
}
