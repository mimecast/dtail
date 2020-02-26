package clients

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/omode"
)

// RunClient is a client to run various commands on the server.
type RunClient struct {
	baseClient
	jobName    string
	background string
}

// NewRunClient returns a new run client to execute commands on the remote server.
func NewRunClient(args Args, background, jobName string) (*RunClient, error) {
	args.Mode = omode.RunClient

	if jobName == "" {
		jobName = hash(strings.Join(args.Arguments, " "))
	}

	c := RunClient{
		baseClient: baseClient{
			Args:       args,
			throttleCh: make(chan struct{}, args.ConnectionsPerCPU*runtime.NumCPU()),
			retry:      false,
		},
		jobName:    jobName,
		background: background,
	}

	c.init(c)
	return &c, nil
}

func (c RunClient) makeHandler(server string) handlers.Handler {
	return handlers.NewClientHandler(server)
}

func (c RunClient) makeCommands() (commands []string) {
	if c.Timeout > 0 {
		commands = append(commands, fmt.Sprintf("timeout %d run%s %s", c.Timeout, c.options(), c.What))
		return
	}

	commands = append(commands, fmt.Sprintf("run%s %s", c.options(), c.What))
	logger.Debug(commands)

	return
}

func (c RunClient) options() string {
	var sb strings.Builder

	logger.Debug("options", fmt.Sprintf(":background=%s", c.background))
	sb.WriteString(fmt.Sprintf(":background=%s", c.background))

	logger.Debug("options", fmt.Sprintf(":jobName=%s", c.jobName))
	sb.WriteString(fmt.Sprintf(":jobName=%s", c.jobName))

	if len(c.Arguments) > 0 {
		logger.Debug("options", fmt.Sprintf(":outerArgs=base64%%%s", strings.Join(c.Arguments, " ")))
		sb.WriteString(fmt.Sprintf(":outerArgs=base64%%%s", encode64(strings.Join(c.Arguments, " "))))
	}

	return sb.String()
}

func encode64(str string) string {
	return base64.StdEncoding.EncodeToString([]byte(str))
}

func hash(str string) string {
	h := sha256.New()
	h.Write([]byte(str))

	return hex.EncodeToString(h.Sum(nil))
}
