# Installing and configuring slk

Read this when `slk` is not on PATH, or a call exits 7 (nothing configured) or 3 (credentials
expired).

## Installing the binary

Ask the user before installing. Any one of these works; the binary is static and has no runtime
dependencies.

```sh
go install github.com/raulsh/slk/cmd/slk@latest     # with a Go toolchain
```

- A release archive from <https://github.com/raulsh/slk/releases/latest>:
  `slk_<version>_<os>_<arch>.tar.gz` for linux and darwin, amd64 and arm64, checked by the
  `SHA256SUMS` beside them.
- On Debian or Ubuntu, the `.deb` from the same page: `sudo dpkg -i slk_*_amd64.deb` (or `_arm64`).

## Credentials: the user does this, not you

slk authenticates with two values from the user's signed-in browser: an `xoxc-` token and the `d`
cookie (`xoxd-`). Together they are the user's whole Slack account, so getting them is the user's
job. Don't read browser cookie databases, profile directories or keychains to find them, and don't
ask the user to paste them into the conversation.

Give the user these steps, to run in their own terminal:

1. Open the Slack workspace in Chrome or Firefox, signed in.
2. Open DevTools (F12), go to Network, filter on `/api/`, and click any request.
3. Right-click it, then Copy > Copy as cURL.
4. Run `slk configure --from-curl -`, paste, and press Ctrl-D.

Then confirm with `slk auth test --json`. Run with no flags, `slk configure` prompts for each value
instead and prints the same walkthrough.

## Expired credentials (exit 3)

Routine: the browser session rotated. The fix is the same copy-as-cURL, replacing the profile:

```sh
slk configure --from-curl - --force
```

## Exit 7 with a config file present

The file lives at `slk config path` (normally `~/.config/slk/config.yaml`). slk refuses to read it
unless it is mode 0600, and reports any invalid field by name; either way `error.hint` and
`error.message` say exactly what to change. Never bypass the permission check with
`SLK_INSECURE_CONFIG_PERMS`.

## Several workspaces

Each workspace is a profile, named after its subdomain by default. `slk config show --json` lists
them with credentials redacted and marks the default; `--profile <name>` selects one per call.
Configuring a workspace makes it the default unless `--set-default=false` is passed.

## Containers and CI

With no config file, these environment variables form a profile on their own: `SLK_WORKSPACE`,
`SLK_TOKEN` and `SLK_COOKIE`, plus `SLK_COOKIE_DS` and `SLK_ENTERPRISE_ID` on enterprise grids.
`SLK_PROFILE` selects a profile, and `SLK_CONFIG` points at a different config file.
