# Configuration

## Namespace files

One namespace per project, one file per namespace: `~/.config/sbx-broker/namespaces/<ns>.yaml`. A namespace name
matches `^[a-z0-9]{1,16}$`. The file holds commands, never values.

In the project's root folder, `sbx-broker init <ns>` creates the file from a template, and `sbx-broker edit` opens
it (see [cli.md](cli.md#init)).

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

### Names and ports

- A secret name matches `^[a-z0-9]+(-[a-z0-9]+)*$`, at most 41 characters.
- A route name matches `^[a-z0-9][a-z0-9-]{0,30}$`.
- A listen port is in 1024 to 65535, and two routes never share one.
- Every listener binds `127.0.0.1` only.

### Fetch commands

- A `command` is an argv list. A shell runs only when you write `["sh", "-c", "..."]`.
- It runs as your user, with no stdin and a timeout of `SBX_BROKER_FETCH_TIMEOUT` (default `60s`), so an unlock prompt
  has time.
- Its stdout, with one trailing newline removed, is the value. Its stdout never goes to the log. A failed fetch logs
  only its exit code or a fixed reason; the first stderr line shows only in `sbx-broker status`, because stderr may
  hold a value.
- A value under 8 bytes is refused: the scrub could not tell it from normal text.
- The broker fetches at `serve` start, at a reload of the namespace, and on `sbx-broker refresh`. It never fetches on
  a timer. All secrets of a namespace are fetched at the same time, so one slow command does not hold back the others.
- The value lives in the broker's memory only. The broker locks its memory pages where the OS allows it, and zeroes a
  value it no longer needs.

### Placeholders

The placeholder of a secret is `sbxb_<ns>_<name>`, with each `-` in the name written as `_`. For the file above:

| Secret | Placeholder |
|---|---|
| `client-secret` | `sbxb_acme_client_secret` |
| `api-key` | `sbxb_acme_api_key` |

A placeholder holds only letters, digits and `_`, so it passes URL, form, JSON and Base64 encoding unchanged in
meaning. It is stable, so app config can hold it.

### File ownership

`serve` refuses a namespace file or the namespace directory when your user does not own it, when it is group- or
world-writable, or when it is a symlink: the broker runs the commands in it.

## Postgres (coming)

A namespace file may already hold a `postgres` section. `serve` reads and checks it, logs that Postgres listeners come
in a later version, and opens no Postgres port.

```yaml
postgres:
  listen: 29620
  databases:
    app:
      upstream: 127.0.0.1:5432
      dbname: <db>
      user: <user>
      password: app-db-password        # a secret name of this namespace
```

## Environment variables

| Variable | Default |
|---|---|
| `SBX_BROKER_CONFIG_DIR` | `~/.config/sbx-broker/namespaces` |
| `SBX_BROKER_STATE_DIR` | `~/.local/state/sbx-broker` (`admin.sock`, `admin.lock`, `broker.log`) |
| `SBX_BROKER_FETCH_TIMEOUT` | `60s` (a Go duration) |

## Reloads

- `serve` checks the config directory every 1 s. A change counts once two checks in a row saw it, so a changed, new or
  removed file takes effect within about 2 s, and an editor that truncates before it writes does not cause a reload.
- `sbx-broker reload` acts at once. It also retries files that were refused and namespaces whose listener did not
  start (for example a port that was in use).
- A reload restarts the whole namespace: it closes its listeners, opens them again and fetches all its secrets again.
- A file with an error keeps the old state of that namespace and logs the error.
