package outwire

// DownstreamResponseIDReporter is implemented by stream sinks whose downstream
// protocol exposes a response identifier the client may quote on the next turn.
// The value must be the identifier already written in the stream, not an
// upstream identifier that the client never saw.
type DownstreamResponseIDReporter interface {
	DownstreamResponseID() string
}

// UpstreamResponseIDReporter narrows DownstreamResponseIDReporter to the case
// where the client-visible id is usable as an upstream continuation key.
// A gateway-generated id is valid for the client but unknown to the upstream,
// so it must not create a binding.
type UpstreamResponseIDReporter interface {
	DownstreamResponseIDReporter
	DownstreamResponseIDFromUpstream() string
}
