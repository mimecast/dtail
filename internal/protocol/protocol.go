package protocol

const (
	// ProtocolCompat -ibility version
	ProtocolCompat string = "4.1"
	// MessageDelimiter delimits separate messages.
	MessageDelimiter byte = '¬'
	// FieldDelimiter delimits messagefields.
	FieldDelimiter string = "|"
	// CSVDelimiter delimits CSV file fields.kj:w
	CSVDelimiter string = ","
	// AggregateKVDelimiter delimits key-values of an aggregation message.
	AggregateKVDelimiter string = "≔"
	// AggregateDelimiter delimits parts of an aggregation message.
	AggregateDelimiter string = "∥"
	// AggregateGroupKeyCombinator combines the group set keys.
	AggregateGroupKeyCombinator string = ","
)
