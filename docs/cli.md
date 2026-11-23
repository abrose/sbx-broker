# The CLI

```text
sbx-broker serve                       run the broker until SIGINT or SIGTERM
sbx-broker status [<ns>]               namespaces, listeners and secret states, without values
sbx-broker refresh [<ns> [<secret>]]   run the fetch commands again
sbx-broker reload                      read the namespace files again now and retry
                                       the files and listeners that failed
sbx-broker log [-f] [<ns>]             the last 100 events; -f follows new ones
sbx-broker edit [<ns>]                 open the namespace file in $VISUAL or $EDITOR
sbx-broker init <ns>                   mark this folder as a project of <ns> and create its namespace file
sbx-broker help                        the usage
```

`<ns>` defaults to the name in a `.sbx-broker` file in this folder or above (see [the project marker](#the-project-marker)).
The environment variables are in [configuration.md](configuration.md#environment-variables).

## The project marker

The file `.sbx-broker` in a project's root folder names the project's namespace. `sbx-broker init <ns>` writes it.

- The CLI looks for it in the current folder, then in each parent folder, and uses the first one it finds.
- The first line of the file, with spaces trimmed, is the namespace name. It must match `^[a-z0-9]{1,16}$`.
- A namespace given as an argument wins over the marker.
- With no argument and no marker, `status` and `log` show all namespaces, and `refresh` and `edit` stop with an error.
- The CLI refuses a marker that is a symlink or not a regular file. An error names the marker's path, never its
  content.
- `serve` never reads a marker. Why the marker holds only a name: see [security.md](security.md#the-project-marker).

## The admin socket

- The CLI talks to `serve` over the Unix socket `admin.sock` (mode 0600, in a directory of mode 0700). There is no
  admin TCP port, and a sandbox cannot reach the socket.
- One broker per state directory: `serve` holds a lock on `admin.lock`, and a second `serve` refuses with
  `a broker already runs`.

## `status`

`status` shows, for each namespace, a header line and the line `file: <path of the namespace file>`. Under a project
marker it shows only that namespace, and it starts with the line `marker: <path of the marker>`. For a namespace with
no file, it names `sbx-broker init <ns>`.

`status` shows each secret's state and its placeholder, never the value:

| State | Shows |
|---|---|
| `ok` | the fetch time, the value's length and the first 8 hex characters of its SHA-256 |
| `failed` | the exit code and the first stderr line of the fetch command |
| `pending` | the fetch still runs |

## `refresh`

`refresh` runs the fetch commands of a namespace, or of one secret, again. It exits 1 when a refreshed secret is not
`ok` afterwards.

## `edit`

`edit` opens the namespace file in `$VISUAL`, else `$EDITOR`, else `vi`. It splits the setting on spaces, so
`code --wait` works, and it runs the editor with no shell. It needs no running broker. The broker reads the changed
file within about 2 s. For a namespace with no file, `edit` names `sbx-broker init <ns>`.

## `init`

`init <ns>` sets up a project in the current folder. It needs no running broker.

1. It writes `.sbx-broker` with `<ns>`. If the folder has a marker with the same name, it keeps it. If the marker names
   another namespace, or is a symlink, it stops.
2. It creates the config folder with mode 0700 if it is missing.
3. It creates `<ns>.yaml` with mode 0600 from a template of comments. It never overwrites a namespace file.

The broker loads the template as an empty namespace. Fill it with `sbx-broker edit`.

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
