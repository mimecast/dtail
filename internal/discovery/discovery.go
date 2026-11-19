package discovery

import (
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/logging"
)

// ServerOrder to specify how to sort the server list.
type ServerOrder int

const (
	// Shuffle the server list?
	Shuffle ServerOrder = iota
)

// Discovery method for discovering a list of available DTail servers.
type Discovery struct {
	// To plug in a custom server discovery module.
	module string
	// Optional module options, the part after the colon of "MODULE:options".
	// They are only an extension point for modules a build adds; the built-in
	// COMMA and FILE modules read the server value instead and take no
	// options: New rejects "comma:a,b" or "file:/path" (see
	// rejectBuiltInOptions), as such a value does not carry the server list.
	options string
	// To either filter a server list or to secify an exact list.
	server string
	// To filter server list.
	regex *regexp.Regexp
	// How to order the server list.
	order ServerOrder
	// Logger for discovery diagnostics.
	logger logging.Logger
}

// New returns a new discovery method. The method is a module name, optionally
// followed by ":options" (see the options field). For the built-in COMMA and
// FILE modules, the server list (or FILE's list file) comes from server, and
// options are rejected; only modules a build adds may read their options.
func New(method, server string, order ServerOrder, logger logging.Logger) (*Discovery, error) {
	module, options, err := parseMethod(method)
	if err != nil {
		return nil, err
	}
	if err := rejectBuiltInOptions(module, options); err != nil {
		return nil, err
	}

	d := Discovery{
		module:  strings.ToUpper(module),
		options: options,
		server:  server,
		order:   order,
		logger:  logging.OrNop(logger),
	}
	if d.module != "" {
		methodName := fmt.Sprintf("ServerListFrom%s", d.module)
		if _, ok := reflect.TypeOf(&d).MethodByName(methodName); !ok {
			return nil, fmt.Errorf("unknown server discovery module %q", method)
		}
	}

	// Whitespace around a /regex/ value is ignored like around every server
	// entry (see trimServerEntry), so " /web/ " filters by the regex web and
	// does not end up as the host "/web/".
	if isRegexServer(server) {
		d.server = strings.TrimSpace(server)
		if err := d.initRegex(); err != nil {
			return nil, err
		}
	}

	return &d, nil
}

// parseMethod splits a discovery method "MODULE[:options]" into its module
// name and options. Options may contain further colons; a method with a colon
// but an empty module name or empty options is rejected.
func parseMethod(method string) (module, options string, err error) {
	module, options, found := strings.Cut(method, ":")
	if found && (module == "" || options == "") {
		return "", "", fmt.Errorf("unable to parse discovery module %q", method)
	}
	return module, options, nil
}

// rejectBuiltInOptions rejects options for the built-in COMMA and FILE
// modules, which read their servers from the server value and take no
// options. Before they were rejected, an old documented form such
// as "comma:server1,server2" with an empty server value was accepted
// silently: a scheduled job then queried this dserver instead (see
// jobs.jobServersStr), wrote a wrong outfile and was skipped for the rest of
// its period. Now the client fails to configure its discovery, so the job
// writes nothing and logs why.
//
// The module name is normalized with strings.ToUpper, as New does to look the
// module up as a ServerListFrom method, so both always agree on which module a
// name selects. strings.EqualFold would not: "fıle" (dotless ı) upper-cases
// to FILE and selects the FILE module, but does not fold to "file", so its
// options would slip through. The error keeps the name as it was given.
func rejectBuiltInOptions(module, options string) error {
	if options == "" {
		return nil
	}
	if upper := strings.ToUpper(module); upper == "COMMA" || upper == "FILE" {
		return fmt.Errorf("discovery module %q takes no options; put the servers in Servers/--servers", module)
	}
	return nil
}

// isRegexServer reports whether the server value, without surrounding
// whitespace, is a /regex/ filter rather than a server list.
func isRegexServer(server string) bool {
	trimmed := strings.TrimSpace(server)
	return strings.HasPrefix(trimmed, "/") && strings.HasSuffix(trimmed, "/")
}

// initRegex compiles the server value, already trimmed and enclosed in
// slashes, as the server filter regex and clears the server value.
func (d *Discovery) initRegex() error {
	var runes []rune
	last := len(d.server) - 1
	for i, char := range d.server {
		if i != 0 && i != last {
			runes = append(runes, char)
		}
	}

	regexStr := string(runes)
	d.logger.Debug("Using filter regex", regexStr)
	regex, err := regexp.Compile(regexStr)
	if err != nil {
		return fmt.Errorf("compile server discovery regex %q: %w", regexStr, err)
	}

	d.regex = regex
	d.server = ""
	return nil
}

// ServerList to connect to via DTail client. The FILE and COMMA modules
// already trim their entries and skip empty ones, so the deduplication below
// sees "host" and " host" as the same server. The regex filter only has
// entries to filter from a custom discovery module, one that does not read
// the server value: a /regex/ server value is no server list (initRegex
// clears it), so the FILE and COMMA modules, explicit or implied, see an
// empty server value. Without a module the result is then empty, the
// explicit COMMA module yields only the serverless sentinel "" (which the
// filter rarely keeps, and a client that is not serverless drops anyway) and
// the explicit FILE module fails to open the file "".
//
// The list can be empty, e.g. a server list file with only comment lines or a
// regex no server matches. ServerList returns it as it is; the client rejects
// it (see baseClient.makeConnections).
func (d *Discovery) ServerList() ([]string, error) {
	servers, err := d.serverListFromModule()
	if err != nil {
		return nil, err
	}

	if d.regex != nil {
		servers = d.filterList(servers)
	}
	servers = d.dedupList(servers)
	if d.order == Shuffle {
		servers = d.shuffleList(servers)
	}

	d.logger.Debug("Discovered servers", len(servers), servers)
	return servers, nil
}

func (d *Discovery) serverListFromModule() ([]string, error) {
	if d.module != "" {
		return d.serverListFromReflectedModule()
	}
	if d.server == "" && d.regex != nil {
		return []string{}, nil
	}
	if IsServerListFile(d.server) {
		// Appears to be a file name, now try to read from that file.
		return d.ServerListFromFILE()
	}
	// Appears to be a list of FQDNs (or a single FQDN)
	return d.ServerListFromCOMMA()
}

// The aim of this is that everyone can plug in their own server discovery
// method to DTail. Just add a method ServerListFromMODULENAME to type
// Discovery, where MODULENAME is an uppercase string. Such a module may read
// the options of "MODULENAME:options" from d.options; the built-in COMMA and
// FILE modules take none (see rejectBuiltInOptions).
func (d *Discovery) serverListFromReflectedModule() ([]string, error) {
	methodName := fmt.Sprintf("ServerListFrom%s", d.module)
	// Now we are reflecting the serve discovery function by it's name.
	rt := reflect.TypeOf(d)
	reflectedMethod, ok := rt.MethodByName(methodName)
	if !ok {
		panic(fmt.Sprintf("validated discovery module %q has no method %s", d.module, methodName))
	}
	inputValues := make([]reflect.Value, 1)
	// Thist input value is method receiver.
	inputValues[0] = reflect.ValueOf(d)
	returnValues := reflectedMethod.Func.Call(inputValues)
	if len(returnValues) != 2 {
		panic(fmt.Sprintf("discovery method %s has invalid return signature", methodName))
	}
	servers, ok := returnValues[0].Interface().([]string)
	if !ok {
		panic(fmt.Sprintf("discovery method %s does not return []string", methodName))
	}
	if returnValues[1].IsNil() {
		return servers, nil
	}
	err, ok := returnValues[1].Interface().(error)
	if !ok {
		panic(fmt.Sprintf("discovery method %s does not return error", methodName))
	}
	return nil, fmt.Errorf("run server discovery module %q: %w", d.module, err)
}

// IsServerListFile reports whether server names an existing file, which
// ServerList then reads as a server list file when no discovery module is
// given. Whitespace around the value is ignored like around every server
// entry, so "servers.txt " is the file servers.txt and not the host
// "servers.txt". The scheduler uses the same check to tell whether the server
// list of a job can change between its waves.
func IsServerListFile(server string) bool {
	// A directory is no server list: "--servers web1" run from a directory
	// with a subdirectory web1 (or log) means the host of that name. Pipes
	// and devices (e.g. a process substitution) remain readable lists.
	info, err := os.Stat(serverListFilePath(server))
	return err == nil && !info.IsDir()
}

// serverListFilePath returns the path of the server list file server names:
// server without surrounding whitespace.
func serverListFilePath(server string) string {
	return strings.TrimSpace(server)
}

// trimServerEntry trims surrounding whitespace from one raw entry of the FILE
// or COMMA module and reports whether anything is left. Both modules use it
// so that their entries are clean before ServerList filters them by regex and
// deduplicates them: the client rejects an empty or whitespace-wrapped
// address, and a single such entry would otherwise abort the whole client.
func trimServerEntry(raw string) (string, bool) {
	server := strings.TrimSpace(raw)
	return server, server != ""
}

// Filter server list based on a regexp.
func (d *Discovery) filterList(servers []string) (filtered []string) {
	d.logger.Debug("Filtering server list")
	for _, server := range servers {
		if d.regex.MatchString(server) {
			filtered = append(filtered, server)
		}
	}
	return
}

// Deduplicate the server list.
func (d *Discovery) dedupList(servers []string) (deduped []string) {
	serverMap := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		if _, ok := serverMap[server]; !ok {
			serverMap[server] = struct{}{}
			deduped = append(deduped, server)
		}
	}
	d.logger.Debug("Deduped server list", len(servers), len(deduped))
	return
}

// Randomly shuffle the server list. A copy of the input slice is made so the
// caller's backing array is never modified. The seed uses UnixNano so that
// two callers started within the same wall-clock second still get different
// shuffle orders, which is important for client-side load spreading.
func (d *Discovery) shuffleList(servers []string) []string {
	d.logger.Debug("Shuffling server list")

	// Copy to avoid mutating the caller's backing array.
	shuffled := append([]string(nil), servers...)

	// Seed with nanosecond resolution to avoid identical orderings when
	// multiple clients start within the same second.
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	r.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	return shuffled
}
