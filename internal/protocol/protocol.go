package protocol

const (
	// ProtocolCompat -ibility version
	ProtocolCompat string = "4.1"
	// MessageDelimiter delimits separate messages.
	MessageDelimiter byte = '¬'
	// FieldDelimiter delimits messagefields.
	FieldDelimiter string = "|"
	// AggregateMessageID is the leading field of a mapreduce aggregate-data
	// message on the wire (AGGREGATE|hostname|serialized-data). It is the
	// single source of truth used by the server to tag such messages and by
	// the mapr client to recognise them, so protocol acks like "AUTHKEY OK"
	// (which merely start with the letter 'A') are not mistaken for data.
	AggregateMessageID string = "AGGREGATE"
	// CSVDelimiter delimits CSV file fields.kj:w
	CSVDelimiter string = ","
	// AggregateKVDelimiter delimits key-values of an aggregation message.
	AggregateKVDelimiter string = "≔"
	// AggregateDelimiter delimits parts of an aggregation message.
	AggregateDelimiter string = "∥"
	// AggregateGroupKeyCombinator combines the group set keys.
	AggregateGroupKeyCombinator string = ","
	// InputBatchBeginCommand opens an explicitly bounded legacy command batch.
	// InputBatchCompleteCommand closes it after every preceding FIFO frame has
	// reached server-side admission.
	InputBatchBeginCommand    string = ".ack input begin"
	InputBatchCompleteCommand string = ".ack input complete"

	// ServerlessInputCompleteCommand is retained as a source-compatible alias.
	ServerlessInputCompleteCommand string = InputBatchCompleteCommand
)
