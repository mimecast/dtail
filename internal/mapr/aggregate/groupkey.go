package aggregate

import (
	"github.com/mimecast/dtail/internal/protocol"
)

// buildGroupKey appends the values of the group-by fields to dst and returns
// the extended buffer. Callers pass a reusable buffer (typically scratch[:0])
// so that building a key allocates nothing on the per-line path; the returned
// bytes borrow both that buffer and the field values, hence whoever stores the
// key must copy it (see serializer.aggregate).
func buildGroupKey(dst []byte, groupBy []string, fields map[string]string) []byte {
	if len(groupBy) == 0 {
		return dst
	}

	for i, field := range groupBy {
		if i > 0 {
			dst = append(dst, protocol.AggregateGroupKeyCombinator...)
		}
		dst = append(dst, fields[field]...)
	}

	return dst
}
