package protocol

const (
	// ProtocolCompat is the protocol (compatibility) version client and
	// server send and compare. Protocol 5 frames every session in EscapedControlFraming from its
	// first frame on, without negotiation; the server rejects the commands
	// of a client of another version.
	ProtocolCompat string = "5"
	// MessageDelimiter terminates each message (frame) of the server-to-
	// client stream. It is the single byte 0xAC (the rune '¬' truncated to a
	// byte), which is also a UTF-8 continuation byte; every session escapes
	// it in frame bodies (see Framing).
	MessageDelimiter byte = 0xAC
	// fieldDelimiter delimits message fields.
	fieldDelimiter string = "|"
	// AggregateMessageID is the leading field of a mapreduce aggregate-data
	// message on the wire (AGGREGATE|hostname|serialized-data). It is the
	// single source of truth used by the server to tag such messages and by
	// the mapr client to recognise them, so protocol acks like "AUTHKEY OK"
	// (which merely start with the letter 'A') are not mistaken for data.
	AggregateMessageID string = "AGGREGATE"
	// CSVDelimiter delimits CSV file fields.
	CSVDelimiter string = ","
	// AggregateKVDelimiter delimits key-values of an aggregation message.
	AggregateKVDelimiter string = "≔"
	// AggregateDelimiter delimits parts of an aggregation message.
	AggregateDelimiter string = "∥"
	// AggregateGroupKeyCombinator combines the group set keys.
	AggregateGroupKeyCombinator string = ","
	// InputBatchBeginCommand opens an explicitly bounded batch of a plain
	// command stream (session.Spec.Commands).
	// InputBatchCompleteCommand closes it after every preceding FIFO frame has
	// reached server-side admission.
	InputBatchBeginCommand    string = ".ack input begin"
	InputBatchCompleteCommand string = ".ack input complete"

	// ServerlessInputCompleteCommand is retained as a source-compatible alias.
	ServerlessInputCompleteCommand string = InputBatchCompleteCommand
)
