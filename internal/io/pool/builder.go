package pool

import (
	"strings"
	"sync"
)

// BuilderBuffer is there to optimize memory allocations (DTail allocates a lot
// of memory while reading log data otherwise)
var BuilderBuffer = sync.Pool{
	New: func() interface{} {
		sb := strings.Builder{}
		return &sb
	},
}

// RecycleBuilderBuffer recycles the buffer again.
func RecycleBuilderBuffer(sb *strings.Builder) {
	sb.Reset()
	BuilderBuffer.Put(sb)
}
