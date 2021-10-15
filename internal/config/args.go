package config

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

// Args is a helper struct to summarize common client arguments.
type Args struct {
	lcontext.LContext
	Arguments          []string
	ConfigFile         string
	ConnectionsPerCPU  int
	Discovery          string
	LogDir             string
	Logger             string
	LogLevel           string
	Mode               omode.Mode
	NoColor            bool
	PrivateKeyPathFile string
	QueryStr           string
	Quiet              bool
	RegexInvert        bool
	RegexStr           string
	Serverless         bool
	ServersStr         string
	Spartan            bool
	SSHAuthMethods     []gossh.AuthMethod
	SSHBindAddress     string
	SSHHostKeyCallback gossh.HostKeyCallback
	SSHPort            int
	Timeout            int
	TrustAllHosts      bool
	UserName           string
	What               string
}

func (a *Args) String() string {
	var sb strings.Builder

	sb.WriteString("Args(")

	sb.WriteString(fmt.Sprintf("%s:%v,", "Arguments", a.Arguments))
	sb.WriteString(fmt.Sprintf("%s:%v,", "ConfigFile", a.ConfigFile))
	sb.WriteString(fmt.Sprintf("%s:%v,", "ConnectionsPerCPU", a.ConnectionsPerCPU))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Discovery", a.Discovery))
	sb.WriteString(fmt.Sprintf("%s:%v,", "LogDir", a.LogDir))
	sb.WriteString(fmt.Sprintf("%s:%v,", "LogLevel", a.LogLevel))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Logger", a.Logger))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Mode", a.Mode))
	sb.WriteString(fmt.Sprintf("%s:%v,", "NoColor", a.NoColor))
	sb.WriteString(fmt.Sprintf("%s:%v,", "PrivateKeyPathFile", a.PrivateKeyPathFile))
	sb.WriteString(fmt.Sprintf("%s:%v,", "QueryStr", a.QueryStr))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Quiet", a.Quiet))
	sb.WriteString(fmt.Sprintf("%s:%v,", "RegexInvert", a.RegexInvert))
	sb.WriteString(fmt.Sprintf("%s:%v,", "RegexStr", a.RegexStr))
	sb.WriteString(fmt.Sprintf("%s:%v,", "SSHAuthMethods", a.SSHAuthMethods))
	sb.WriteString(fmt.Sprintf("%s:%v,", "SSHBindAddress", a.SSHBindAddress))
	sb.WriteString(fmt.Sprintf("%s:%v,", "SSHHostKeyCallback", a.SSHHostKeyCallback))
	sb.WriteString(fmt.Sprintf("%s:%v,", "SSHPort", a.SSHPort))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Serverless", a.Serverless))
	sb.WriteString(fmt.Sprintf("%s:%v,", "ServersStr", a.ServersStr))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Spartan", a.Spartan))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Timeout", a.Timeout))
	sb.WriteString(fmt.Sprintf("%s:%v,", "TrustAllHosts", a.TrustAllHosts))
	sb.WriteString(fmt.Sprintf("%s:%v,", "UserName", a.UserName))
	sb.WriteString(fmt.Sprintf("%s:%v", "What", a.What))
	sb.WriteString(")")

	return sb.String()
}

// SerializeOptions returns a string ready to be sent over the wire to the server.
func (a *Args) SerializeOptions() string {
	options := make(map[string]string)

	if a.Quiet {
		options["quiet"] = fmt.Sprintf("%v", a.Quiet)
	}
	if a.Spartan {
		options["spartan"] = fmt.Sprintf("%v", a.Spartan)
	}
	if a.Serverless {
		options["serverless"] = fmt.Sprintf("%v", a.Serverless)
	}
	if a.LContext.MaxCount != 0 {
		options["max"] = fmt.Sprintf("%d", a.LContext.MaxCount)
	}
	if a.LContext.BeforeContext != 0 {
		options["before"] = fmt.Sprintf("%d", a.LContext.BeforeContext)
	}
	if a.LContext.AfterContext != 0 {
		options["after"] = fmt.Sprintf("%d", a.LContext.AfterContext)
	}

	var sb strings.Builder
	var i int
	for k, v := range options {
		if i > 0 {
			sb.WriteString(":")
		}
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(v)
		i++
	}
	return sb.String()
}

// DeserializeOptions deserializes the options, but into a map.
func DeserializeOptions(opts []string) (map[string]string, lcontext.LContext, error) {
	options := make(map[string]string, len(opts))
	var ltx lcontext.LContext

	for _, o := range opts {
		kv := strings.SplitN(o, "=", 2)
		if len(kv) != 2 {
			return options, ltx, fmt.Errorf("Unable to parse options: %v", kv)
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

		switch key {
		case "before":
			iVal, err := strconv.Atoi(val)
			if err != nil {
				return options, ltx, err
			}
			ltx.BeforeContext = iVal
		case "after":
			iVal, err := strconv.Atoi(val)
			if err != nil {
				return options, ltx, err
			}
			ltx.AfterContext = iVal
		case "max":
			iVal, err := strconv.Atoi(val)
			if err != nil {
				return options, ltx, err
			}
			ltx.MaxCount = iVal
		default:
			options[key] = val
		}
	}

	return options, ltx, nil
}
