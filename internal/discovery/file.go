package discovery

import (
	"bufio"
	"fmt"
	"os"
)

// ServerListFromFILE retrieves a list of servers from a file.
func (d *Discovery) ServerListFromFILE() (servers []string, retErr error) {
	d.logger.Debug("Retrieving server list from file", d.server)

	file, err := os.Open(d.server)
	if err != nil {
		return nil, fmt.Errorf("open server discovery file %q: %w", d.server, err)
	}
	defer func() {
		if err := file.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("close server discovery file %q: %w", d.server, err)
		}
	}()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		servers = append(servers, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan server discovery file %q: %w", d.server, err)
	}

	return servers, nil
}
