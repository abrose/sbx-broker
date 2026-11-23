# sbx-broker

A credential broker on the Mac for apps in Docker Sandboxes (`sbx`).
An app in a sandbox holds a **placeholder** such as `sbxb_acme_api_key` in place of each secret. It calls the broker
on the Mac, and the broker swaps the placeholder for the real value on the way out and takes the value out of what
comes back. The sandbox only ever sees placeholders.

The design is in [docs/specs/2026-11-22-sbx-broker-design.md](docs/specs/2026-11-22-sbx-broker-design.md).

## What it does

- Fetches each secret by a command you configure (`gopass show -o ...`, `security find-generic-password -w ...`,
  `op read ...`, any command that prints the value). The value lives in the broker's memory only: never on disk,
  never in a log, never in an answer to the sandbox.
- Runs one HTTP reverse proxy per route on `127.0.0.1:<port>`, each to one fixed upstream. A route forwards every
  method and path as they come, so an app only changes `http://localhost:9080` to
  `http://host.docker.internal:<port>`.
- Swaps placeholders in the request: in header values (as text, and inside a Base64 token such as
  `Authorization: Basic ...` or a Base64 `X-API-Key`), in the path, in the query, and in a form, JSON or `text/*`
  body.
- Scrubs the values out of every answer: headers and body, streamed, for every content type.
- Keeps the namespaces apart: the listener port decides the namespace, and a request may use only the secrets of
  that namespace.
- Logs every swap and refusal, with secret names only.

## What it does not do

- It does not choose the rights of a credential. It passes on the credentials you configure.
- It has no secret store of its own, and it does not fetch on a timer: only at start, at a reload of the namespace,
  and on `refresh`.
- It does not speak TLS to the sandbox. The hop from the sandbox to the broker is plain HTTP on the Mac.
- It does not start tunnels or watch upstreams. It only forwards to their ports.
- It does not support WebSockets or other protocol upgrades.
- It ships no sbx kit. A project's own kit opens the broker ports (see "The network rules of a project kit").
- It is made and tested for macOS, where it runs as your user; it does not run in Docker. The Postgres proxy is coming in
  plan 4b; the TUI and the launchd agent (`install`, `uninstall`) are coming in plan 4c.

## Build

```sh
go build ./cmd/sbx-broker
```

## A namespace

One namespace per project, one file per namespace: `~/.config/sbx-broker/namespaces/<ns>.yaml`. A namespace name
matches `^[a-z0-9]{1,16}$`. The file holds commands, never values.

```yaml
# ~/.config/sbx-broker/namespaces/acme.yaml
secrets:
  client-secret:
    command: ["gopass", "show", "-o", "acme/idp/client-secret"]
  api-key:
    command: ["op", "read", "op://<vault>/<item>/credential"]

http:
  - name: idp
    listen: 29610
    upstream: https://<idp-host>
  - name: api
    listen: 29611
    upstream: http://127.0.0.1:9080
    transforms: [swap]        # the default; swap is the only transform for now
```

- A secret name matches `^[a-z0-9]+(-[a-z0-9]+)*$`, at most 41 characters. A route name matches
  `^[a-z0-9][a-z0-9-]{0,30}$`. A listen port is in 1024 to 65535, and two routes never share one.
- A `command` is an argv list. It runs as your user, with no stdin and a timeout of `SBX_BROKER_FETCH_TIMEOUT`
  (default `60s`). Its stdout, with one trailing newline removed, is the value. A shell runs only when you write
  `["sh", "-c", "..."]`. A value under 8 bytes is refused: the scrub could not tell it from normal text.
- The placeholder of a secret is `sbxb_<ns>_<name>` with each `-` as `_`: `sbxb_acme_client_secret`. It holds only
  letters, digits and `_`, so it passes URL, form, JSON and Base64 encoding unchanged in meaning.
- `serve` refuses a namespace file or the namespace dir that your user does not own, that is group- or
  world-writable, or that is a symlink: the broker runs the commands in it.

## The CLI

```text
sbx-broker serve                     run the broker until SIGINT or SIGTERM
sbx-broker status [<ns>]             namespaces, listeners and secret states, without values
sbx-broker refresh <ns> [<secret>]   run the fetch commands again
sbx-broker reload                    read the namespace files again now
sbx-broker log [-f] [<ns>]           the last 100 events; -f follows new ones
sbx-broker help                      the usage
```

| Variable | Default |
|---|---|
| `SBX_BROKER_CONFIG_DIR` | `~/.config/sbx-broker/namespaces` |
| `SBX_BROKER_STATE_DIR` | `~/.local/state/sbx-broker` (`admin.sock`, `admin.lock`, `broker.log`) |
| `SBX_BROKER_FETCH_TIMEOUT` | `60s` (a Go duration) |

- The CLI talks to `serve` over the Unix socket `admin.sock` (mode 0600, in a dir of mode 0700). There is no admin
  TCP port, and a sandbox cannot reach the socket.
- One broker per state dir: `serve` holds a lock on `admin.lock`, and a second `serve` refuses with
  `a broker already runs`.
- `status` shows each secret's state (`ok`, `failed` with the exit code and the first stderr line, or `pending`), its
  placeholder, and for `ok` its length and the first 8 hex characters of its SHA-256, never the value.
- `refresh` exits 1 when a refreshed secret is not `ok` afterwards.
- `log` and `status` print control characters escaped, so a sandboxed app cannot inject terminal escapes or fake
  lines through a path, a method or an upstream message.
- The log keeps the last 10,000 events in memory and appends JSON lines to `broker.log` (mode 0600, rotated to
  `broker.log.1` at 10 MiB). Kinds: `swap`, `refuse`, `violation`, `unscanned`, `upstream`, `fetch`, `reload`,
  `error`, `info`. No event holds a value or a query string.

### Reloads

- `serve` checks the config dir every 1 s. A change counts once two checks in a row saw it, so a changed, new or
  removed file takes effect within about 2 s.
- `sbx-broker reload` acts at once. It also retries files that were refused and namespaces whose listener did not
  start (for example a port that was in use).
- A reload restarts the whole namespace: it closes its listeners, opens them again and fetches all its secrets again.
  A file with an error keeps the old state of that namespace and logs the error.

## How an app uses a placeholder

### HTTP

Point the app at the broker route and give it the placeholder where it would hold the secret. Two requests as the
app sends them:

```text
GET /v1/items HTTP/1.1
Host: host.docker.internal:29611
X-API-Key: sbxb_acme_api_key

POST /realms/acme/protocol/openid-connect/token HTTP/1.1
Host: host.docker.internal:29610
Content-Type: application/x-www-form-urlencoded

grant_type=client_credentials&client_id=app&client_secret=sbxb_acme_client_secret
```

The upstream gets the real values in their place. The placeholder may also be Base64-encoded in a header, as in
`Authorization: Basic <base64 of user:placeholder>`.

The broker sets `Host` to the upstream's host and adds no header that names it. It removes the app's
`Accept-Encoding`, because the scrub needs the plain bytes of the answer. An upstream that compresses anyway with
`gzip` or `deflate` is decoded and scrubbed, and the app gets the plain answer with no `Content-Encoding`.

### Postgres (coming in plan 4b)

The app's URL will name the broker's Postgres listener, the database entry and the placeholder as the password, with
no TLS on the hop from the sandbox:

```text
postgres://<user>:sbxb_acme_app_db_password@host.docker.internal:29620/<db>?sslmode=disable
```

Until then `serve` reads and checks a `postgres` section of a namespace file, logs that Postgres listeners come in a
later version, and opens no Postgres port.

## The network rules of a project kit

A sandbox reaches the Mac's `127.0.0.1:<port>` at `http://host.docker.internal:<port>` only through a runtime rule
`localhost:<port>`. The broker has no kit; the project's own kit (schema 3) holds one rule per listener of its
namespace, beside its other rules:

```yaml
capabilities:
  - type: com.docker.sandbox/network-policy@2
    config:
      runtime:
        allow:
          - hosts: ["localhost:29610", "localhost:29611"]   # the routes of namespace acme
```

A project kit opens only the ports of its own namespace. A changed port in a namespace file needs the same change in
the kit: if the kit is behind, the sbx proxy answers 403 (see below).

## The broker's answers

The broker refuses a request with a JSON body and forwards nothing:

| Status | Body | Meaning |
|---|---|---|
| 503 | `{"error":"secret not loaded","secret":"<name>","state":"<pending\|failed\|undefined>"}` | A placeholder of this namespace whose secret is still fetching, failed its fetch, or is not defined. Check `sbx-broker status`, fix the command, `sbx-broker refresh <ns> <secret>` |
| 403 | `{"error":"placeholder of another namespace","namespace":"<ns>"}` | The app sent a placeholder of another namespace. Logged as a `violation` |
| 502 | `{"error":"upstream unreachable","route":"<route>"}` | The upstream did not answer: a tunnel is down, a wrong port |
| 502 | `{"error":"unsupported content encoding","route":"<route>"}` | The upstream answered in an encoding the scrub cannot read (`br`, `zstd`, or a list of codings). `gzip` and `deflate` are decoded and scrubbed |
| 502 | `{"error":"protocol switch not supported","route":"<route>"}` | The upstream switched protocols (a WebSocket or another upgrade). The raw stream would pass every transform, so it is closed |
| 400 | `{"error":"bad request","route":"<route>"}` | The request could not be scanned (for example a body that could not be read) |

A 403 **without** a JSON body like the one above comes from the sbx proxy, not from the broker: the sandbox has no
rule for that port. Its text says `Approval required for <host>:<port>`. Add the rule to the project kit; do not
approve it by hand.

## What the scrub covers

The broker takes each value of the namespace out of every answer header and body, for every content type, in these
forms, each replaced by the same form of its placeholder:

- the raw value;
- Base64, standard and URL-safe, of the whole value, and embedded in a longer Base64 text (such as an echoed
  `Authorization: Basic` header); in an embedded value up to 4 bits at each edge stay unscrubbed;
- `url.QueryEscape` and `url.PathEscape`;
- the JSON string escape, without and with HTML escaping (`&`, `<`, `>`), and with `/` as `\/`;
- the `encodeURIComponent` form;
- the lowercase-hex versions of the percent-encoded forms (`%2f` for `%2F`);
- the values that went into this request, also when a `refresh` changed or dropped them since.

Other re-encodings are **not** covered: a hash, a split value, another Base64 alphabet, the plain hex of a value,
double encoding, or any other transformation an upstream makes before it echoes a value.

The request side has limits too. The broker scans a body only when it is `application/x-www-form-urlencoded`,
`application/json` or `text/*`, at most 1 MiB, and not compressed (no `Content-Encoding`, or `identity`). Any other
body is forwarded unscanned and logged as `unscanned`; a placeholder in it reaches the upstream as is.

## Known gaps

These are accepted in the design:

- An upstream that transforms a value before it echoes it (hashes, splits or re-encodes it other than the forms
  above) defeats the scrub.
- The hop from the sandbox to the broker is plain HTTP on the Mac. Any process on the Mac can connect to a listener.
- The broker runs as your user, not isolated.
- A sandbox can use every secret of its namespace against the upstreams of its namespace. That is the purpose; it
  cannot read a value.
- The broker cannot tell sandboxes apart: all requests come from the Mac itself. The port decides the namespace, so
  sandboxes of one project share its secrets.

## The proof

Three levels:

| Level | Command | Needs |
|---|---|---|
| Unit | `go test ./...` | Go |
| Integration | `go test -tags integration ./test/integration/` | Docker; the fixed ports 29691 to 29693 free |
| End to end | `go test -tags e2e -timeout 20m ./test/e2e/` | sbx, the instance `check`, the template `sbx-stack-base:0` |

Go caches a passing result and then prints `(cached)` without running the proof. Add `-count=1` to run it again, for
example after an sbx update.

- **Integration:** the recorder mock (`test/mocks/recorder`, an echo API and a mock IdP) runs in a container, the
  broker runs in the test with the test namespaces `t1` (ports 29691 and 29692) and `t2` (port 29693), and the cases
  of `test/cases` go through the routes of `t1`. The test asserts that the mock got the real value in each place,
  that no answer and no line of `broker.log` holds a value in any covered form, that the refusals are right and reach
  no mock, and one log event per case.
- **End to end:** the same mock and broker, and the test client (`cmd/sbx-broker-testclient`) runs the cases inside
  a real sandbox `check-broker`. The test kit in `test/e2e/kit` opens only the ports of `t1`, so the test also
  asserts that port 29693 gets the sbx proxy's 403 and shows as blocked in `sbx policy log`. The test removes the
  sandbox at the end, also after a failure.

## License

MIT, see [LICENSE](LICENSE).
