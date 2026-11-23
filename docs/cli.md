# The CLI

```text
sbx-broker                             the terminal UI (see the TUI below)
sbx-broker tui                         the same
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
- The TUI starts on the secrets of the marker's namespace, or on the list of namespaces with no marker.
- The CLI refuses a marker that is a symlink or not a regular file. An error names the marker's path, never its
  content.
- `serve` never reads a marker. Why the marker holds only a name: see [security.md](security.md#the-project-marker).

## The admin socket

- The CLI talks to `serve` over the Unix socket `admin.sock` (mode 0600, in a directory of mode 0700). There is no
  admin TCP port, and a sandbox cannot reach the socket.
- One broker per state directory: `serve` holds a lock on `admin.lock`, and a second `serve` refuses with
  `a broker already runs`.

## The TUI

`sbx-broker` with no command, or `sbx-broker tui`, starts the terminal UI. It shows what the broker loaded, asks the
broker for its status every 2 s, and never starts a broker. When no broker answers at start, it prints this line to
stderr and exits 1:

```text
no broker runs (start it with: sbx-broker serve, or as a service with: sbx-broker install)
```

When the broker goes away later, the header shows the same line and the TUI keeps asking.

| View | Columns or fields |
|---|---|
| Namespaces | `NAME`, `LISTENERS`, `SECRETS` (`<ok> of <n> ok`), `ERROR` |
| Secrets | `NAME`, `PLACEHOLDER`, `COMMAND` (cut to fit with `…`), `STATE` (`fetching` while a refresh runs), `FETCHED`, `SWAPS` |
| Inspect | name, placeholder, the whole command, state, fetch time, exit code and error line of a failed fetch, length, `sha256 <first 8 hex>`, swaps, and `value: hidden (r to reveal)` |
| Routes | `HTTP`: `NAME`, `LISTEN`, `UPSTREAM`, `TRANSFORMS`; `POSTGRES <listen>`: `ALIAS`, `UPSTREAM`, `DBNAME`, `USER`, `PASSWORD` (a secret name), `REQUIRE-AUTH` |
| Log | the header `log · ns <filter> · kind <filter>`, then the newest 1,000 events, newest last, one line each, the same line `sbx-broker log` prints |

The Log view follows the broker's log from the start of the TUI, also while another view shows. When the broker goes away
the log stays on screen; when it answers again the TUI follows again and adds only the events it has not got.

`SWAPS` counts the `swap` events that name the secret since the broker started. A command shows as shell words: an
argument with a character other than `A-Za-z0-9_@%+=:,./-` is in single quotes.

| Where | Key | Action |
|---|---|---|
| all | `1` `2` `3` `4`, `tab` | Namespaces, Secrets, Routes, Log; next view |
| all | `q`, `ctrl+c` | quit |
| lists | `up`/`k`, `down`/`j` | move |
| Namespaces | `enter` | select the namespace, open Secrets |
| Secrets | `enter` | Inspect |
| Secrets | `f`, `F` | refresh the secret, refresh all of the namespace |
| Secrets | `a`, `e` | add a secret, edit the command of the secret |
| Routes | `a`, `p`, `e` | add an HTTP route, add a database, edit the route or database |
| Secrets, Routes | `d`, then `y` | remove the secret, route or database; any other key after `d` cancels |
| Secrets, Routes | `o` | open the namespace file in `$VISUAL`, else `$EDITOR`, else `vi` (in the config folder, as `edit` does) |
| a form | `tab`, `shift+tab` | next field, previous field |
| a form | `enter` on the last field, `ctrl+s` | save |
| a form | `esc` | cancel |
| Inspect | `r`, then `y` | reveal; any other key after `r` cancels |
| Inspect | `esc` | back to Secrets |
| Log | `n` | next namespace filter: `all`, then each namespace in order |
| Log | `t` | next kind filter: `all`, then each kind (`swap`, `refuse`, `fetch`, `reload`, `violation`, `unscanned`, `upstream`, `error`, `info`, `close`, `reveal`) |
| Log | `c` | clear both filters |
| Log | `up`/`k`, `down`/`j` | scroll back, scroll forward |
| Log | `G` | back to the newest |

### Edit forms

The forms change the namespace file of the namespace on screen. The fields, in order:

| Form | Fields |
|---|---|
| secret | `name`, `command` (shell words: spaces split, quotes group, a backslash escapes; no expansion, no shell) |
| HTTP route | `name`, `listen`, `upstream` |
| database | `alias`, `upstream`, `dbname`, `user`, `password secret`, `require-auth` (`scram-sha-256` or empty), `postgres listen` (filled with the current one) |

A name or alias is fixed once it exists: an app's config holds the placeholder made from it. Transforms are not in a
form; `o` opens the file for them. While a form is open its fields take every key as text, also `q` and the digits;
`ctrl+c` still quits.

A save checks the new file as `serve` would load it, also its ports against the other namespace files, and writes
nothing on an error: the error shows under the form, which keeps its input. When the file changed on disk after the
form opened, the save writes nothing, the form closes and the TUI shows
`the namespace file changed on disk; the TUI read it again, make the change again`. A save that works replaces the file
in one step and sends `reload`.

### Reveal

`r` asks `Reveal the value of <name>? The broker logs it. (y/n)`. With `y` the broker sends the value, only of a secret
in state `ok`, and logs a `reveal` event. The TUI shows `value: <value>   (any key hides it)` and holds the value only
while it shows it: it overwrites the value with zeros and drops it at the next key, at a view switch, 30 s after it
appeared, and when the TUI quits. A status update does not hide it. The TUI runs in the terminal's alternate screen, so
no value stays in the scrollback. No CLI command reveals a value.

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
- `log`, `status` and the TUI print control characters escaped, so a sandboxed app cannot inject terminal escapes or fake lines
  through a path, a method or an upstream message.

| Kind | Meaning |
|---|---|
| `swap` | a request went to the upstream, or a Postgres login reached the real server; names the secrets it used |
| `refuse` | the broker refused a request with a 503 or a 403, or refused a Postgres login |
| `violation` | a placeholder of another namespace; comes with the `refuse` event of the 403 |
| `unscanned` | a body the broker did not scan (see [how-it-works.md](how-it-works.md#request-limits)) |
| `upstream` | the upstream failed or answered in a way the broker refuses (a 502, or a failed Postgres login) |
| `fetch` | a fetch command ended, with the secret's new state |
| `reload` | a namespace was loaded, reloaded or stopped |
| `error` | a request that could not be scanned (a 400), a namespace file with an error, a listener that did not start |
| `close` | a Postgres session ended, with its duration |
| `reveal` | the owner showed a secret's value in the TUI; names the secret |
| `info` | other notes |

A Postgres event shows `<user>@<alias>` in the route column, for example `app@app`.
