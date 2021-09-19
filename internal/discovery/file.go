package discovery

import (
	"bufio"
	"os"

	"github.com/mimecast/dtail/internal/io/dlog"
)

// ServerListFromFILE retrieves a list of servers from a file.
func (d *Discovery) ServerListFromFILE() (servers []string) {
	dlog.Common.Debug("Retrieving server list from file", d.server)

	file, err := os.Open(d.server)
	if err != nil {
		dlog.Common.FatalPanic(d.server, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		servers = append(servers, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		dlog.Common.FatalPanic(d.server, err)
	}

	return
}
