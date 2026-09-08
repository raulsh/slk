package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a configuration duration that accepts either a Go duration
// string ("15m", "90s", "0s") or a plain number of seconds.
//
// time.Duration alone cannot be unmarshalled from YAML at all: a bare `0`
// fails, and so does `900`. Requiring a unit everywhere is a reasonable
// house style, but a config file people hand-edit should not reject the most
// obvious way to write "no delay".
type Duration time.Duration

// Std returns the wrapped standard duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML accepts a duration string or a number of seconds.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	switch node.Tag {
	case "!!str":
		parsed, err := time.ParseDuration(node.Value)
		if err != nil {
			return fmt.Errorf("line %d: %q is not a duration; write it like 30s, 15m or 2h",
				node.Line, node.Value)
		}
		*d = Duration(parsed)
		return nil

	case "!!int", "!!float":
		// A bare number is read as seconds, which is what someone writing
		// `misfire_grace: 0` or `check_timeout: 90` means.
		var seconds float64
		if err := node.Decode(&seconds); err != nil {
			return fmt.Errorf("line %d: %q is not a number of seconds", node.Line, node.Value)
		}
		*d = Duration(time.Duration(seconds * float64(time.Second)))
		return nil

	case "!!null":
		// A key written with no value leaves the default in place. yaml.v3
		// does not even call this for a bare `key:`, but an explicit `~`
		// reaches here, and it should behave the same way.
		return nil

	default:
		return fmt.Errorf("line %d: expected a duration such as 15m, or a number of seconds, got %s",
			node.Line, node.Tag)
	}
}

// MarshalYAML writes the readable string form.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
