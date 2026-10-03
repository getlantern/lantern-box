// Package clientcontext sends client metadata from a client to its server at the start of each
// proxied connection, and exposes it to the server's connection trackers.
//
// On the client, an [Injector] installed into the box context wraps the outbounds sing-box
// creates, so every dial through an outbound enabled for injection sends the metadata, whether it
// comes from the router or from a direct caller such as a DNS detour or a probe. On the server, a
// [Manager] added with router.AppendTracker reads it. Trackers added to the router after the
// Manager can read it from the connection with [InfoFromConn].
package clientcontext

const (
	// clientInfoPrefix marks a client-info frame whose sender does not wait for
	// a reply. On TCP the frame may be followed immediately by the flow's data.
	clientInfoPrefix = "CLIENTINFO2 "

	// legacyClientInfoPrefix marks a client-info frame whose sender waits for
	// ackResponse before sending anything else.
	legacyClientInfoPrefix = "CLIENTINFO "

	// ackResponse is sent only to legacyClientInfoPrefix senders.
	ackResponse = "OK"
)

// GetClientInfoFn returns the ClientInfo to send on a new connection.
type GetClientInfoFn func() ClientInfo

// ClientInfo holds information about the client user/device. The zero
// ClientInfo identifies no client: probes send it, and [InfoFromConn] reports
// no info for a connection that carries it.
type ClientInfo struct {
	DeviceID    string
	Platform    string
	IsPro       bool
	CountryCode string
	Version     string
}
