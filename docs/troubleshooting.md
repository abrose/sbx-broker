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

The broker refuses a login with a `FATAL` error and ends the connection. It refuses a bad startup first (the first two
rows). Then the checks run in the order of the rest of the table, and none of the first five of them (checks 1 to 5)
opens a connection to the real server. Checks 6 and 7 come from the attempt to log in there.

| Code | Message | Meaning and fix |
|---|---|---|
| `0A000` | `replication connections are not supported` | The startup had a `replication` parameter |
| `08P01` | `message too long` | A startup over 10,000 bytes or a password over 1,000 bytes. Also what an app DSN with `sslnegotiation=direct` gets: the broker reads the TLS hello as a startup of absurd length. Use `sslmode=disable` (or `prefer`) for the hop to the broker |
| `08P01` | `unsupported frontend protocol` | The client speaks another major protocol version than 3 |
| `28000` | `placeholder of another namespace` | The password holds a placeholder of another namespace. Use this namespace's placeholder. Logged as a `violation` and a `refuse` |
| `3D000` | `database "<db>" is not configured in namespace <ns>` | The database name of the URL is no alias of the `postgres` section. Use an alias |
| `28000` | `user "<user>" may not use database "<alias>"` | The URL's user is not the entry's `user`. Use that user |
| `28P01` | `password is not the placeholder of database "<alias>"` | The password is not exactly the placeholder of the entry's secret (`sbxb_<ns>_<secret>`) |
| `57P03` | `secret not loaded: <secret> (<state>)` | The secret is still fetching or failed. Check `sbx-broker status`, fix the command, then `sbx-broker refresh <ns> <secret>` |
| `08006` | `upstream unreachable: database "<alias>"` | The real server did not answer, or its TLS failed: a tunnel is down, or the `upstream` is wrong |
| `08006` | `upstream unreachable: database "<alias>"` | The real server must have TLS on (`ssl=on`). The broker always asks for TLS and never falls back to plain text, and the official `postgres` image runs with `ssl=off` by default. Turn TLS on at the server, or every login ends here |
| the server's | `upstream: <its message>` | The real server refused the login (for example a wrong password). Each value in the message is replaced by its placeholder. Check the secret's value |
| `28000` | `upstream: database "<alias>" requires scram-sha-256, the server asked for another method` | `require-auth: scram-sha-256` is set and the server asked for a cleartext password, MD5, or no password at all (trust). Fix the server's login method, or remove the key if you trust the path |
| libpq's `server does not support SSL` | (from the app's driver, not the broker) | The app's own connection to the broker is plain: the broker answers an `SSLRequest` with `N`. A DSN with `sslmode=require` or `verify-full` fails. Use `sslmode=disable` (or `prefer`) for the hop to the broker. The hop to the real server has TLS anyway |

A connection that does not finish its password message within 10 s of its accept (10 s in total, not 10 s of silence)
is closed with no answer.
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
- **The launchd agent does not start:** `install` says `the broker does not answer; see <state>/serve.log`. That file
  holds the agent's stdout and stderr, for example a refused namespace file or `a broker already runs`. `launchctl print
  gui/$(id -u)/io.github.abrose.sbx-broker` shows whether launchd knows the agent and its last exit code.
- **A fetch command is not found under launchd:** the agent has the `PATH` of the shell that ran `install`, not the
  `PATH` of a later shell. Run `sbx-broker install` again from a shell whose `PATH` finds the command
  (`command -v op`).
- **A fetch command works in a terminal but fails as the launchd agent:** the agent gets only `PATH` and the
  `SBX_BROKER_*` variables, no other variable of your shell. A command that needs `GNUPGHOME`, `OP_ACCOUNT`,
  `PASSWORD_STORE_DIR` or `SSH_AUTH_SOCK` gets them through `env`, for example
  `command: ["env", "GNUPGHOME=/Users/<you>/.gnupg", "gpg", "--decrypt", "/Users/<you>/secrets/<name>.gpg"]`.
  `SSH_AUTH_SOCK` changes with each login, so a fixed value breaks after a reboot.
- **`install` refuses: `a broker already runs on <socket>, not as the launchd agent`:** a `serve` runs, for example in
  a terminal tab. Stop it, then run `sbx-broker install` again.
- **`install` fails: `a broker answers, but it is not the launchd agent (the agent has no running process)`:** another
  broker answered on the socket while the agent has no process of its own. Stop the other broker, then run
  `sbx-broker install` again.
- **`install` fails with a `launchctl bootstrap` error:** the plist stays in `~/Library/LaunchAgents`.
  `sbx-broker uninstall` removes it.
- **`the broker does not answer`** (a command, the TUI's header, or the TUI's start): a broker accepted the connection
  on `admin.sock` but sent no answer within 5 s (a `refresh` waits `SBX_BROKER_FETCH_TIMEOUT` plus 5 s). Most often a
  `serve` in a terminal was stopped with `ctrl+z`: run `fg` in that terminal, or stop it. Else the broker hangs: stop it
  (`kill <pid>`; for the launchd agent `sbx-broker uninstall`), then start it again. The running TUI keeps asking every
  2 s and shows the broker again when it answers.
- **`install` refuses: `a broker holds <socket> but does not answer`:** the same, at install: that broker still holds
  the state directory's lock, so the agent's broker could not start. Stop it, then run `sbx-broker install` again.
- **A port did not open:** another process used it. Free it, then run `sbx-broker reload`.
- **A placeholder reached the upstream unchanged:** the body was not scanned. Check the log for `unscanned` and the
  [request limits](how-it-works.md#request-limits).
- **What happened to a request:** `sbx-broker log -f <ns>` shows each swap and refusal with the secret names.
