package discovery

import (
	"bufio"
	"github.com/mimecast/dtail/internal/io/logger"
	"os"
)

// ServerListFromFILE retrieves a list of servers from a file.
func (d *Discovery) ServerListFromFILE() (servers []string) {
	logger.Debug("Retrieving server list from file", d.server)

	file, err := os.Open(d.server)
	if err != nil {
		logger.FatalExit(d.server, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		servers = append(servers, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		logger.FatalExit(d.server, err)
	}

	return
}
