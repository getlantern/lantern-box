package option

import "github.com/sagernet/sing/common/json/badoption"

// DNSPoolServerOptions configure a pool DNS server: it answers from whichever
// of Servers answers first, trying them in order with Stagger between starts.
// Each entry is the tag of another DNS server in the same config, so any
// server type, and any TLS tactic it supports, can be part of the pool.
type DNSPoolServerOptions struct {
	Servers []string `json:"servers"`
	// Stagger is how long one server gets before the next is also tried.
	// Empty or zero uses 250ms.
	Stagger badoption.Duration `json:"stagger,omitempty"`
	// Timeout bounds each server's exchange. Empty or zero uses 5s.
	Timeout badoption.Duration `json:"timeout,omitempty"`
}
