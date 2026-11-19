package discovery

import "strings"

// ServerListFromCOMMA retrieves a list of servers from comma separated input
// list. Each entry is trimmed and empty entries are skipped (see
// trimServerEntry), so "a, b" and "a,,b," both yield [a b]. Only an empty
// input yields the sentinel [""], one entry naming no server. Without a
// discovery module the client then runs serverless (config sets
// Args.Serverless for an empty --servers without --discovery, see
// transformClient) and opens a single serverless session for that entry.
// With a discovery module the client is not serverless and drops the entry
// "", so it fails as no server to connect to (see
// clients.connectableServers), just like input of only whitespace and
// commas, which yields an empty list.
func (d *Discovery) ServerListFromCOMMA() ([]string, error) {
	d.logger.Debug("Retrieving server list from comma separated list", d.server)

	if d.server == "" {
		return []string{""}, nil
	}
	rawServers := strings.Split(d.server, ",")
	servers := make([]string, 0, len(rawServers))
	for _, raw := range rawServers {
		if server, ok := trimServerEntry(raw); ok {
			servers = append(servers, server)
		}
	}

	return servers, nil
}
