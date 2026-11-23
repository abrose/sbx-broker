# Testing

The broker is proved at three levels:

| Level | Command | Needs |
|---|---|---|
| Unit | `go test ./...` | Go |
| Integration | `go test -tags integration ./test/integration/` | Docker; the fixed ports 29691 to 29695 free |
| End to end | `go test -tags e2e -timeout 20m ./test/e2e/` | sbx; the `sbx-instance` wrapper of sbx-instances in `PATH`, with the instance `check` selected for this folder; the template `sbx-stack-base:0` in that instance |
| launchd | `go test -tags launchd -run TestLaunchdAgent -v ./internal/launchd` | macOS, a logged-in GUI session (the `gui/<uid>` domain) |

Go caches a passing result and then prints `(cached)` without running the proof. Add `-count=1` to run it again, for
example after an sbx update.

## Integration

The recorder mock (`test/mocks/recorder`, an echo API and a mock IdP) runs in a container. The broker runs in the test
with the test namespaces `t1` (HTTP ports 29691 and 29692, Postgres port 29694) and `t2` (HTTP port 29693, Postgres
port 29695), and the cases of `test/cases` go through the routes of `t1`. The test asserts:

- the mock got the real value in each place;
- no answer and no line of `broker.log` holds a value in any covered form;
- the refusals are right and reach no mock;
- one log event per case.

A real Postgres (`postgres:17-bookworm`) runs in a second container, with TLS from the image's snakeoil certificate,
the image's scram-sha-256 password check, and `log_connections=on`. The entry `app` of `t1` has
`require-auth: scram-sha-256`, so a login proves the key does not break SCRAM. The cases of `test/pgcases` log in at
port 29694 with `pgx`:

| Case | Result | Reaches the server |
|---|---|---|
| `pg-login` | works over TLS, `select` shows `app appdb true` | yes |
| `pg-cancel` | a `pg_sleep(30)` ends with `57014` after a cancel request, well under 10 s | yes |
| `pg-other-namespace` | `28000` | no |
| `pg-failed` | `57P03`, the secret did not load | no |
| `pg-wrong-placeholder` | `28P01` | no |
| `pg-unknown-db` | `3D000` | no |
| `pg-wrong-user` | `28000` | no |
| `pg-upstream-refused` | `28P01`, the server refuses the old password | tried, no login line |

"Reaches the server" is read from the server's log: a `connection authorized` line with the case's `application_name`.
The test also asserts one `swap`, `refuse` or `upstream` event per case, and no value in any message, row or log line.

## End to end

The same mock and broker run on the Mac, and the test client (`cmd/sbx-broker-testclient`) runs the cases inside a real
sandbox `check-broker`. The test kit in `test/e2e/kit` opens only the ports of `t1`, so the test also asserts that port
29693 gets the sbx proxy's 403 and shows as blocked in `sbx policy log`. The test removes the sandbox at the end, also
after a failure.

The proof covers Postgres too. The real Postgres container binds 127.0.0.1 on the Mac and only the broker dials it. The
test client runs the cases of `test/pgcases` in the sandbox with `-postgres host.docker.internal:29694`, which the kit
rule `localhost:29694` allows, also for raw TCP, and the same assertions as the integration proof check the outcomes
and the server's log. The same cases run against the closed port 29695 must all end in a connection error (the sbx
proxy accepts the connect and then closes it, so a bare connect proves nothing: only real logins would show a
forward), and `broker.log` must hold no login event of `t2`. The policy log lists `localhost:29694` as allowed and, once
the client has sent its logins, `localhost:29695` as blocked (a bare TCP connect leaves no entry): the sbx-side proof of
the block.

### Prerequisites

- The test runs sbx only in the instance `check`. Plain `sbx` must be the wrapper of sbx-instances, which picks the
  instance from the marker file `.sbx-instance` above this folder; `sbx-instance which` must say `check`.
- The test stops at once when `sbx-instance` is not in `PATH` or names another instance.
- The instance needs the template `sbx-stack-base:0` (loaded by `build.sh` of sbx-stack-base).

### A pending approval

Each run leaves a pending network approval for `localhost:29693` in the instance, because the port stays closed on
purpose. The test never approves it. You can review these requests with `sbx policy approval ls` and reject or dismiss
them; do not grant them.

## launchd

```sh
go test -tags launchd -run TestLaunchdAgent -v ./internal/launchd
```

The proof builds `sbx-broker` into a temp dir and runs it as a real launchd agent with the label
`io.github.abrose.sbx-broker.test`. The plist and the config dir are in temp dirs, the state dir is under `/tmp`, so the
proof never touches your own agent `io.github.abrose.sbx-broker`, its plist or its dirs. The test namespace's fetch
command `fake-fetch` lives only in a temp bin dir that the plist's `PATH` names, so a secret in state `ok` proves that
the agent uses the captured `PATH`. The test asserts:

- the secret is `ok` within 75 s;
- a second install with a changed env replaces the agent: `launchctl print` shows a new pid and the new env, and the
  secret is `ok` again within 75 s;
- after `Uninstall`, `launchctl print gui/<uid>/io.github.abrose.sbx-broker.test` fails, the plist is gone, and no
  process holds the state dir's lock.

The test boots out a test agent left by an interrupted run before it starts, and its cleanup boots the agent out after
a failure. To check by hand that nothing is left:

```sh
launchctl print gui/$(id -u)/io.github.abrose.sbx-broker.test   # must fail
launchctl bootout gui/$(id -u)/io.github.abrose.sbx-broker.test # only if it did not
```
