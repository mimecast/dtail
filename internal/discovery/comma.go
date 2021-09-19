package discovery

import (
	"strings"

	"github.com/mimecast/dtail/internal/io/dlog"
)

// ServerListFromCOMMA retrieves a list of servers from comma separated input list.
func (d *Discovery) ServerListFromCOMMA() []string {
	dlog.Common.Debug("Retrieving server list from comma separated list", d.server)
	return strings.Split(d.server, ",")
}
