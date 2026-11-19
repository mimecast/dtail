package protocol

const (
	// HiddenCapabilitiesPrefix identifies server capability advertisements
	// sent over the hidden control channel. Every protocol 5 client and
	// mapreduce session starts with this marked control frame, also when it
	// lists no capability at all. Capabilities name what a server can do
	// beyond the protocol 5 baseline; everything every protocol 5 peer
	// supports (SESSION START/UPDATE, failed-command reports, encoded read
	// paths, escaped control framing) is not advertised.
	HiddenCapabilitiesPrefix = ".syn capabilities "

	// CapabilityJournalV1 marks support for journal read targets
	// ("journal:unit.service"). It is a runtime property of the server
	// (Linux with journalctl on PATH), not of the protocol version, so it is
	// advertised and checked by clients.
	CapabilityJournalV1 = "journal-v1"
)
