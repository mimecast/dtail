package config

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

// Args is a helper struct to summarize common client arguments.
type Args struct {
	lcontext.LContext
	Arguments             []string
	AuthorizedKeysPath    string
	ConfigFile            string
	ConnectionsPerCPU     int
	ControlTTYPath        string
	Discovery             string
	HostnameOverride      string
	HostKeyPath           string
	InteractiveQuery      bool
	KnownHostsPath        string
	LogDir                string
	Logger                string
	LogLevel              string
	LogPayload            bool
	Mode                  omode.Mode
	NoAuthKey             bool
	NoColor               bool
	QueryStr              string
	Quiet                 bool
	RegexInvert           bool
	RegexStr              string
	SSHAgentKeyIndex      int
	SSHAuthMethods        []gossh.AuthMethod
	SSHBindAddress        string
	SSHHostKeyCallback    gossh.HostKeyCallback
	SSHPort               int
	SSHPrivateKeyFilePath string
	// SSHPrivateKeyFallbackPaths contains additional bootstrap keys selected
	// during configuration initialization. It is not exposed as a CLI flag.
	SSHPrivateKeyFallbackPaths []string
	Serverless                 bool
	ServersStr                 string
	Plain                      bool
	Timeout                    int
	TrustAllHosts              bool
	UserName                   string
	What                       string
}

func (a *Args) String() string {
	var sb strings.Builder

	sb.WriteString("Args(")

	fmt.Fprintf(&sb, "Arguments:%v,", a.Arguments)
	fmt.Fprintf(&sb, "AuthorizedKeysPath:%v,", a.AuthorizedKeysPath)
	fmt.Fprintf(&sb, "ConfigFile:%v,", a.ConfigFile)
	fmt.Fprintf(&sb, "ConnectionsPerCPU:%v,", a.ConnectionsPerCPU)
	fmt.Fprintf(&sb, "ControlTTYPath:%v,", a.ControlTTYPath)
	fmt.Fprintf(&sb, "Discovery:%v,", a.Discovery)
	fmt.Fprintf(&sb, "HostnameOverride:%v,", a.HostnameOverride)
	fmt.Fprintf(&sb, "HostKeyPath:%v,", a.HostKeyPath)
	fmt.Fprintf(&sb, "InteractiveQuery:%v,", a.InteractiveQuery)
	fmt.Fprintf(&sb, "KnownHostsPath:%v,", a.KnownHostsPath)
	fmt.Fprintf(&sb, "LogDir:%v,", a.LogDir)
	fmt.Fprintf(&sb, "LogLevel:%v,", a.LogLevel)
	fmt.Fprintf(&sb, "LogPayload:%v,", a.LogPayload)
	fmt.Fprintf(&sb, "Logger:%v,", a.Logger)
	fmt.Fprintf(&sb, "Mode:%v,", a.Mode)
	fmt.Fprintf(&sb, "NoAuthKey:%v,", a.NoAuthKey)
	fmt.Fprintf(&sb, "NoColor:%v,", a.NoColor)
	fmt.Fprintf(&sb, "QueryStr:%v,", a.QueryStr)
	fmt.Fprintf(&sb, "Quiet:%v,", a.Quiet)
	fmt.Fprintf(&sb, "RegexInvert:%v,", a.RegexInvert)
	fmt.Fprintf(&sb, "RegexStr:%v,", a.RegexStr)
	fmt.Fprintf(&sb, "SSHAgentKeyIndex:%v,", a.SSHAgentKeyIndex)
	fmt.Fprintf(&sb, "SSHAuthMethods:%v,", a.SSHAuthMethods)
	fmt.Fprintf(&sb, "SSHBindAddress:%v,", a.SSHBindAddress)
	fmt.Fprintf(&sb, "SSHHostKeyCallback:%v,", a.SSHHostKeyCallback)
	fmt.Fprintf(&sb, "SSHPrivateKeyFilePath:%v,", a.SSHPrivateKeyFilePath)
	fmt.Fprintf(&sb, "SSHPort:%v,", a.SSHPort)
	fmt.Fprintf(&sb, "Serverless:%v,", a.Serverless)
	fmt.Fprintf(&sb, "ServersStr:%v,", a.ServersStr)
	fmt.Fprintf(&sb, "Plain:%v,", a.Plain)
	fmt.Fprintf(&sb, "Timeout:%v,", a.Timeout)
	fmt.Fprintf(&sb, "TrustAllHosts:%v,", a.TrustAllHosts)
	fmt.Fprintf(&sb, "UserName:%v,", a.UserName)
	fmt.Fprintf(&sb, "What:%v", a.What)
	sb.WriteString(")")

	return sb.String()
}

// SerializeOptions returns a string ready to be sent over the wire to the server.
func (a *Args) SerializeOptions() string {
	options := make(map[string]string)

	if a.Quiet {
		options["quiet"] = fmt.Sprintf("%v", a.Quiet)
	}
	if a.Plain {
		options["plain"] = fmt.Sprintf("%v", a.Plain)
	}
	if a.Serverless {
		options["serverless"] = fmt.Sprintf("%v", a.Serverless)
	}
	if a.MaxCount != 0 {
		options["max"] = fmt.Sprintf("%d", a.MaxCount)
	}
	if a.BeforeContext != 0 {
		options["before"] = fmt.Sprintf("%d", a.BeforeContext)
	}
	if a.AfterContext != 0 {
		options["after"] = fmt.Sprintf("%d", a.AfterContext)
	}

	return serializeOptions(options)
}

func serializeOptions(options map[string]string) string {
	if len(options) == 0 {
		return ""
	}

	keys := make([]string, 0, len(options))
	for k := range options {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(":")
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(serializeOptionValue(options[k]))
	}
	return sb.String()
}

func serializeOptionValue(value string) string {
	if strings.ContainsAny(value, ":=|") || strings.HasPrefix(value, "base64%") {
		return "base64%" + base64.StdEncoding.EncodeToString([]byte(value))
	}

	return value
}

// DeserializeOptions deserializes the options, but into a map.
func DeserializeOptions(opts []string) (map[string]string, lcontext.LContext, error) {
	options := make(map[string]string, len(opts))
	var ltx lcontext.LContext

	if len(opts) == 1 {
		raw := strings.TrimSpace(opts[0])
		if raw == "" {
			return options, ltx, nil
		}
		opts = strings.Split(raw, ":")
	}

	for _, o := range opts {
		kv := strings.SplitN(o, "=", 2)
		if len(kv) != 2 {
			return options, ltx, fmt.Errorf("unable to parse options: %v", kv)
		}
		key := kv[0]
		val := kv[1]

		if strings.HasPrefix(val, "base64%") {
			s := strings.SplitN(val, "%", 2)
			decoded, err := base64.StdEncoding.DecodeString(s[1])
			if err != nil {
				return options, ltx, err
			}
			val = string(decoded)
		}

		var err error
		if options, err = setOption(key, val, options, &ltx); err != nil {
			return options, ltx, err
		}
	}

	return options, ltx, nil
}

func setOption(key, val string, options map[string]string, ltx *lcontext.LContext) (map[string]string, error) {
	switch key {
	case "before":
		iVal, err := strconv.Atoi(val)
		if err != nil {
			return options, err
		}
		ltx.BeforeContext = iVal
	case "after":
		iVal, err := strconv.Atoi(val)
		if err != nil {
			return options, err
		}
		ltx.AfterContext = iVal
	case "max":
		iVal, err := strconv.Atoi(val)
		if err != nil {
			return options, err
		}
		ltx.MaxCount = iVal
	default:
		options[key] = val
	}
	return options, nil
}
