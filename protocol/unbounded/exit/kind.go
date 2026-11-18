package exit

// Kind is the policy a session is served with, by the genesis it answers (see broflake's
// clientcore.ExitOptions). Each kind has its own exit box router, and a session only ever
// routes through its own kind's.
type Kind int

const (
	// Pool sessions may be strangers', and get the pool policy: lantern-cloud's residential
	// allowlist.
	Pool Kind = iota
	// Personal sessions answered a genesis sent to the volunteer's rooms alone, so they are
	// friends', and get the personal policy: no allowlist, but the same abuse rejects.
	Personal
)

// KindOf is the kind of a session broflake reports as personal or not.
func KindOf(personal bool) Kind {
	if personal {
		return Personal
	}
	return Pool
}

func (k Kind) String() string {
	if k == Personal {
		return "personal"
	}
	return "pool"
}
