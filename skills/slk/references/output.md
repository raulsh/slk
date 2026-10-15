# slk JSON reference

Every command takes `--json` and prints one envelope on stdout. This describes envelope schema 1.
`schema` appears in every envelope (and in `slk version --json`); it bumps only when a field is
removed or changes meaning, while new fields can appear without a bump.

## Contents

- [Envelope](#envelope)
- [Message](#message) — `messages`, `thread`, `search`
- [Unread](#unread) — `unread`
- [Conversation](#conversation) — `conversations list`
- [User](#user) — `users info`, `users list`
- [Identity](#identity) — `auth test`
- [Exit codes](#exit-codes)

## Envelope

| Field | Meaning |
|---|---|
| `ok` | `true` on success. On `false`, read `error`. |
| `schema` | Contract version, currently `1`. |
| `command` | The subcommand that ran. |
| `count` | Number of records in `results`. |
| `results` | Always an array; `[]` on failure. |
| `names` | `{"users": {id: name}, "channels": {id: name}}` for ids mentioned inside message text. Absent when there are none. |
| `page` | Search only: `page`, `pages`, `total`, `has_more`. |
| `window` | The resolved time range: `since` and `until` (RFC 3339), `since_ts` and `until_ts` (Slack ts), `tz`. Absent when no range was given. |
| `truncated` | `true` when more existed than was returned. |
| `notes` | What was actually asked, such as the assembled search query. |
| `warnings` | Non-fatal problems: skipped conversations, filters that matched nothing, why a result was truncated. |
| `error` | On failure: `code` and `message`, plus `hint` (the remedy), `retry_after` (seconds) and `slack_error` (Slack's own error string) when known. |

## Message

| Field | Meaning |
|---|---|
| `channel` | Conversation id. |
| `channel_name` | Display name: `#eng` for a channel, `@alice` for a DM. |
| `ts` | The message's id within its conversation, and also its time. `channel` plus `ts` address it: `slk thread <channel> <ts>`. |
| `time` | `ts` as RFC 3339, in the local timezone. |
| `user`, `user_name` | Author id and handle. |
| `bot`, `bot_id` | Posted by an app or bot. `bot_id` is kept when no name resolved. |
| `vip` | The author is someone the user marked VIP in Slack. |
| `text` | Slack mrkdwn as sent, with raw references such as `<@U0123ABCD>`. |
| `text_rendered` | The same text with references resolved to names. Present only when it differs from `text`. |
| `thread_ts` | On a thread message, the parent's `ts`. |
| `reply_count` | On a thread parent, the number of replies. |
| `unread` | Fetched by an `--unread` run. |
| `edited` | The message was edited. |
| `subtype` | Slack's subtype, such as `thread_broadcast` or `file_share`. |
| `files` | `[{id, name, type, size, title}]`, metadata only. |
| `reactions` | `[{name, count}]`. |
| `attachment_count` | Legacy attachments are present; their content is only under `--raw`. |
| `permalink` | A link that opens the message in Slack. |
| `raw` | Slack's untouched payload, only with `--raw`. |

## Unread

Rows come sorted by mentions, then by most recent activity.

| Field | Meaning |
|---|---|
| `channel`, `channel_name` | Conversation id and display name. |
| `type` | `channel`, `private`, `mpim` or `dm`. |
| `mention_count` | Unread mentions of the user. |
| `count`, `counted` | Slack's client API has no per-conversation unread count, so `count` is 0 and `counted` is false. The row's presence is what means "has unreads". |
| `thread_unread`, `thread_mentions` | Unread thread replies, and mentions among them, when Slack reports them. |
| `last_read_ts` | The user's read marker. |
| `latest_ts`, `latest_time` | The newest message. |
| `read_state` | `ok`, or `never_read` when the user has never opened the conversation. |

## Conversation

| Field | Meaning |
|---|---|
| `id`, `name` | Conversation id and display name. |
| `type` | `channel`, `private`, `mpim` or `dm`. |
| `user`, `user_name` | For a DM, the other person. |
| `topic`, `purpose`, `members` | When known. |
| `is_archived` | Archived conversations are listed only with `--archived`. |
| `is_shared` | Shared with another organization (Slack Connect). |
| `has_unreads`, `mention_count` | Read state. |
| `last_read`, `latest_ts`, `latest_time` | The user's read marker and the newest message. |

## User

| Field | Meaning |
|---|---|
| `id` | User id. |
| `name` | Handle, as used in `@alice`. |
| `real_name`, `display_name` | Names from the profile. |
| `email`, `title`, `tz` | When the profile has them. |
| `bot`, `deleted` | An app account; a deactivated account. |
| `vip` | Marked VIP by the user. |

## Identity

`auth test` returns one record: `user_id` and `user_name` (who the user is), `team_id` and
`team_name`, `workspace_url`, `profile` (the configured profile that answered), `enterprise` and
`enterprise_id` on an enterprise grid, and `writes_allowed`.

## Exit codes

| Exit | `error.code` | Meaning |
|---|---|---|
| 0 | | Success, possibly with `truncated: true`. |
| 1 | `internal` | Unclassified failure. |
| 2 | `usage` | Bad flag, argument or combination. |
| 3 | `auth` | Credentials rejected: the token or cookie expired. |
| 4 | `rate_limited` | Slack rate limit; wait `retry_after` seconds. |
| 5 | `not_found` | Unknown, ambiguous or inaccessible channel, user or message. |
| 6 | `network` | Network failure, or `--timeout` elapsed. |
| 7 | `config` | Nothing configured, or the config file is unreadable or too permissive. |
| 8 | `write_refused` | A write was refused: it needs the user's explicit opt-in (`allow_writes`). |
| 9 | `partial` | The run ended early with partial results. |
| 130 | `interrupted` | Interrupted. |
