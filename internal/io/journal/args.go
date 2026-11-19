package journal

import (
	"strings"

	"github.com/mimecast/dtail/internal/io/fs"
)

// UnitArgs returns the journalctl arguments selecting the entries of the
// journal: read target spec ("journal:unit.service" selects "-u
// unit.service"), or none for a spec without a unit. The read command and the
// field probe's sampler both build their journalctl invocation from it.
func UnitArgs(spec string) []string {
	source := strings.TrimPrefix(spec, fs.JournalSpecPrefix)
	if source == "" {
		return nil
	}
	return []string{"-u", source}
}
