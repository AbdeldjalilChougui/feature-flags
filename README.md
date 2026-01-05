# feature-flags

[![Go Reference](https://pkg.go.dev/badge/github.com/AbdeldjalilChougui/feature-flags.svg)](https://pkg.go.dev/github.com/AbdeldjalilChougui/feature-flags)

A small self-hosted feature-flag server with a Go SDK:

- **Flags with variants.** Booleans, strings, numbers or any JSON value.
- **Environments** (development, production, …), each with its own SDK key
  and its own configuration per flag.
- **Targeting rules** on context attributes (`eq`, `in`, `not_in`,
  `contains`, `starts_with`, `ends_with`, `matches`, `gt/gte/lt/lte`; list
  attributes match if any element does).
- **Percentage rollouts** with consistent hashing. A user keeps their
  variant, and widening 10% → 20% keeps the first 10%.
- A **REST API**, **Server-Sent Events** that push every change to SDKs,
  and an **htmx admin UI** (embedded, no build step).
- A **Go SDK** that evaluates locally from a cache. There's no network call
  per check, and it keeps working on the last known config if the server
  goes down.

## Who it's for

Teams that want trunk-based development and gradual rollouts without a SaaS
bill: a single binary and a JSON file, which is enough for dozens of flags
and many app instances.

## Install

Install the `flagd` server with Go:

```sh
go install github.com/AbdeldjalilChougui/feature-flags/cmd/flagd@latest
```

Add the Go SDK to your module:

```sh
go get github.com/AbdeldjalilChougui/feature-flags/sdk@latest
```

## Quick start

```sh
go build -o flagd ./cmd/flagd
FLAGS_ADMIN_TOKEN=change-me ./flagd -addr :8080 -data flags.json
# prints one SDK key per environment
open http://localhost:8080/admin          # Basic auth: any user, the admin token as password
```

Create a flag through the API:

```sh
curl -H "Authorization: Bearer change-me" localhost:8080/api/flags -d '{
  "key": "new-checkout",
  "variants": {"on": true, "off": false},
  "environments": {
    "production": {
      "enabled": true,
      "off_variant": "off",
      "rules": [
        {"clauses": [{"attribute": "plan", "op": "in", "values": ["pro", "team"]}], "serve": {"variant": "on"}}
      ],
      "default": {"rollout": [{"variant": "on", "weight": 1000}, {"variant": "off", "weight": 9000}]}
    }
  }
}'
```

Use it from Go:

```go
c := sdk.New("http://localhost:8080", os.Getenv("FLAGS_SDK_KEY"))
go c.Start(ctx)
c.WaitReady(ctx)
if c.Bool("new-checkout", flag.Context{"key": user.ID, "plan": user.Plan}, false) {
    // new code path
}
```

### API

| method | path | auth | |
|---|---|---|---|
| GET/POST | `/api/environments` | admin | list / create (`{"key","name"}`) |
| GET/POST | `/api/flags` | admin | list / create (missing environments get an "off" config) |
| GET/PUT/DELETE | `/api/flags/{key}` | admin | read / replace / delete |
| PUT | `/api/flags/{key}/environments/{env}` | admin | replace one environment's config |
| POST | `/api/flags/{key}/environments/{env}/toggle` | admin | kill switch |
| POST | `/api/evaluate` | admin | `{"environment","flag","context"}` → result and reason |
| GET | `/sdk/flags` | SDK key | full config of the key's environment |
| GET | `/sdk/stream` | SDK key | SSE: an `event: put` with the full config on connect and after every change, `: ping` every 20 s |
| POST | `/sdk/evaluate` | SDK key | server-side evaluation for non-Go clients |

SDK keys go in `Authorization: Bearer …` or `?sdk_key=` (for browser
`EventSource`, which can't set headers).

## How it works

- **Evaluation** lives in package `flag` and is shared by the server and
  the SDK. If the flag is disabled, the result is `off_variant`. Otherwise
  the rules are tried in order, and the first whose clauses *all* match
  serves its variant or rollout. Everyone else gets `default`. A clause on a
  missing attribute never matches, not even `neq`/`not_in`, so a rule can't
  accidentally target users you know nothing about. Results include a
  reason (`OFF`, `RULE_MATCH` + index, `DEFAULT`, `ERROR`, and whether a
  rollout picked it).
- **Rollouts.** `bucket = sha256(flagKey + "." + salt + "." + id) mod 10000`,
  and weights are in basis points. Variants take consecutive bucket ranges
  in the order listed, so increasing the first variant's weight only adds
  users. The random per-flag salt means different flags bucket users
  independently. `bucket_by` can split by another attribute, such as `org`.
- **Server.** State (version, environments, flags) is held in memory and
  written to one JSON file atomically on every change. The version counter
  increases on every change. SSE subscribers get a coalescing signal and
  re-send the full payload, so a slow client can't make the server queue
  stale diffs.
- **SDK.** It holds one SSE connection with a watchdog: if nothing arrives,
  not even a ping, for 60 s, it reconnects. When streaming fails (some
  proxies buffer or block SSE), it polls `/sdk/flags` between reconnect
  attempts with exponential backoff, so updates still arrive. Payloads with
  an older version are ignored.
- **Admin UI.** Server-rendered `html/template` plus htmx 1.9.12 (embedded,
  0BSD). Toggles swap a single table row. The config editor is a JSON
  textarea with server-side validation, and a "try it" box evaluates any
  context. Writes require htmx's `HX-Request` header. A cross-site form
  can't send custom headers, so this blocks CSRF even though Basic-auth
  credentials are sent automatically.

## Tests

```sh
go test -race ./...
```

- `flag`: off/default, every operator, number types (Go `int` versus JSON
  `float64`), list attributes, missing attributes, a 20% rollout over 20k
  users (±1.5%), stickiness when widening to 50%, independence across flags,
  `bucket_by`, validation messages, and a JSON round trip.
- `server`: CRUD and validation, default configs for unconfigured
  environments, toggle, evaluate, environments, reload from disk, admin and
  SDK auth, the SSE stream (initial put, a pushed update with a higher
  version, keepalives), and the htmx endpoints (CSRF guard, row partial,
  create, validation notice, save, rollout summary, evaluation tester).
- `sdk`: a real server in-process. It covers readiness, local evaluation,
  an update pushed after a toggle, the cache surviving server shutdown,
  falling back to polling behind a proxy that rejects SSE, and a bad SDK key.

## Status and limitations

- Storage is a single JSON file. That's fine for one server instance and
  hundreds of flags, but it doesn't support multiple replicas (they'd need
  shared storage and fan-out of changes).
- There's no audit log, no per-user accounts or roles, and no scheduled
  changes.
- No segments (reusable user lists) and no semver or date operators.
- The SDK fetches the whole environment. That's fine for server-side use,
  but don't ship an environment's full rules to browsers if they contain
  sensitive targeting data.
- Only a Go SDK. Other languages can use `/sdk/evaluate` or reimplement the
  evaluation rules above.

## License

MIT. See [LICENSE](LICENSE). The embedded htmx is © Big Sky Software,
0BSD-licensed.
