package proto

// VersionInformation communicates the protocol version this process supports.
// Added in v1
type VersionInformation struct {
	Version int32 `json:"version"`
}

type Message struct {
	Msg string `json:"msg"`
}
