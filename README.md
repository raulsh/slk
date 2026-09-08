# slk

**Read your Slack from the command line, as you, so an agent can shell out to it.**

Every Slack integration assumes you want to build an app: create a bot, request scopes, wait for an
admin to approve it. That is the wrong shape for "let my agent read my messages". A bot is a
different identity with different visibility — it cannot see your DMs, it cannot see the private
channels you are in, it cannot search, and it has no concept of *unread*, because unread is a fact
about a person. `slk` borrows the credentials your own browser is already using, so it sees exactly
what you see, needs nobody's approval, and reads your DMs because they are your DMs.

The output is shaped for a reader that pays per token: compact records with names already resolved,
one JSON envelope that answers the follow-up questions before they are asked, and nothing on stdout
but results.

The approach is the one [libslackrtm](https://github.com/InterLinked1/slack-rtm) takes — a workspace
`xoxc` token paired with the `xoxd` browser cookie.

## Being straight about what this is

This uses Slack's internal web-client API, not the documented public one. Three things follow, and
none of them are worked around by pretending otherwise:

- **It is undocumented and unsupported.** Endpoints can change without notice. Everything here that
  depends on one has a named fallback and degrades loudly rather than silently.
- **The credentials are your whole account.** Anyone holding your token and cookie is you. The
  config file is `0600` and `slk` refuses to read it if it is looser.
- **They expire routinely**, when your browser session rotates. That is treated as a normal event,
  not an exception: `slk` re-mints the token from the cookie when it can, and when it cannot it exits
  with a distinct code and tells you exactly what to re-copy.

Whether you may use it on a given workspace is between you and that workspace's policy.

## Install

A `.deb` for Debian and Ubuntu, from the
[latest release](https://github.com/raulsh/slk/releases/latest):

```sh
sudo dpkg -i slk_*_amd64.deb        # or _arm64
```

Or a static binary, from the same place — linux and darwin, amd64 and arm64.
Or from source:

```sh
go install github.com/raulsh/slk/cmd/slk@latest
```

Or build it:

```sh
make build          # bin/slk
make install        # into ~/.local/bin
make deb            # dist/slk_<version>_<arch>.deb
```

The package installs the binary plus bash, zsh and fish completions, and
declares no dependencies — the binary is fully static.

## Reading your Slack

```sh
slk configure --from-curl -          # paste a DevTools "Copy as cURL", Ctrl-D
slk auth test                        # confirm it works and say who you are

slk unread                           # where should I look?
slk messages --all --unread          # what did I miss?
slk messages --all --unread --vip    # ...from people I actually care about

slk conversations list --unread-only
slk messages --channel eng --since 2d
slk messages --dm @alice --since 7d
slk messages --channel eng --on 2026-09-01
slk thread https://acme.slack.com/archives/C0123/p1725806400123456
slk search "deploy failed" --in '#eng' --since 7d
slk users list --vip

slk browse                           # interactive, when you'd rather look around
```

`unread` and `messages --unread` answer different questions and both are worth
asking: the first is a one-screen summary of where to look, the second is the
content. Reading never marks anything read.

**VIPs.** If you have marked people VIP in Slack, `slk` reads that: `--vip` on
`messages` keeps only their messages, `users list --vip` lists them without
enumerating the directory, and a `★` marks them in the human view and a `"vip":
true` in JSON. It is your own classification, which makes it the best triage
signal available.

### Time filters

`--since`, `--until` and `--on` all take the same forms:

| Form | Means |
|---|---|
| `7d`, `2w`, `3h`, `90m`, `1h30m` | that long ago |
| `today`, `yesterday`, `now` | |
| `mon` … `sun` | the most recent such weekday |
| `2026-09-01` | that day — 00:00 for `--since`, end of day for `--until` |
| `2026-09-01T14:30`, `…T14:30:00Z` | that instant |
| `@1725806400` | unix seconds |

The window is half-open, so `--since D --until D+1` is exactly day D and chained
windows neither drop nor duplicate. `--on D` is shorthand for a whole day.
`1m` is a minute, not a month.

Volume is bounded by two separate flags, because conflating them is a real
footgun: `--limit` is **per conversation**, `--max-total` caps the **whole run**.
Selection always takes the most recent messages in the range; `--oldest-first`
controls print order only.

## Interactive mode

`slk browse` opens a terminal UI: conversations needing attention sort to the
top (mentions, then unread, then recency), `enter` opens one, `esc` goes back,
`/` filters, `u` narrows to unread and `v` to VIP DMs, `r` reloads, `?` expands
the help.

It is the one command not meant for an agent — it needs a terminal and never
terminates on its own — so it refuses to start without one and points at the
equivalent data commands. Everything it shows is available as JSON.

## Not built yet

`slk tail` (live RTM stream), `slk send` and `slk mark`.

## Output for agents

Three shapes, one global switch. **Stdout is the data channel, unconditionally** — logs, warnings and
human error prose go to stderr, always, so `slk … --json | jq` is safe.

| Flag | Shape |
|---|---|
| *(default)* | aligned text for a person |
| `--json` | one self-describing envelope covering a complete result set |
| `--jsonl` | one record per line, for streaming or capping with `head` |
| `--raw` | adds Slack's untouched payload; implies the machine format |

The envelope carries `ok`, `schema`, `command`, `count`, `results`, and — when relevant — a `names`
map resolving only the ids that appear in `results`, a `page` cursor, the resolved `window` for a
time-filtered query, and `warnings`. On failure it carries `error` instead, with `results` still an
empty array so no consumer needs a nil check.

`schema` is the contract version. It bumps only when a field is removed or changes meaning.

## Exit codes

An agent has to branch on failure without parsing prose.

| Code | Meaning | What to do |
|---|---|---|
| 0 | success | — |
| 1 | unclassified failure | report it |
| 2 | bad flag, argument or combination | fix the invocation |
| 3 | credentials rejected — the token or cookie expired | a human must re-run `slk configure` |
| 4 | rate-limit budget exhausted | retry after `retry_after` |
| 5 | unknown or invisible channel, user or message | try another target |
| 6 | network failure or `--timeout` | retry with backoff |
| 7 | nothing configured, or the config is unreadable | a human must set it up |
| 8 | a write was refused | it needs explicit opt-in |
| 9 | partial results returned, run ended early | use them, or resume from the cursor |
| 130 | interrupted | — |

3 and 7 are both "a human must act", but the remedies differ — a stale credential versus nothing
configured at all — and that is the distinction worth branching on.

## Development

```sh
make build     # static binary into bin/
make check     # gofmt gate, vet, golangci-lint, tests - what CI runs
make test
make dist      # cross-compiled release archives
make deb       # Debian package
make help      # every target
```

CI runs `check` and, on every push, builds the `.deb` and asserts the binary
inside it is statically linked — packaging breaks quietly, and finding out at
release time is finding out too late. Pushing a `v*` tag builds the archives
and packages for linux and darwin on amd64 and arm64 and publishes them as a
release.

The package map, with the reason each boundary exists:

| Package | Why it is separate |
|---|---|
| `internal/cli` | cobra's flag plumbing is an adapter; nothing else imports it |
| `internal/config` | one owner for the credential file's shape, permissions and redaction |
| `internal/slack` | all knowledge of Slack's private protocol, behind one interface |
| `internal/output` | the JSON field names are a public API; one directory to review |
| `internal/errdefs` | classification both the producers and the renderer need, without a cycle |
| `internal/slacktime` | Slack `ts` ↔ day-based input, so timezone rules are not reinvented |
| `internal/resolve` | target grammar, ambiguity rules, and the on-disk name cache |
| `internal/tui` | the interactive browser; keeps the event loop out of the batch commands |

`cli.Execute(ctx, Env) int` takes its streams and its TTY check as injected fields and never calls
`os.Exit`, which is what makes flag wiring, format selection, write gating and exit codes testable
in-process. `cmd/slk/main.go` is the only thing that exits.

## License

[0BSD](LICENSE). Do what you like with it.

## Notes on Slack's web client API

Behaviours this depends on, verified rather than assumed. *(Populated as each is confirmed against a
real workspace — see the plan's spike list.)*

- The token and the cookie only work together; an `xoxc` token alone returns `not_authed`.
- The `xoxd` cookie value is URL-encoded as the browser stores it, and re-encoding it corrupts it.
- Token expiry is indistinguishable from a wrong token, so both get the same remediation.
- `results` in a JSON envelope is always an array, including on failure.
