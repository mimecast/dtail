package protocol

const (
	// HiddenCapabilitiesPrefix identifies server capability advertisements sent over the hidden control channel.
	HiddenCapabilitiesPrefix = ".syn capabilities "

	// CapabilityQueryUpdateV1 marks support for in-flight query replacement over an existing session.
	CapabilityQueryUpdateV1 = "query-update-v1"

	// CapabilityJournalV1 marks support for journal protocol operations over an existing session.
	CapabilityJournalV1 = "journal-v1"
)
