package protocol

const (
	// HiddenCapabilitiesPrefix identifies server capability advertisements sent over the hidden control channel.
	HiddenCapabilitiesPrefix = ".syn capabilities "

	// CapabilityQueryUpdateV1 marks support for in-flight query replacement over an existing session.
	CapabilityQueryUpdateV1 = "query-update-v1"

	// CapabilityJournalV1 marks support for journal protocol operations over an existing session.
	CapabilityJournalV1 = "journal-v1"

	// CapabilityCommandFailureV1 marks that the server reports failed session
	// commands with a hidden HiddenCommandFailedPrefix message.
	CapabilityCommandFailureV1 = "command-failure-v1"

	// CapabilityEscapedFramingV1 marks that the server switches a session to
	// EscapedFraming when the client sends EscapedFramingCommand as the
	// session's first command. The capabilities frame is the first frame of
	// a session and is always sent in RawFraming; a client that sent
	// EscapedFramingCommand decodes every later frame as escaped when the
	// session's first frame is a capabilities frame listing this capability,
	// and never decides on a later frame.
	CapabilityEscapedFramingV1 = "escaped-framing-v1"

	// CapabilityEncodedReadPathV1 marks that the server decodes read command
	// paths encoded with EncodeReadPath (see EncodedReadPathPrefix), so it can
	// read paths that contain a space. Clients refuse to send such a path to
	// a server that does not advertise it: an older server would split the
	// path at its spaces, or look for a file named after the encoded word.
	CapabilityEncodedReadPathV1 = "encoded-read-path-v1"

	// CapabilityControlFramesV1 marks that the server switches an escaped
	// session to EscapedControlFraming when the client sends
	// ControlFramesCommand as the session's second command, right after an
	// EscapedFramingCommand that switched it to EscapedFraming. A client
	// that sent both requests decodes every frame after the capabilities
	// frame as EscapedControlFraming when the session's first frame lists
	// both capabilities.
	CapabilityControlFramesV1 = "control-frames-v1"
)

// EscapedFramingCommand asks the server to escape every frame it sends after
// its capabilities frame (see Framing). Clients send it as the very first
// command of a session, before any command that produces output, and without
// waiting for the capabilities; a server ignores it as a later command.
// Servers that do not advertise CapabilityEscapedFramingV1 ignore it like any
// other ".ack" they do not know and keep RawFraming, and older clients never
// send it, so their sessions stay on RawFraming too.
const EscapedFramingCommand = ".ack framing escaped-v1"

// ControlFramesCommand asks the server to mark its control frames (see
// EscapedControlFraming), so that no --plain log line can be taken for one.
// Clients send it as the session's second command, right after
// EscapedFramingCommand and in the same write, without waiting for the
// capabilities. Servers ignore it unless they advertise
// CapabilityControlFramesV1, the session switched to EscapedFraming with its
// first command and this is its second command; servers that do not know it
// ignore it like any other ".ack" they do not know. Either way the session
// then keeps the framing it has, and so does a client that does not find
// CapabilityControlFramesV1 in the session's first frame.
const ControlFramesCommand = ".ack framing control-v1"
