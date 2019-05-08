package proto

const (
	// ProtoVersion is the latest version of the protocol. It is implicitly 0 for
	// clients that didn't yet have a protocol version
	ProtoVersion = 1
	// V0NotifyReady is the value sent at the end in the v0 protocol to indicate
	// readyness
	V0NotifyReady = 42

	// V1StartReadyHandshake is at the start of a v1 handshake
	V1StartReadyHandshake = 0x42

	// V1MessageSteppingDown is the message the old process sends in the handshake
	V1MessageSteppingDown = "stepping down"
)
