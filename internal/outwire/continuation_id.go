package outwire

// DownstreamResponseIDReporter is implemented by stream sinks whose downstream
// protocol exposes a response identifier the client may quote on the next turn.
// The value must be the identifier already written in the stream, not an
// upstream identifier that the client never saw.
type DownstreamResponseIDReporter interface {
	DownstreamResponseID() string
}
