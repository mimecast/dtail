package protocol

const (
	// ProtocolCompat -ibility version
	ProtocolCompat string = "4"
	// MessageDelimiter delimits separate messages.
	MessageDelimiter byte = '¬'
	// FieldDelimiter delimits aggregation fields.
	FieldDelimiter string = "|"
	// Arrow for multiple purposes
	Arrow string = "➔"
	// AggregateDelimiter delimits parts of an aggregation message.
	AggregateDelimiter string = Arrow
)
