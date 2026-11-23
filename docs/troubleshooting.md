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

## Postgres logins

The broker refuses a login with a `FATAL` error and ends the connection. The checks run in this order, and none of the
first five opens a connection to the real server:

| Code | Message | Meaning and fix |
|---|---|---|
| `28000` | `placeholder of another namespace` | The password holds a placeholder of another namespace. Use this namespace's placeholder. Logged as a `violation` and a `refuse` |
| `3D000` | `database "<db>" is not configured in namespace <ns>` | The database name of the URL is no alias of the `postgres` section. Use an alias |
| `28000` | `user "<user>" may not use database "<alias>"` | The URL's user is not the entry's `user`. Use that user |
| `28P01` | `password is not the placeholder of database "<alias>"` | The password is not exactly the placeholder of the entry's secret (`sbxb_<ns>_<secret>`) |
| `57P03` | `secret not loaded: <secret> (<state>)` | The secret is still fetching or failed. Check `sbx-broker status`, fix the command, then `sbx-broker refresh <ns> <secret>` |
| `08006` | `upstream unreachable: database "<alias>"` | The real server did not answer, or its TLS failed: a tunnel is down, or the `upstream` is wrong |
| the server's | `upstream: <its message>` | The real server refused the login (for example a wrong password). Each value in the message is replaced by its placeholder. Check the secret's value |
| `28000` | `upstream: database "<alias>" requires scram-sha-256, the server asked for another method` | `require-auth: scram-sha-256` is set and the server asked for a cleartext password or MD5. Fix the server's login method, or remove the key if you trust the path |
| `0A000` | `replication connections are not supported` | The startup had a `replication` parameter |
| `08P01` | `message too long` | A startup over 10,000 bytes or a password over 1,000 bytes |
| `08P01` | `unsupported frontend protocol` | The client speaks another major protocol version than 3 |

A connection that sends nothing for 10 s from its accept to the end of its password message is closed with no answer.
The log shows each refusal as a `refuse` or `upstream` event with `<user>@<alias>` in the route column.

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
