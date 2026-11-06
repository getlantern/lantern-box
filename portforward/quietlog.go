package portforward

import (
	"log/slog"
	"regexp"
	"strings"
)

// Option configures NewForwarder and ProbeUPnP.
type Option func(*options)

type options struct {
	quiet bool
}

// QuietLogs has the Forwarder, and the discovery that builds it, log without
// addresses or ports. See (*Forwarder).SetQuietLogs.
func QuietLogs() Option {
	return func(o *options) { o.quiet = true }
}

func applyOptions(opts []Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// SetQuietLogs turns quiet logging on or off. When it is on, every log line
// keeps its message and the kind of any error, but drops the mapped ports and
// the LAN address, and masks any address or number inside error text (such
// as a gateway's IP or a port in a router's refusal). It is off by default.
// To also quiet the discovery that finds the gateway, pass QuietLogs to
// NewForwarder.
func (f *Forwarder) SetQuietLogs(quiet bool) {
	f.quiet.Store(quiet)
}

func (f *Forwarder) logInfo(msg string, args ...any) {
	slog.Info(msg, redactLogArgs(f.quiet.Load(), args)...)
}

func (f *Forwarder) logWarn(msg string, args ...any) {
	slog.Warn(msg, redactLogArgs(f.quiet.Load(), args)...)
}

func logInfo(quiet bool, msg string, args ...any) {
	slog.Info(msg, redactLogArgs(quiet, args)...)
}

func logWarn(quiet bool, msg string, args ...any) {
	slog.Warn(msg, redactLogArgs(quiet, args)...)
}

// quietDropKeys are log keys whose values are themselves ports or addresses.
var quietDropKeys = map[string]bool{
	"external_port": true,
	"internal_port": true,
	"internal_ip":   true,
}

var (
	ipv4RE   = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	ipv6RE   = regexp.MustCompile(`(?i)\[?\b(?:[0-9a-f]{0,4}:){2,7}[0-9a-f]{0,4}\b\]?`)
	numberRE = regexp.MustCompile(`\d+`)
)

// redactText masks addresses, then every remaining number, which takes ports
// with it, so an error keeps its wording but names no endpoint.
func redactText(s string) string {
	s = ipv4RE.ReplaceAllString(s, "<ip>")
	s = ipv6RE.ReplaceAllString(s, "<ip>")
	return numberRE.ReplaceAllString(s, "<n>")
}

// redactLogArgs returns args unchanged unless quiet. Quiet, it drops the
// pairs in quietDropKeys and masks addresses and numbers in error values,
// including the string-valued "*_err" fields discovery logs. args are
// slog's alternating keys and values, possibly mixed with slog.Attrs.
func redactLogArgs(quiet bool, args []any) []any {
	if !quiet {
		return args
	}
	out := make([]any, 0, len(args))
	for i := 0; i < len(args); i++ {
		key, ok := args[i].(string)
		if !ok || i+1 >= len(args) {
			out = append(out, args[i])
			continue
		}
		val := args[i+1]
		i++
		if quietDropKeys[key] {
			continue
		}
		switch v := val.(type) {
		case error:
			val = redactText(v.Error())
		case string:
			if key == "err" || strings.HasSuffix(key, "_err") {
				val = redactText(v)
			}
		}
		out = append(out, key, val)
	}
	return out
}
