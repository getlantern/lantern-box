// Package clientcontext provides [adapter.ConnectionTracker]s that sends and receives client
// metadata after connection handshake between the client and server. The metadata is stored in the
// context for other trackers to use.
//
// Usage:
// Create a [ClientContextInjector] on the client side to send client info to the server, and a
// [Manager] on the server side to receive and store the info. Both trackers should be added to the
// router using router.AppendTracker. Trackers added to the Manager with [Manager.AppendTracker] can
// access the client info from the connection context using [ClientInfoFromContext].
package clientcontext

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

const packetPrefix = "CLIENTINFO "

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
