package discovery

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// serverFileCommentPrefix starts a comment line in a server list file.
const serverFileCommentPrefix = "#"

// utf8BOM is the UTF-8 byte order mark some editors write at the start of a
// file. strings.TrimSpace does not remove it (U+FEFF is no white space), so
// without stripping it the first entry became an invalid host name.
const utf8BOM = "\uFEFF"

// ServerListFromFILE retrieves a list of servers from a file, one server per
// line. Each line is trimmed (see trimServerEntry), and blank lines as well as
// lines starting with '#' after trimming are skipped. Without that, a trailing
// blank line, an empty separator line or an indented entry became an empty or
// whitespace-wrapped address, which the client rejects, aborting the whole
// client before it connected to any server. A UTF-8 byte order mark at the
// start of the file is stripped first, and whitespace around the file path
// is ignored (see serverListFilePath). A file with no server entry yields an
// empty list, which the client rejects as no server to connect to.
func (d *Discovery) ServerListFromFILE() (servers []string, retErr error) {
	path := serverListFilePath(d.server)
	d.logger.Debug("Retrieving server list from file", path)

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open server discovery file %q: %w", path, err)
	}
	defer func() {
		if err := file.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("close server discovery file %q: %w", path, err)
		}
	}()

	scanner := bufio.NewScanner(file)
	for first := true; scanner.Scan(); first = false {
		line := scanner.Text()
		if first {
			line = strings.TrimPrefix(line, utf8BOM)
		}
		server, ok := trimServerEntry(line)
		if !ok || strings.HasPrefix(server, serverFileCommentPrefix) {
			continue
		}
		servers = append(servers, server)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan server discovery file %q: %w", path, err)
	}

	return servers, nil
}
