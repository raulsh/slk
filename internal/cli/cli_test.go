package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/raulsh/slk/internal/errdefs"
)

type result struct {
	code   int
	stdout string
	stderr string
}

// run executes the whole command tree against injected streams and reports
// what a caller would see on each one.
//
// Testing at this level is deliberate: flag wiring, format selection, write
// gating and exit codes are exactly what breaks, and every one of them is
// invisible to a unit test of any single function.
func run(t *testing.T, args ...string) result {
	t.Helper()

	var stdout, stderr strings.Builder
	code := Execute(context.Background(), Env{
		Args:   args,
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  strings.NewReader(""),
		// Nothing is a terminal in a test, which is also an agent's case.
		IsTTY:   func(*os.File) bool { return false },
		Version: "1.2.3",
		Commit:  "abc1234",
	})
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"version", []string{"version"}, errdefs.ExitOK},
		{"version json", []string{"version", "--json"}, errdefs.ExitOK},
		{"no command", nil, errdefs.ExitUsage},
		{"unknown command", []string{"bogus"}, errdefs.ExitUsage},
		{"unknown flag", []string{"version", "--nope"}, errdefs.ExitUsage},
		{"extra argument", []string{"version", "extra"}, errdefs.ExitUsage},
		{"json and jsonl", []string{"version", "--json", "--jsonl"}, errdefs.ExitUsage},
		{"bad log level", []string{"version", "--log-level", "shout"}, errdefs.ExitUsage},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, tc.args...)
			if got.code != tc.want {
				t.Errorf("Execute(%q) = %d, want %d (stderr: %s)",
					tc.args, got.code, tc.want, got.stderr)
			}
		})
	}
}

// TestNothingButResultsOnStdout is the invariant an agent depends on: it parses
// stdout, so a stray warning or error line there would corrupt the parse.
func TestNothingButResultsOnStdout(t *testing.T) {
	cases := [][]string{
		{"version", "--nope"},
		{"bogus"},
		{"version", "--log-level", "shout"},
	}

	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got := run(t, args...)
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty on failure", got.stdout)
			}
			if !strings.Contains(got.stderr, "error: ") {
				t.Errorf("stderr = %q, want an error: line", got.stderr)
			}
		})
	}
}

// TestJSONFailureIsParseable covers the case an agent hits when it gets the
// invocation wrong: it asked for JSON, so it must receive JSON to find out
// what went wrong, not prose it cannot read.
func TestJSONFailureIsParseable(t *testing.T) {
	got := run(t, "version", "--json", "--jsonl")

	if got.code != errdefs.ExitUsage {
		t.Fatalf("exit = %d, want %d", got.code, errdefs.ExitUsage)
	}

	var env struct {
		OK      bool `json:"ok"`
		Schema  int  `json:"schema"`
		Results []any
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &env); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, got.stdout)
	}
	if env.OK {
		t.Error("ok = true, want false")
	}
	if env.Error.Code != string(errdefs.CodeUsage) {
		t.Errorf("error.code = %q, want %q", env.Error.Code, errdefs.CodeUsage)
	}
	// An absent results array would force every consumer into a nil check.
	if env.Results == nil {
		t.Error("results = null, want []")
	}
	// The human line must still be there for whoever is watching the terminal.
	if !strings.Contains(got.stderr, "error: ") {
		t.Errorf("stderr = %q, want an error: line alongside the JSON", got.stderr)
	}
}

func TestVersionFormats(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		got := run(t, "version")
		if !strings.HasPrefix(got.stdout, "slk 1.2.3 (commit abc1234, go") {
			t.Errorf("stdout = %q, want the version line", got.stdout)
		}
	})

	t.Run("short", func(t *testing.T) {
		got := run(t, "version", "--short")
		if got.stdout != "1.2.3\n" {
			t.Errorf("stdout = %q, want %q", got.stdout, "1.2.3\n")
		}
	})

	t.Run("json envelope", func(t *testing.T) {
		got := run(t, "version", "--json")
		var env struct {
			OK      bool   `json:"ok"`
			Command string `json:"command"`
			Count   int    `json:"count"`
			Results []struct {
				Version string `json:"version"`
				Commit  string `json:"commit"`
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(got.stdout), &env); err != nil {
			t.Fatalf("stdout is not JSON: %v (%q)", err, got.stdout)
		}
		if !env.OK || env.Command != "version" || env.Count != 1 {
			t.Errorf("envelope = %+v, want ok/version/1", env)
		}
		if len(env.Results) != 1 || env.Results[0].Version != "1.2.3" {
			t.Errorf("results = %+v, want one record for 1.2.3", env.Results)
		}
	})

	t.Run("jsonl is one bare record per line", func(t *testing.T) {
		got := run(t, "version", "--jsonl")
		lines := strings.Split(strings.TrimSpace(got.stdout), "\n")
		if len(lines) != 1 {
			t.Fatalf("got %d lines, want 1: %q", len(lines), got.stdout)
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
			t.Fatalf("line is not JSON: %v", err)
		}
		// JSONL emits the record itself, never the envelope, so a consumer can
		// treat every line the same way.
		if _, wrapped := rec["ok"]; wrapped {
			t.Errorf("jsonl record is enveloped: %v", rec)
		}
		if rec["version"] != "1.2.3" {
			t.Errorf("version = %v, want 1.2.3", rec["version"])
		}
	})
}

// TestRawImpliesMachineFormat pins the rule that --raw resolves to a machine
// format. Stated as "--raw implies --json" it would make a streaming command
// self-destruct against the --json rejection.
func TestRawImpliesMachineFormat(t *testing.T) {
	got := run(t, "version", "--raw")
	if got.code != errdefs.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(got.stdout), "{") {
		t.Errorf("stdout = %q, want JSON", got.stdout)
	}
}

// TestUsageFollowsTheErrorLine keeps the reason readable: the usage block is
// long enough to scroll the reason away if it comes first.
func TestUsageFollowsTheErrorLine(t *testing.T) {
	got := run(t, "version", "--nope")

	errIdx := strings.Index(got.stderr, "error: ")
	useIdx := strings.Index(got.stderr, "Usage:")
	if errIdx < 0 || useIdx < 0 {
		t.Fatalf("stderr = %q, want both an error line and usage", got.stderr)
	}
	if errIdx > useIdx {
		t.Errorf("usage precedes the error line in %q", got.stderr)
	}
	// The sentinel's own text must not leak into the message.
	if strings.Contains(got.stderr, "invalid usage:") {
		t.Errorf("stderr leaks the sentinel text: %q", got.stderr)
	}
}
