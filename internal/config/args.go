package config

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/mimecast/dtail/internal/omode"

	gossh "golang.org/x/crypto/ssh"
)

// Args is a helper struct to summarize common client arguments.
type Args struct {
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

	sb.WriteString(fmt.Sprintf("%s:%s,", "LogDir", a.LogDir))
	sb.WriteString(fmt.Sprintf("%s:%s,", "Logger", a.Logger))
	sb.WriteString(fmt.Sprintf("%s:%s,", "LogLevel", a.LogLevel))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Arguments", a.Arguments))
	sb.WriteString(fmt.Sprintf("%s:%v,", "ConfigFile", a.ConfigFile))
	sb.WriteString(fmt.Sprintf("%s:%v,", "ConnectionsPerCPU", a.ConnectionsPerCPU))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Discovery", a.Discovery))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Mode", a.Mode))
	sb.WriteString(fmt.Sprintf("%s:%v,", "NoColor", a.NoColor))
	sb.WriteString(fmt.Sprintf("%s:%v,", "PrivateKeyPathFile", a.PrivateKeyPathFile))
	sb.WriteString(fmt.Sprintf("%s:%v,", "QueryStr", a.QueryStr))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Quiet", a.Quiet))
	sb.WriteString(fmt.Sprintf("%s:%v,", "RegexInvert", a.RegexInvert))
	sb.WriteString(fmt.Sprintf("%s:%v,", "RegexStr", a.RegexStr))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Serverless", a.Serverless))
	sb.WriteString(fmt.Sprintf("%s:%v,", "ServersStr", a.ServersStr))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Spartan", a.Spartan))
	sb.WriteString(fmt.Sprintf("%s:%v,", "SSHAuthMethods", a.SSHAuthMethods))
	sb.WriteString(fmt.Sprintf("%s:%v,", "SSHHostKeyCallback", a.SSHHostKeyCallback))
	sb.WriteString(fmt.Sprintf("%s:%v,", "SSHPort", a.SSHPort))
	sb.WriteString(fmt.Sprintf("%s:%v,", "Timeout", a.Timeout))
	sb.WriteString(fmt.Sprintf("%s:%v,", "TrustAllHosts", a.TrustAllHosts))
	sb.WriteString(fmt.Sprintf("%s:%v,", "UserName", a.UserName))
	sb.WriteString(fmt.Sprintf("%s:%v", "What", a.What))
	sb.WriteString(")")

	return sb.String()
}

// SerializeOptions returns a string ready to be sent over the wire to the server.
func (a *Args) SerializeOptions() string {
	return fmt.Sprintf("quiet=%v:spartan=%v:serverless=%v", a.Quiet, a.Spartan,
		a.Serverless)
}

// DeserializeOptions deserializes the options, but into a map.
func DeserializeOptions(opts []string) (map[string]string, error) {
	options := make(map[string]string, len(opts))
	for _, o := range opts {
		kv := strings.SplitN(o, "=", 2)
		if len(kv) != 2 {
			return options, fmt.Errorf("Unable to parse options: %v", kv)
		}
		key := kv[0]
		val := kv[1]

		if strings.HasPrefix(val, "base64%") {
			s := strings.SplitN(val, "%", 2)
			decoded, err := base64.StdEncoding.DecodeString(s[1])
			if err != nil {
				return options, err
			}
			val = string(decoded)
		}
		options[key] = val
	}
	return options, nil
}
