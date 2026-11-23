# Testing

The broker is proved at three levels:

| Level | Command | Needs |
|---|---|---|
| Unit | `go test ./...` | Go |
| Integration | `go test -tags integration ./test/integration/` | Docker; the fixed ports 29691 to 29693 free |
| End to end | `go test -tags e2e -timeout 20m ./test/e2e/` | sbx; the `sbx-instance` wrapper of sbx-instances in `PATH`, with the instance `check` selected for this folder; the template `sbx-stack-base:0` in that instance |

Go caches a passing result and then prints `(cached)` without running the proof. Add `-count=1` to run it again, for
example after an sbx update.

## Integration

The recorder mock (`test/mocks/recorder`, an echo API and a mock IdP) runs in a container. The broker runs in the test
with the test namespaces `t1` (ports 29691 and 29692) and `t2` (port 29693), and the cases of `test/cases` go through
the routes of `t1`. The test asserts:

- the mock got the real value in each place;
- no answer and no line of `broker.log` holds a value in any covered form;
- the refusals are right and reach no mock;
- one log event per case.

## End to end

The same mock and broker run on the Mac, and the test client (`cmd/sbx-broker-testclient`) runs the cases inside a real
sandbox `check-broker`. The test kit in `test/e2e/kit` opens only the ports of `t1`, so the test also asserts that port
29693 gets the sbx proxy's 403 and shows as blocked in `sbx policy log`. The test removes the sandbox at the end, also
after a failure.

### Prerequisites

- The test runs sbx only in the instance `check`. Plain `sbx` must be the wrapper of sbx-instances, which picks the
  instance from the marker file `.sbx-instance` above this folder; `sbx-instance which` must say `check`.
- The test stops at once when `sbx-instance` is not in `PATH` or names another instance.
- The instance needs the template `sbx-stack-base:0` (loaded by `build.sh` of sbx-stack-base).

### A pending approval

Each run leaves a pending network approval for `localhost:29693` in the instance, because the port stays closed on
purpose. The test never approves it. You can review these requests with `sbx policy approval ls` and reject or dismiss
them; do not grant them.
