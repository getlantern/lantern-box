// Package clientcontext provides [adapter.ConnectionTracker] implementations
// that exchange client metadata after the connection handshake. Manager exposes
// decoded metadata on the wrapped connection for downstream trackers to read
// with [InfoFromConn].
//
// Usage:
// Register [ClientContextInjector] on the client router and [Manager] on the
// server router. Trackers registered after Manager can recover client info with
// [InfoFromConn].
package clientcontext

import "slices"

// since sing-box only wraps inbound connections with trackers, conn on the client is from the user
// (e.g. tun connection), while conn on the server is from an outbound on the client. The connection
// to the server isn't established until after conn is wrapped on the client side and we don't have
// access to it until after the handshake.
//
//                     Client                         Server
//                  -------------                 -------------
//    conn    --->  tracker(conn)                       |
// (i.e. tun)            |                              |
//                   dial server   ----------->       conn
//                       |                              |
//                       +<--------  handshake  ------->+
//                       |                              |
//                handshakeSuccess   <----------   tracker(conn)
//                       |                              |
//                send client info   --------->  read client info
//                       |                             |
//                  pipe traffic                 dial upstream
//                                                    ...
//                                                pipe traffic
//
// This is why writeConn (client) doesn't send the client info until ConnHandshakeSuccess while
// readConn (server) reads it immediately upon creation.

const (
	// legacyPacketPrefix marks a client-info frame from a client that waits for
	// ackResponse.
	legacyPacketPrefix = "CLIENTINFO "

	// packetPrefix marks a client-info frame from a client that does not wait
	// for a reply.
	packetPrefix = "CLIENTINFO2 "

	// ackResponse is sent only to legacyPacketPrefix clients.
	ackResponse = "OK"
)

type GetClientInfoFn func() ClientInfo

// ClientInfo holds information about the client user/device.
type ClientInfo struct {
	DeviceID    string
	Platform    string
	IsPro       bool
	CountryCode string
	Version     string
}

// MatchBounds specifies inbound and outbound matching rules.
// The empty string and "any" are treated as a wildcard.
type MatchBounds struct {
	Inbound  []string
	Outbound []string
}

func (mb MatchBounds) clone() MatchBounds {
	return MatchBounds{
		Inbound:  slices.Clone(mb.Inbound),
		Outbound: slices.Clone(mb.Outbound),
	}
}

type boundsRule struct {
	tags     []string
	tagMap   map[string]bool
	matchAny bool
}

func newBoundsRule(tags []string) *boundsRule {
	br := &boundsRule{tags: tags, tagMap: make(map[string]bool)}
	if len(tags) == 1 && (tags[0] == "" || tags[0] == "any") {
		br.matchAny = true
		return br
	}
	for _, tag := range tags {
		br.tagMap[tag] = true
	}
	return br
}

func (b *boundsRule) match(tag string) bool {
	return (b.matchAny && tag != "") || b.tagMap[tag]
}
