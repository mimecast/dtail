package discovery

import (
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/mimecast/dtail/internal/io/logger"
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
	// To specifiy optional server discovery module options.
	options string
	// To either filter a server list or to secify an exact list.
	server string
	// To filter server list.
	regex *regexp.Regexp
	// How to order the server list.
	order ServerOrder
}

// New returns a new discovery method.
func New(method, server string, order ServerOrder) *Discovery {
	module := method
	options := ""

	if strings.Contains(module, ":") {
		s := strings.Split(module, ":")
		if len(s) != 2 {
			logger.FatalExit("Unable to parse discovery module", module)
		}
		module = s[0]
		options = s[1]
	}

	d := Discovery{
		module:  strings.ToUpper(module),
		options: options,
		server:  server,
		order:   order,
	}

	if strings.HasPrefix(server, "/") && strings.HasSuffix(server, "/") {
		d.initRegex()
	}

	return &d
}

func (d *Discovery) initRegex() {
	var runes []rune
	last := len(d.server) - 1
	for i, char := range d.server {
		if i != 0 && i != last {
			runes = append(runes, char)
		}
	}

	regexStr := string(runes)
	logger.Debug("Using filter regex", regexStr)

	regex, err := regexp.Compile(regexStr)
	if err != nil {
		logger.FatalExit("Could not compile regex", regexStr, err)
	}

	d.regex = regex
	d.server = ""
}

// ServerList to connect to via DTail client.
func (d *Discovery) ServerList() []string {
	servers := d.serverListFromModule()

	if d.regex != nil {
		servers = d.filterList(servers)
	}

	servers = d.dedupList(servers)

	if d.order == Shuffle {
		servers = d.shuffleList(servers)
	}

	logger.Debug("Discovered servers", len(servers), servers)
	return servers
}

func (d *Discovery) serverListFromModule() []string {
	if d.module != "" {
		return d.serverListFromReflectedModule()
	}

	if _, err := os.Stat(d.server); err == nil {
		// Appears to be a file name, now try to read from that file.
		return d.ServerListFromFILE()
	}

	// Appears to be a list of FQDNs (or a single FQDN)
	return d.ServerListFromCOMMA()
}

// The aim of this is that everyone can plug in their own server discovery
// method to DTail. Just add a method ServerListFrommMODULENAME to type
// Discovery. Whereas MODULENAME must be a upeprcase string.
func (d *Discovery) serverListFromReflectedModule() []string {
	methodName := fmt.Sprintf("ServerListFrom%s", d.module)

	rt := reflect.TypeOf(d)
	reflectedMethod, ok := rt.MethodByName(methodName)
	if !ok {
		logger.FatalExit("No such server discovery module", d.module, methodName)
	}

	inputValues := make([]reflect.Value, 1)
	// Thist input value is method receiver.
	inputValues[0] = reflect.ValueOf(d)
	returnValues := reflectedMethod.Func.Call(inputValues)

	// First return value is server list.
	return returnValues[0].Interface().([]string)
}

// Filter server list based on a regexp.
func (d *Discovery) filterList(servers []string) (filtered []string) {
	logger.Debug("Filtering server list")

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

	logger.Info("Deduped server list", len(servers), len(deduped))
	return
}

// Randomly shuffle the server list.
func (d *Discovery) shuffleList(servers []string) []string {
	logger.Debug("Shuffling server list")

	r := rand.New(rand.NewSource(time.Now().Unix()))
	shuffled := make([]string, len(servers))
	n := len(servers)

	for i := 0; i < n; i++ {
		randIndex := r.Intn(len(servers))
		shuffled[i] = servers[randIndex]
		servers = append(servers[:randIndex], servers[randIndex+1:]...)
	}

	return shuffled
}
