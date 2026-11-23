# The CLI

```text
sbx-broker serve                     run the broker until SIGINT or SIGTERM
sbx-broker status [<ns>]             namespaces, listeners and secret states, without values
sbx-broker refresh <ns> [<secret>]   run the fetch commands again
sbx-broker reload                    read the namespace files again now
sbx-broker log [-f] [<ns>]           the last 100 events; -f follows new ones
sbx-broker help                      the usage
```

The environment variables are in [configuration.md](configuration.md#environment-variables).

## The admin socket

- The CLI talks to `serve` over the Unix socket `admin.sock` (mode 0600, in a directory of mode 0700). There is no
  admin TCP port, and a sandbox cannot reach the socket.
- One broker per state directory: `serve` holds a lock on `admin.lock`, and a second `serve` refuses with
  `a broker already runs`.

## `status`

`status` shows each secret's state and its placeholder, never the value:

| State | Shows |
|---|---|
| `ok` | the fetch time, the value's length and the first 8 hex characters of its SHA-256 |
| `failed` | the exit code and the first stderr line of the fetch command |
| `pending` | the fetch still runs |

## `refresh`

`refresh` runs the fetch commands of a namespace, or of one secret, again. It exits 1 when a refreshed secret is not
`ok` afterwards.

## `log`

- The log keeps the last 10,000 events in memory and appends JSON lines to `broker.log` in the state directory
  (mode 0600, rotated to `broker.log.1` at 10 MiB).
- No event holds a value or a query string.
- `log` and `status` print control characters escaped, so a sandboxed app cannot inject terminal escapes or fake lines
  through a path, a method or an upstream message.

| Kind | Meaning |
|---|---|
| `swap` | a request went to the upstream; names the secrets it used |
| `refuse` | the broker refused a request with a 503 or a 403 |
| `violation` | a placeholder of another namespace; comes with the `refuse` event of the 403 |
| `unscanned` | a body the broker did not scan (see [how-it-works.md](how-it-works.md#request-limits)) |
| `upstream` | the upstream failed or answered in a way the broker refuses (a 502) |
| `fetch` | a fetch command ended, with the secret's new state |
| `reload` | a namespace was loaded, reloaded or stopped |
| `error` | a request that could not be scanned (a 400), a namespace file with an error, a listener that did not start |
| `info` | other notes, for example that Postgres listeners come in a later version |
