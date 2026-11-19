package clients

import (
	"github.com/mimecast/dtail/internal/config"
	sessionspec "github.com/mimecast/dtail/internal/session"
)

// SessionSpec captures the mutable, per-connection workload a DTail client wants to run.
type SessionSpec = sessionspec.Spec

// NewSessionSpec returns a session specification from client args.
func NewSessionSpec(args config.Args) SessionSpec {
	return sessionspec.NewSpec(sessionspec.Parameters{
		Mode:              args.Mode,
		What:              args.What,
		SerializedOptions: args.SerializeOptions(),
		Query:             args.QueryStr,
		Regex:             args.RegexStr,
		RegexInvert:       args.RegexInvert,
		Timeout:           args.Timeout,
		Serverless:        args.Serverless,
	})
}
