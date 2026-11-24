# Signal account API

The routes behind the "Signal account" card (System, Notifications) and the group picker of Signal channels. They
talk to the database rows that the Signal service `artex-signal` reads and reports to; they never call the service
itself. Operator guide: [../example-deployment/README.md](../example-deployment/README.md#signal-notifications-optional).
Design: [adr/0011-signal-sidecar-with-database-config.md](adr/0011-signal-sidecar-with-database-config.md).
Source: `server/signal_api.go`.

## Rules for every route

- **Token in the `Authorization` header only:** `Authorization: Bearer <token>`. A cookie or a query parameter is not
  accepted, so a request forged by another site cannot carry the token. A missing or invalid token, or a token
  without a subject, answers `401` `{"error": "Unauthorized"}`. The subject names the operator in the audit lines.
- **JSON only:** every `POST`, and any request with a body, needs `Content-Type: application/json` (parameters such
  as `charset` are allowed). Otherwise `415` `{"error": "Content-Type must be application/json"}`.
- **`Cache-Control: no-store`** on every response, including `GET /api/notify/signal/link`, which returns the link
  code.
- **Error bodies** are `{"error": "<text>"}`, and where the client has to tell cases apart also `"code": "<code>"`.
- Bodies of the password and confirm routes are at most 64 KiB (`413` `Request body too large`) and must arrive
  within 10 seconds; malformed JSON answers `400` `Invalid request format`.

Errors that every route can return:

| Status | Code | Text | When |
|---|---|---|---|
| 503 | | `management console data source (PostgreSQL) is not connected` | no database |
| 503 | `not_configured` | `Signal service is not configured: set ARTEX_SIGNAL_API` | `ARTEX_SIGNAL_API` is not set (every route except the status route) |
| 503 | `busy` | `Signal state is locked by another database session, retry later` | a lock on the Signal rows was not granted within 3 seconds |
| 503 | | `Data source temporarily unavailable, please retry later` | any other database error (details only in the server log) |

## Status

### `GET /api/notify/signal/account`

Answers `200`. Without `ARTEX_SIGNAL_API` it returns `"mode": "off"` with only `state`, `db_role`, `db_auth` and
`warnings` meaningful. A status read is also where the server notices that the service finished a command.

```json
{
  "mode": "on",
  "sidecar": {"alive": true, "heartbeat_age_s": 4, "version": "v0.0.0", "signal_cli_version": "0.14.10"},
  "state": "linked",
  "gen": 3,
  "account": {"number_hint": "+15… (11 digits)", "confirmed": true},
  "linking": null,
  "groups_count": 2,
  "groups_at": "2026-11-24T10:00:00Z",
  "remote_unlink": "",
  "last_error": "",
  "last_error_at": null,
  "db_role": {"state": "ok", "detail": ""},
  "db_auth": "ok",
  "warnings": []
}
```

| Field | Values |
|---|---|
| `sidecar.alive` | the service reported within the last 30 seconds |
| `state` | `unknown` (starting, nothing reported yet), `unlinked`, `linking`, `linked`, `error` |
| `gen` | the command generation; the confirm route needs it |
| `account` | only for `linked`: the first three characters and the digit count of the number, never the number itself |
| `linking` | only for `linking`: `{"expires_at": "<RFC 3339>"}` |
| `remote_unlink` | after an unlink: `local_only` (the device must be removed on the phone) |
| `last_error` | the service's last error, with link codes and full numbers removed |
| `db_role.state` | `ok`, `missing`, `error`, `unchecked`, with a short `detail` |
| `db_auth` | `ok`, `trust`, `unchecked`, `asserted` |
| `warnings` | `[{"kind": "...", "text": "..."}]` |

## Linking

### `POST /api/notify/signal/link`

Starts a link. Request:

```json
{"password": "<current password>", "device_name": "ARTEX", "acknowledge": true}
```

`device_name` is optional (default `ARTEX`): 1 to 50 letters, digits, spaces, dots, underscores or hyphens; it is
shown on the phone. Success: `202` `{"gen": 4}`. The service then creates the link code; fetch it with `GET`.

| Status | Code | Text |
|---|---|---|
| 400 | | `acknowledge must be true: the linked device can read the account's messages` |
| 400 | | `password is required` |
| 400 | | `signal device name must be 1 to 50 characters of letters, digits, space, dot, underscore or hyphen` |
| 403 | `password_incorrect` | `Current password is wrong` |
| 403 | | `Password not initialized; set a password first` |
| 429 | | `Too many failed attempts, retry later`, with `Retry-After` (the login throttle, see the README) |
| 503 | `db_role_not_ready` | `The database role of the Signal service is not ready (<state>)` |
| 503 | `sidecar_unavailable` | `Signal service is not connected` |
| 409 | `link_pending` | `A link is already pending` |
| 409 | `unlink_pending` | `An unlink is still in progress` |
| 409 | `already_linked` | `An account is already linked` |
| 409 | `unlink_first` | `The Signal service is in an error state: unlink first` |
| 409 | `not_ready` | `The Signal service has not reported its state yet` |
| 409 | `conflict` | `The link request conflicts with the current state` |

A wrong password counts against the login throttle. A successful request clears the confirm back-off.

### `GET /api/notify/signal/link`

Returns the pending link code, once the service has created it and until it expires:

```json
{"gen": 4, "uri": "sgnl://linkdevice?...", "expires_at": "2026-11-24T10:02:00Z"}
```

The code is valid for 2 minutes. It is never logged; the server writes one audit line per generation when it is
fetched. `404` `no_link_pending` `No link is pending` while no code is available (not created yet, cancelled,
completed or expired).

### `DELETE /api/notify/signal/link`

Cancels the pending link. No body. Success: `202` with an empty body. `409` `no_link_pending` `No link is pending`.

### `POST /api/notify/signal/account/confirm`

Confirms the linked account. Nothing is sent from an account until it is confirmed. Request:

```json
{"gen": 4, "last_digits": "550100"}
```

`gen` is the `gen` of the status; `last_digits` are the last 6 digits of the linked number. Success: `200` with an
empty body.

| Status | Code | Text |
|---|---|---|
| 400 | | `last_digits must be the last 6 digits of the number` (also for a negative `gen`) |
| 403 | `number_mismatch` | `The digits do not match the linked number` |
| 409 | `stale` | `The account changed: reload and try again` (another generation, no account linked, or a command pending) |
| 429 | | `Too many failed attempts, retry later`, with `Retry-After` in seconds |

**Back-off.** A wrong guess counts like a wrong password in the login throttle. In addition, the third wrong guess
from a source or device starts a wait of 10 seconds, which doubles with each further wrong guess up to 15 minutes.
While the wait runs, the route answers `429` with `Retry-After` before it reads the body. A right answer, or a new
link request, clears it.

## Unlinking

### `POST /api/notify/signal/unlink`

Request: `{"password": "<current password>"}`. Success: `202` with an empty body. The service deletes its local
account data; the device stays on the account until it is removed on the phone.

| Status | Code | Text |
|---|---|---|
| 400 | | `password is required` |
| 403 | `password_incorrect` | `Current password is wrong` |
| 403 | | `Password not initialized; set a password first` |
| 429 | | `Too many failed attempts, retry later`, with `Retry-After` |
| 503 | `sidecar_unavailable` | `Signal service is not connected` |
| 409 | `link_pending` | `A link is pending: cancel it first` |
| 409 | `unlink_pending` | `An unlink is already in progress` |
| 409 | `nothing_to_unlink` | `There is no linked account to unlink` |
| 409 | `conflict` | `The unlink request conflicts with the current state` |

## Groups

### `GET /api/notify/signal/groups`

The groups of the linked account, as the service last reported them (member groups only, not blocked ones):

```json
{"groups": [{"id": "group.<base64 id>", "masked": "group.…abcd", "name": "Findings"}], "fetched_at": "2026-11-24T10:00:00Z"}
```

`id` is the value a channel stores as its recipient. `fetched_at` is `null` before the first list.

### `POST /api/notify/signal/groups/refresh`

Asks the service to read the list again. No body. Success: `202` with an empty body; poll `GET` until `fetched_at`
changes. `409` `not_linked` `No account is linked`.
