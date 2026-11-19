package protocol

const (
	// HiddenSessionStartOKPrefix acknowledges a successful SESSION START request.
	HiddenSessionStartOKPrefix = ".syn session start ok"
	// HiddenSessionUpdateOKPrefix acknowledges a successful SESSION UPDATE request.
	HiddenSessionUpdateOKPrefix = ".syn session update ok"
	// HiddenSessionErrorPrefix reports a rejected SESSION request.
	HiddenSessionErrorPrefix = ".syn session err "
)
