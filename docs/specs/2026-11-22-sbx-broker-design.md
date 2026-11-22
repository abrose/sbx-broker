# sbx-broker: a credential broker on the Mac for task sandboxes

Status: approved by the owner on 2026-11-22 (Crit review). The builder check is done (designer's calls 7 to 9).

## Context

The owner moves the development of a multi-repo project into sbx task sandboxes, slice by slice. Each part of a slice
must also fit other projects. The slices, in the owner's order (2026-11-20):

| # | Slice | Parts |
|---|---|---|
| 1 | Docker in the sandbox | `sbx-kit-docker` (done, 2026-11-22) |
| 2 | **Cluster services in the sandbox (this design)** | `sbx-broker`, a new repo |
| 3 and later | One repo of the project per slice | its language kits, and the first broker namespace |

The services of a project often call services in a dev cluster, which the developer reaches through
`kubectl port-forward`: one forward per service, on a loopback port of the Mac. Many calls carry a credential: a client
secret, an API key, a database password. The agent in a sandbox must be able to make these calls, but must never see
or hold a long-lived credential.

## Intent

A credential never enters a sandbox. The app in the sandbox holds a **placeholder** in place of each secret. It calls
the **broker** on the Mac, and the broker swaps the placeholder for the real value on the way out and removes the real
value from what comes back. The owner manages the secrets and watches every swap in a TUI.

## Goals

1. An app in a sandbox reaches a cluster service through the broker, with a placeholder as its credential, and the
   call succeeds.
2. The swap works where apps commonly put credentials:
   - in HTTP headers, also in a Base64 form (an API key header sent as Base64, `Authorization: Basic`);
   - in a form body (`client_secret` of an OAuth token request);
   - in the Postgres login (through the broker's own Postgres proxy).
3. **Namespaces:** one namespace per project. The secrets of one namespace are never swapped into a request that
   arrives on the listener of another namespace.
4. The secret values come from **fetch commands** that the owner already uses: `gopass show -o <path>`,
   `security find-generic-password -w ...`, `op read op://...`, or any other command that prints the value.
5. A **TUI** to add, edit, remove and inspect secrets and routes, and to follow a live log of every swap.
6. **Clear proof:** mock services in containers stand in for the cluster, an app in a real sandbox calls them through
   the broker, and the checks show what each mock received and what the app got back (owner, 2026-11-22).
7. **Extensible:** a route can use a transform other than the plain swap, for cases where a swap breaks the request,
   for example a body signed with the secret (owner, 2026-11-22).

## Non-goals

- **Read-only access.** The broker passes on the credentials that the owner configures. Choosing a role with the right
  rights is the owner's job (owner, 2026-11-22).
- Running in Docker or on Linux for now. The broker is Mac-native (owner, 2026-11-22). The Go code stays portable; only
  the service setup (launchd) is Mac-specific.
- Starting or watching port-forwards. The developer's own tooling owns them; the broker only forwards to their ports.
- TLS from the sandbox to the broker. That hop is plain HTTP on the Mac (see "Security").
- A secret store of its own. The values live where the owner already keeps them.
- An sbx kit for the broker, or a generated one per namespace. The project's own kit holds the port rules (owner,
  2026-11-22).
- Scripted or plugged-in transforms (Starlark, WASM, plugin processes). A new transform is Go code in the broker,
  added when a case needs it (owner, 2026-11-22).

## Anti-goals

- A real secret value on disk, in a log, in the TUI by default, or in any response to the sandbox.
- A secret of namespace A in a request on a listener of namespace B.
- An admin interface reachable from a sandbox.
- A broker that forwards a request with an unswapped placeholder of its own namespace as if it were fine.

## Facts this design builds on

| # | Fact | Source |
|---|---|---|
| B1 | A sandbox reaches a Mac loopback port at `host.docker.internal:<port>` after a network rule `localhost:<port>`. A kit rule `hosts: ["localhost:${{ kit.args.<arg> }}"]` works | F18, slice 1 |
| B2 | sbx swaps its own placeholders in headers only, not in bodies, and only as the literal text | probe 2026-11-20 |
| B3 | Some apps send an API key header as Base64 of the key, so a literal-text swap never sees the placeholder | general |
| B4 | OAuth client secrets often go in the form body (`client_secret_post`), and an app may offer no setting that moves them to a header | general |
| B5 | An app may take some IdP endpoint URLs from settings of their own (config can point them at the broker) and derive others from the token issuer (only a code change can move them) | general |
| B6 | An IdP error body can quote the rejected client secret | general |
| B7 | A project file can list a kit by a relative or absolute local path | kit-builder skill |
| B8 | A Postgres upstream may require TLS (`sslmode=require`). Developers often keep their passwords in `~/.pgpass` | general |

## Approaches

The owner picked approach 1 on 2026-11-22.

1. **One gate program in Go (picked).** One binary does all HTTP routes: the swap in headers and bodies, and the scrub
   of responses. One HTTP component, simple unit tests, nothing third-party in the HTTP secret path. Cost: our own Go
   code. (At first with PgBouncer for Postgres; see below.)
2. **Caddy for header routes, plus a small token service.** Caddy cannot change a request body, so the token path still
   needs our own code. Two HTTP components and two configs.
3. **mitmproxy in reverse mode, with a Python addon.** Short addon, but a large Python tool built for interception sits
   in the secret path.

Where it runs, decided in the same talk:

- **Docker, with host-side fetching (rejected by the owner).** A fetch command needs the Mac (Keychain, GPG agent,
  1Password app), so the design split into a host CLI that fetches and a container that swaps. The owner did not want
  two parts.
- **Mac-native, one process (picked).** The broker runs the fetch commands itself, as the owner's user.

Postgres, decided after the builder check:

- **PgBouncer (rejected).** It reads passwords only from a file, so a value would touch disk. It answers a missing
  secret with a plain login failure, it allows one password per user name, and it is one more process to supervise.
- **The broker's own Postgres proxy in Go (picked).** No value on disk, the same clear errors and log as HTTP, routing
  by database name, no Homebrew dependency. Cost: our own protocol code, on top of `pgx`.

Transforms, decided in the same talk:

- **Go interface plus Starlark scripts (rejected by the owner).** No rebuild for a new case, but an interpreter and its
  sandbox to maintain.
- **Plugin processes (rejected).** Each secret value would leave the broker process.
- **Go only (picked).** A new transform is Go code with its tests, added when a case needs it.

## Design

### The repo and the binary

- Repo `sbx-broker` (public, MIT), Go. The owner accepted the name on 2026-11-22.
- One binary `sbx-broker`. With no subcommand it starts the TUI (owner, 2026-11-22):

```text
sbx-broker                           # the TUI, a client of the running broker
sbx-broker tui                       # the same, by name
sbx-broker serve                     # the broker process; launchd starts it, or a terminal pane
sbx-broker status [<namespace>]      # namespaces, listeners, secret states, without values
sbx-broker refresh <ns> [<secret>]   # run the fetch commands again
sbx-broker log [-f] [<ns>]           # the swap log
sbx-broker install | uninstall       # the launchd agent
```

- The CLI and the TUI talk to `serve` over a Unix socket `~/.local/state/sbx-broker/admin.sock`, mode 0600, in a
  directory of mode 0700. There is no admin TCP port. A sandbox cannot reach a Unix socket on the Mac.

### Namespaces

- One file per namespace: `~/.config/sbx-broker/namespaces/<ns>.yaml`. It holds no secret values, only commands.
  The broker reads only this directory. It follows no symlink out of it.
- The file holds commands that the broker runs as the owner. So `serve` refuses a file, and the directory, unless the
  owner owns it and no group or other user can write it.
- The TUI writes the file in place and keeps its comments and key order (a YAML node edit, not a new dump).
- A namespace name matches `^[a-z0-9]{1,16}$`: no `-` and no `_`, so a placeholder has exactly one reading (see below).
- **The listener port decides the namespace.** The broker cannot tell which sandbox sent a request: all of them arrive
  from the Mac itself. So every listener belongs to exactly one namespace, and a request on it may use only the
  secrets of that namespace. The project kit of a namespace allows only its own ports.
- Two namespaces may not share a port. The same secret name in two namespaces is fine: the placeholders differ.
- `serve` reloads a namespace file when it changes (or on `sbx-broker reload`). A file with an error keeps the old
  state of that namespace and logs the error.

```yaml
# ~/.config/sbx-broker/namespaces/<ns>.yaml
secrets:
  idp-client-secret:
    command: ["gopass", "show", "-o", "<path>"]
  orders-api-key:
    command: ["op", "read", "op://<vault>/<item>/credential"]
  app-db-password:
    command: ["sh", "-c", "grep '^<host>:5432:' ~/.pgpass | cut -d: -f5"]

http:
  - name: idp
    listen: 29610
    upstream: https://<idp-host>
  - name: orders-api
    listen: 29611
    upstream: http://127.0.0.1:9080

postgres:
  listen: 29620
  databases:
    app:
      upstream: 127.0.0.1:5432
      dbname: <db>
      user: <user>
      password: app-db-password        # a secret name of this namespace
```

### Secrets and placeholders

- A secret definition: a name (`^[a-z0-9][a-z0-9-]{0,40}$`) and a `command` as an argv list. A string runs through
  `sh -c` only when the owner writes `["sh", "-c", "..."]`.
- **Placeholder:** `sbxb_<ns>_<name>` with `-` written as `_`, for example `sbxb_acme_idp_client_secret`. It uses
  only letters, digits and `_`, so it survives URL encoding, form encoding, JSON and Base64 unchanged in meaning. It is
  stable, so app config can hold it. The namespace part ends at the second `_`, because a namespace name has no `_`.
  So the broker always knows the namespace of a placeholder, also of one whose secret is not defined.
- A placeholder of this namespace whose name is not defined counts as "not loaded": 503.
- **Fetch:** at `serve` start, at a reload of the namespace, and on `refresh`. Never on a timer. The command runs as the
  owner's user, with a timeout of 60 s (an unlock prompt may need time), no stdin, and stdout as the value with one
  trailing newline removed. Its stdout never goes to the log. Its stderr goes only to `status` (the first line, in the
  `failed` state): the log event of a failed fetch holds only the exit code or a fixed reason, because stderr may hold a
  value (ruling during the build, Task 7).
- **State per secret:** `ok` (with the fetch time), `failed` (with the exit code and the first stderr line), or
  `pending`. The value lives in memory only. The broker locks its memory pages where the OS allows it and never writes a
  value anywhere.
- A value shorter than 8 characters is refused: the response scrub could not tell it from normal text.

### The HTTP swap

Each HTTP route is one listener on `127.0.0.1:<listen>` and one upstream. A route forwards every method and path as
they come (a reverse proxy, no path prefix), so an app only changes `http://localhost:9080` to
`http://host.docker.internal:29611`.

**Request side,** before forwarding:

1. Find every placeholder of the form `sbxb_<ns>_<name>`:
   - in each header value, as literal text;
   - in each header value that is Base64 (also the part after `Basic `), after decoding;
   - in the URL query;
   - in the body, if its type is `application/x-www-form-urlencoded`, `application/json` or `text/*`, up to 1 MiB.
2. A placeholder of this namespace whose secret is `ok`: swap it for the value. In a Base64 header, decode, swap,
   encode again. In a form body, swap the decoded field and encode again. Fix `Content-Length`.
3. A placeholder of this namespace whose secret is not `ok`: answer **503** with a JSON body that names the secret and
   its state. Do not forward.
4. A placeholder of **another** namespace: answer **403**, log it as a namespace violation, do not forward.
5. A body over 1 MiB, or of another type: forward unchanged and not scanned. Log that it was not scanned.

**Response side,** before returning:

1. Replace each real value of this namespace with its placeholder, in all headers and in the body, streamed (with a
   window as long as the longest value), for every content type. Also the Base64 form of each value.
2. The broker changes no other header of the response.

The upstream TLS uses the Mac's system roots. The broker sets `Host` to the upstream host, keeps `X-Forwarded-*` off,
and sends no header that names the broker.

### Transforms

The request swap and the response scrub above are the transform `swap`. Some requests break under a plain swap: a
body or header signed with the secret (an HMAC), or an AWS SigV4 request that the app signed with the placeholder.
Such a request needs a transform that does more, for example signs it again with the real value.

- A transform is a Go type in the broker. It implements one interface: it gets the request and a lookup for the
  secrets of its namespace, and returns the changed request. A second method does the same for the response.
- A route names its transforms in order. The default is `[swap]`. A transform can take settings:

```yaml
http:
  - name: storage
    listen: 29612
    upstream: http://127.0.0.1:9000
    transforms:
      - sigv4: {access-key: storage-access-key, secret-key: storage-secret-key}   # a later transform
      - swap
```

- The secret lookup of a transform reaches only its own namespace, the same as `swap`.
- The 403 and 503 rules apply to every transform: an unknown or unloaded secret stops the request.
- **Only `swap` ships now** (owner, 2026-11-22). A new transform is added when a case needs it, with unit tests and
  its own mock in the proof. No scripts and no plugins.

### Postgres

The broker has its own Postgres proxy, in Go, on top of `pgx` (its `pgproto3` and `pgconn` packages).

- **One listener per namespace** on `127.0.0.1:<listen>`. The `dbname` of the app's login picks the database entry
  (`orders`, `app`, ...), so one port serves every database of the namespace. Two entries may use the same `user` with
  different passwords.
- **Client side:**
  - The broker answers an `SSLRequest` with `N`: the hop from the sandbox is plain, like HTTP. The app's URL uses
    `sslmode=disable` or `prefer`. The agent context says so.
  - The broker asks for a cleartext password. The app sends the placeholder, which is not a secret.
  - The broker checks the `user` and the placeholder against the database entry. A placeholder of another namespace
    ends the login with a `FATAL` error (code `28000`) and a message like the HTTP 403. A secret that is not loaded
    ends it with a `FATAL` error and a message like the HTTP 503. An unknown `dbname` ends it with code `3D000`.
- **Server side:** the broker logs in to `upstream` with `pgconn`: TLS with `sslmode=require` and the real password
  (SCRAM or whatever the server asks for).
- **After the login:** the broker passes the upstream's startup messages to the app, then copies bytes both ways
  without reading them. It does not scan Postgres traffic after the login.
- **Cancel requests:** the broker gives the app the upstream's key data and forwards a `CancelRequest` on its listener
  to the upstream that owns that key.
- No value touches disk.

### The swap log

- One event per swap or refusal: time, namespace, route or database, method, path without the query, the secret names
  swapped or scrubbed, the direction, the upstream status, and the duration. Never a value, never a query string,
  never a body.
- One event per Postgres login: namespace, database entry, user, result, and later the connection's duration.
- Also events for fetches (`ok` or `failed`), reloads, namespace violations, and unscanned bodies.
- Kept in memory (the last 10,000 events) and appended to `~/.local/state/sbx-broker/broker.log` as JSON lines,
  rotated at 10 MiB.

### The TUI

A Go TUI (Bubble Tea). Views:

| View | What it shows and does |
|---|---|
| Namespaces | each namespace, its listeners, and how many secrets are `ok` |
| Secrets | name, placeholder, command, state, last fetch, swap count. Add, edit, remove a definition (it writes the namespace file). Refresh one or all |
| Inspect | the details of one secret. The value stays hidden. It shows its length and the first 8 hex characters of its SHA-256, so the owner can compare it with the source. A reveal needs a key press and a confirmation, and is logged |
| Routes | the HTTP routes and the Postgres databases, with add, edit, remove |
| Log | the swap log, live, with a filter by namespace and by kind |

The TUI never holds a value it does not show.

When no broker runs, the TUI says so on one line, names `sbx-broker serve` and `sbx-broker install`, and exits with
code 1. It does not start the broker itself.

### How a sandbox reaches the broker

The broker has no kit and writes none (owner, 2026-11-22). A sandbox needs one runtime network rule
`localhost:<port>` for each listener of its namespace (B1). The project's own kit in `sbx-projects/<project>/` holds
these rules, beside the project's other rules. The app config in the sandbox holds the URLs
(`http://host.docker.internal:<port>`) and the placeholders. The README shows the rule lines and says what a 503 and a
403 from the broker mean.

A changed port in a namespace file needs the same change in the project kit. If the kit is behind, the sbx proxy
refuses the call with a 403, so the mistake shows at once.

### Security

- **What a sandbox can do:** send requests to the listeners of its own namespace, with any placeholder. So it can use
  every secret of its namespace against the upstreams of its namespace. That is the purpose. It cannot read a value: the
  scrub removes values from responses.
- **What a sandbox cannot do:** reach another namespace's listeners (no rule), reach the admin socket (Unix socket),
  or choose the upstream (fixed per listener).
- **Known gaps, accepted:**
  - An upstream that transforms a secret before it echoes it (hashes, splits or re-encodes it other than Base64)
    defeats the scrub.
  - The sandbox-to-broker hop is plain HTTP on the Mac. Any process on the Mac can connect to a listener, as it can to
    any port-forward on the Mac.
  - The broker runs as the owner's user, not isolated.

### An IdP call that cannot move to the broker

Some apps derive an IdP URL from the token issuer, so config cannot point that call at the broker (B5). Such a call
goes straight to `<idp-host>`, with a placeholder as its client secret. If the app also fetches discovery and JWKS from
`<idp-host>`, the project kit needs a rule for that host that allows only `GET` and `HEAD`. The `POST` of the unmoved
call then gets a 403 from the sbx proxy and never leaves the sandbox, so the IdP never sees the placeholder. Only that
one call fails; the project's agent context should say so.

## Proof

The owner wants clear proof that the broker does what this design says. Three levels:

| Level | What runs | When |
|---|---|---|
| Unit | Go tests of the swap (literal, Base64, Basic, form, JSON, query), the scrub (streamed, split across chunks, Base64), the namespace rules (403, 503), the placeholder parser, the fetch (timeout, exit code, trailing newline), the Postgres login messages, and each transform | each commit, seconds |
| Integration | testcontainers-go starts the mock services on the Mac's Docker. `serve` runs with two test namespaces. A test client on the Mac plays the app | each commit, under a minute |
| End to end | the same mocks and broker, and the test client runs inside a real `check-broker` sandbox, with a test kit in the repo that opens the test ports | before a release and after an sbx update, a few minutes |

**The mock services** (images built from the repo, started with testcontainers-go):

- **Recording mock API:** it records every request it gets (method, path, query, headers, body). The test reads the
  records back over a separate admin port that no namespace routes to. On request, it echoes a header or the body,
  sets the received value in a response header, or answers an error that quotes it.
- **Mock IdP:** a token endpoint that accepts only the expected `client_secret` in the form body and answers a token.
  On a wrong secret it answers 401 with the received secret in the error body, like the real IdP can (B6).
- **Postgres:** the official image, with SCRAM and a password that only the broker knows.
- **One mock per transform** that checks what the transform must produce (for example a signature).

**The test client** is a small Go program, built for the sandbox's Linux and copied into its workspace. It sends the
cases and prints what it got back as JSON lines. The test on the Mac compares them with the mock records.

**What the proof asserts:**

1. Each mock received the real value, in each place: header, Base64 header, Basic auth, form, JSON, query, Postgres
   login.
2. The app never received a real value: not in a response header, a body, an error body, or a Postgres message, also
   when the mock echoes it.
3. A placeholder of the other test namespace gets 403 (HTTP) or a `28000` login error (Postgres), and the mock records
   nothing.
4. A secret that is not loaded gets 503, or the matching Postgres error, and the mock records nothing.
5. End to end only: the sbx policy blocks the other namespace's ports, and allows the own ones.
6. The log has one event per case, and no log line contains a value. The test greps the log file for every value.
7. Nothing is left: no `check-broker` sandbox, no test container, no test namespace file.

shellcheck, gitleaks and `go vet` run before each commit.

## Left to the builder

1. The Go module layout, the TUI layout, and the libraries besides Bubble Tea.
2. How `serve` notices a changed namespace file (watch or poll).
3. The exact JSON of the admin socket protocol.
4. The launchd plist, and whether `install` also starts it.
5. A probe: does the sbx proxy pass a plain HTTP request to `host.docker.internal:<port>` unchanged, with all headers
   and the body? F18 proved that the port is reachable, not that nothing is changed.

## Designer's calls

1. **The repo name `sbx-broker`.** Go is a build detail. The owner proposed `sbx-go-broker` and accepted this name.
2. **The listener port decides the namespace,** because the broker cannot see which sandbox sent a request.
3. **The Postgres hop from the sandbox is plain, and the app sends the placeholder as a cleartext password.** The
   placeholder is not a secret, and plain matches the HTTP hop. The app's URL must not require TLS.
4. **One listener per HTTP route, no path prefix,** so app URLs keep their paths and only host and port change.
5. **A 503 for a secret that is not loaded, a 403 for a placeholder of another namespace.** Both stop the request, so
   no upstream sees a placeholder by mistake.
6. **No kit at all** (owner, 2026-11-22). The project's own kit holds the port rules. A generated kit added little:
   the rules are a few lines, and a stale rule shows at once as a 403 of the sbx proxy.
7. **A namespace name has no `-` and no `_`,** so each placeholder has one reading (builder check).
8. **The namespace files stay in `~/.config/sbx-broker/namespaces/`,** with an owner-only mode check, because the
   broker runs their commands (builder check).
9. **An IdP call that config cannot move goes straight to the IdP, and the sbx proxy blocks it** (`GET` and `HEAD`
   only for that host). No code change and no HTTPS interception (builder check).
10. **Only the `swap` transform ships now.** The interface is there, and the first other transform comes with the first
    case that needs it, with its own mock in the proof.
