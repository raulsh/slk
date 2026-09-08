package config

import (
	"encoding/json"
	"log/slog"
	"strings"
)

// Secret is a credential string that refuses to print itself.
//
// The xoxc token and xoxd cookie together are full access to a human's Slack
// account. Leaking one into a log line, a --log-level debug dump, an error
// message or a panic hands the account over. Every path that could stringify a
// credential goes through this type, so redaction is the default and revealing
// is the explicit, greppable exception.
//
// There is deliberately no UnmarshalYAML or UnmarshalJSON override: the file
// contains a plain string, and reading it must not be special.
type Secret string

// Reveal returns the credential in the clear.
//
// Every call site is a deliberate decision: `grep -rn 'Reveal()'` enumerates
// every way a credential can leave this process. There are exactly three -
// building the HTTP credentials, writing the config file, and
// `config show --reveal`.
func (s Secret) Reveal() string { return string(s) }

// Empty reports whether the credential is unset, without revealing it.
func (s Secret) Empty() bool { return s == "" }

// String satisfies fmt.Stringer, so %v and %s redact.
func (s Secret) String() string { return Redact(string(s)) }

// GoString satisfies fmt.GoStringer, so %#v redacts too. Without it, a
// struct dump with %+v on a containing pointer would print the raw value.
func (s Secret) GoString() string { return `"` + Redact(string(s)) + `"` }

// MarshalJSON redacts, so a credential can never reach --json output.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(Redact(string(s))) }

// MarshalYAML redacts as well.
//
// That makes marshalling a Config always safe, which is the property worth
// having - so writing the real file deliberately bypasses this type through
// the private wire structs in store.go rather than trusting a caller to
// remember which direction they are going.
func (s Secret) MarshalYAML() (any, error) { return Redact(string(s)), nil }

// LogValue satisfies slog.LogValuer, so a credential passed as a structured
// log attribute redacts rather than landing in the journal.
func (s Secret) LogValue() slog.Value { return slog.StringValue(Redact(string(s))) }

// Redact keeps just enough to tell two credentials apart in a bug report: the
// prefix up to the first dash, an ellipsis, and the last four bytes.
//
//	xoxc-…a1b2
//
// A short or prefix-less value is reported only by length, because there is no
// safe way to show part of something we cannot recognise.
func Redact(v string) string {
	if v == "" {
		return ""
	}
	prefix := ""
	if i := strings.Index(v, "-"); i > 0 && i < 8 {
		prefix = v[:i+1]
	}
	rest := v[len(prefix):]
	if len(rest) < 8 {
		return prefix + "…"
	}
	return prefix + "…" + rest[len(rest)-4:]
}
