# Troubleshooting

## The broker's answers

The broker refuses a request with a JSON body and forwards nothing:

| Status | Body | Meaning and fix |
|---|---|---|
| 503 | `{"error":"secret not loaded","secret":"<name>","state":"<pending\|failed\|undefined>"}` | A placeholder of this namespace whose secret is still fetching, failed its fetch, or is not defined. Check `sbx-broker status`, fix the command, then run `sbx-broker refresh <ns> <secret>` |
| 403 | `{"error":"placeholder of another namespace","namespace":"<ns>"}` | The app sent a placeholder of another namespace. Logged as a `violation` |
| 502 | `{"error":"upstream unreachable","route":"<route>"}` | The upstream did not answer: a tunnel or port-forward is down, or the port is wrong |
| 502 | `{"error":"unsupported content encoding","route":"<route>"}` | The upstream answered in an encoding the scrub cannot read (`br`, `zstd`, or a list of codings). `gzip` and `deflate` are decoded and scrubbed |
| 502 | `{"error":"protocol switch not supported","route":"<route>"}` | The upstream switched protocols (a WebSocket or another upgrade). The raw stream would pass every scrub, so it is closed |
| 400 | `{"error":"bad request","route":"<route>"}` | The request could not be scanned (for example a body that could not be read). Logged as an `error` |

## A 403 from the sbx proxy

A 403 **without** a JSON body like the ones above comes from the sbx proxy, not from the broker: the sandbox has no
network rule for that port. Its text says `Approval required for <host>:<port>`.

Fix: add `localhost:<port>` to the project kit's runtime rules (see the [README](../README.md#3-open-the-ports-in-the-project-kit)).
Do not approve the request by hand.

## Other checks

- **The broker does not start:** `a broker already runs` means another `serve` holds the state directory. A refused
  namespace file is not owned by your user, is group- or world-writable, or is a symlink (see
  [configuration.md](configuration.md#file-ownership)).
- **A port did not open:** another process used it. Free it, then run `sbx-broker reload`.
- **A placeholder reached the upstream unchanged:** the body was not scanned. Check the log for `unscanned` and the
  [request limits](how-it-works.md#request-limits).
- **What happened to a request:** `sbx-broker log -f <ns>` shows each swap and refusal with the secret names.
