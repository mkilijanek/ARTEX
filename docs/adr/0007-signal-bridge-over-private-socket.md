# ADR 0007: The Signal bridge is reached only through a private socket

- **Status:** Accepted, built (option A below, approved by the project owner). Not verified yet: delivery from a real
  Signal account and the 2000-byte message limit (verified against the bridge API without a Signal account).
- **Date:** 2026-11-23
- **Runbooks:** [../../example-deployment/README.md](../../example-deployment/README.md#signal-notifications-optional)
  ("Signal notifications", including "Incident: isolation check C12 appeared"), [../tool-isolation.md](../tool-isolation.md) (C12)

## Context

Findings should reach Signal (one group or individual numbers). The practical way to send from a server is the
self-hosted `bbernhard/signal-cli-rest-api` bridge. Two facts drive the design:

- **The bridge has no authentication**, no Unix-socket listener and no bind-address option, and its API does much
  more than send: `/v1/devices/{number}` links a new device to the account (which then reads every future message),
  and there are endpoints to register and unregister the number and manage groups. Whoever can send it an HTTP
  request can take over the Signal account.
- **Agent tools share the server's network namespace.** They run as uid 30002 ([ADR 0006](0006-agent-tools-run-as-a-separate-user.md)) but
  can open any TCP connection the server can, and proxy variables are hints, not enforcement. A bridge on a network
  that `artex` is on is reachable by a prompt-injected tool call. In addition, the notification dial guard allows
  private (RFC 1918) addresses, so a UI-editable URL, for example a generic webhook, could reach it too.

## Decision

Option A: **`artex` has no network path to the bridge; the server reaches it only through a Unix socket under
`/run/artex-private`, served by an allowlisting nginx sidecar.** Shipped as the opt-in override
`example-deployment/docker-compose.signal.yml`.

1. **Networks.** `signal-internal` (`internal: true`) holds only the proxy and the bridge; `signal-egress` gives the
   bridge alone access to Signal's servers. `artex` is on neither, and nothing is published. nginx re-resolves the
   bridge's name, so a restarted bridge with a new address needs no proxy restart.
2. **Socket.** The proxy listens on a socket in the volume `signalsock`, mounted in `artex` at
   `/run/artex-private/signal`. `/run/artex-private` is root `0700` (the same protection as the database socket), and
   the proxy sets the socket to root `0600` as a second layer.
3. **Allowlist.** Exact locations only: `POST /v2/send` (body at most 64 KiB) and `GET /v1/about`; everything else
   gets `403` and never reaches the bridge. The upstream URI is fixed, so the bridge sees only those two paths and no
   query string.
4. **Endpoint and sender are operator configuration.** `ARTEX_SIGNAL_API` (`unix:/absolute/path` only) and one global
   sender `ARTEX_SIGNAL_NUMBER` (E.164), read once at start. The channel config holds only `recipients` and
   `recipient_type`, so the UI can neither point Signal at another host nor pick another account. The client dials
   only that socket, ignores proxy variables and never follows redirects. The shared dial guard is unchanged.
5. **Self-check C12** (only when `ARTEX_SIGNAL_API` is set): the server checks as root that a socket exists, then a
   tool shell must be refused by the socket and must not be able to list its directory. Any other result degrades the
   isolation status. Because the bridge may start after the self-check, C12 alone is re-run after the first
   successful send and on each channel test that reaches the send.
6. **Message and failure handling.** Plain text only (finding text comes from scanned targets); numbers and group ids
   masked before any error text is cut short; a partial delivery is a permanent failure.

## Alternatives rejected

- **B. An internal network shared with `artex`, plus an authenticating proxy** (bearer token from the server
  environment). Tools could still reach the proxy over TCP and probe or overload it; the token is another secret to
  generate, rotate and redact; the webhook channel could reach the proxy host; and C12 would have to accept
  "reachable but authenticated", which is weaker and harder to verify.
- **C. The same shared network with nothing else**, relying on proxy variables: tools open raw TCP; account takeover.
- **D. A firewall rule by uid** (`-m owner --uid-owner 30002`): needs `NET_ADMIN` in the `artex` container, which the
  shipped Compose files do not grant.
- **E. A UI-editable bridge URL, or a per-channel exception in the dial guard:** a user-editable URL to an internal
  host is exactly what must not exist, and an exception list widens the SSRF surface of every channel.
- **F. A socat sidecar instead of nginx:** simpler, but no path allowlist and no body limit.
- **G. The proxy in the bridge's network namespace** (`network_mode: service:signal-cli`): after the bridge is
  recreated the proxy keeps a dead namespace until it is restarted too.
- **A TCP endpoint for the bridge** (`ARTEX_SIGNAL_API=http://...`) is not offered: it would bring back B or C.

## Consequences

- Two more containers (bridge and proxy), two networks and two volumes; `artex` waits at start for the bridge's JVM
  (tens of seconds) while the override is in use.
- Account management (link, register, PIN, group ids, rate-limit challenge, device list) is done only on the host
  with `docker compose exec signal-cli ...`; none of it is reachable through the proxy.
- The protection depends on Docker isolating user-defined networks. Podman, or Docker with `"iptables": false`, do
  not, and users on the Docker host can reach the bridge's container address (accepted).
- One sender per ARTEX instance; changing it needs a restart.
- Only the shipped example deployment supports Signal; the repository's root `docker-compose.yml` does not.
- `signal-data` holds the account keys and needs a backup kept like a secret.
- The bridge answers `400` for transient failures too, so those are not retried automatically (manual retry from the
  delivery records); delivery is at least once (a timeout after the bridge sent can give a duplicate).
- Signal retires old client versions, so the pinned bridge image has to be upgraded regularly.

## Rollback

Run the example deployment without the override and remove the two containers, keeping `signal-data`; Signal
deliveries then fail visibly. Without Signal support in the code, existing Signal channels are an unknown kind and
their deliveries are marked failed. Steps: the example deployment README, "Rollback of the override".

## Outcome to check

No C12 on any start or re-run other than a `not verified: socket absent` that clears after the first send; Signal
deliveries reach `sent`; no `Signal bridge socket unreachable` or retries-exhausted failures that configuration does
not explain.
