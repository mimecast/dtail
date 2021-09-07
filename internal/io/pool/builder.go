package pool

import (
	"strings"
	"sync"
)

var BuilderBuffer = sync.Pool{
	New: func() interface{} {
		sb := strings.Builder{}
		return &sb
	},
}

func RecycleBuilderBuffer(sb *strings.Builder) {
	sb.Reset()
	BuilderBuffer.Put(sb)
}
