# Security

## What the broker protects

- The sandbox holds only placeholders. The broker swaps them on the way out and scrubs the real values from every
  answer, its headers and its errors. The [known gaps](#known-gaps) below are the exceptions.
- A value never goes to disk or into a log. The log and `status` hold secret names, never values.
- The namespaces stay apart: a request may use only the secrets of the namespace its port belongs to.
- The admin interface is a Unix socket of your user, out of reach of a sandbox.
- The namespace files stay on the Mac, out of reach of a sandbox (see [the project marker](#the-project-marker)).

## The project marker

A sandbox can write its project folder. A namespace file holds commands that the broker runs as your user, and the
upstreams that get the real values. In the project folder, a namespace file would let the agent run code on the Mac,
use any secret of your store, or send a value to a host of its choice. A marker that held a path would do the same:
the agent could point it at a file that it writes.

So the marker `.sbx-broker` holds only a namespace name:

- `serve` never reads a marker.
- The CLI maps the name to `<config folder>/<name>.yaml`, and it accepts only `^[a-z0-9]{1,16}$`, so the path stays in
  the config folder.
- The CLI refuses a marker that is a symlink or not a regular file, also when the agent swaps it during the read.
- No error prints the marker's content, and every path from the project folder that the CLI prints is escaped, so a
  folder name cannot inject terminal escapes.
- `sbx-broker edit` runs your editor in the config folder, not in the project folder. Some editors load config from
  their working folder, and that config can run commands; the agent could plant it in the project folder.

The agent can still change the name to another namespace of yours. Then `status` shows that namespace (names,
placeholders and states, never values), and `edit` opens that file, which your editor shows by its path.

## What it does not do

- It does not choose the rights of a credential. It passes on the credentials you configure; choosing a role with the
  right rights is your job.
- It has no secret store of its own. The values stay where you keep them.
- It does not speak TLS to the sandbox. The hop from the sandbox to the broker is plain HTTP, or a plain Postgres
  connection, on the Mac.
- It does not start tunnels or port-forwards, and it does not watch upstreams. It only forwards to their ports.
- It does not support WebSockets or other protocol upgrades.
- It is made and tested for macOS, where it runs as your user. It does not run in Docker.

## Postgres

- The hop from the sandbox to the broker's Postgres port is plain, like the HTTP hop: the app uses `sslmode=disable`,
  and the broker answers an `SSLRequest` with `N`.
- The broker swaps and scrubs only the login. After the hand-off it copies bytes and does not read them, so a query
  can use the database as the credential allows, and a result that holds a value is not scrubbed.
- The app gets the real server's cancel key. It works only through the broker, which matches it against its open
  sessions and forwards it. The broker sends that cancel request to the real server over plain TCP, because the request
  carries only a process ID and a key, no value of a namespace.
- `pgconn` takes the password as a Go string, so a copy of the real password stays in the broker's memory until the
  garbage collector frees it. It cannot be wiped, and nothing writes it anywhere. The broker wipes the byte copy it
  gets from the store as soon as the login call returns.
- The broker's TLS to the real server encrypts but checks no certificate. A fake server on the path could therefore ask
  for a cleartext password and read it. `require-auth: scram-sha-256` closes this: the login ends before the password
  leaves the broker, unless the server speaks SCRAM.
- Channel binding stays `prefer`. An active attacker between the Mac and the real server can still relay a SCRAM
  login. This is a known limit: the broker's goal is to keep values out of the sandbox, not to secure the Mac's path
  to the database.
- `serve` removes every `PG*` variable from its own process when it starts, so no `PGSERVICE`, `PGSSLMODE` or
  `PGOPTIONS` of the terminal changes an upstream login. The fetch commands keep the environment `serve` started
  with.
- Replication connections are refused.

## Known gaps

These are accepted in the design:

- An upstream that transforms a value before it echoes it (hashes, splits or re-encodes it other than the
  [covered forms](how-it-works.md#the-forms-the-scrub-covers)) defeats the scrub.
- The scrub knows only the values this broker process has seen. After a broker start or restart, before the first
  successful fetch of a secret, the broker has never seen its value, so a copy an upstream stored earlier reaches the
  app unscrubbed. The same holds for a value retired before the namespace file was removed or the broker stopped:
  the broker forgets it then.
- A body the broker does not scan reaches the upstream as is (see the
  [request limits](how-it-works.md#request-limits)).
- The hop from the sandbox to the broker is plain HTTP on the Mac. Any process on the Mac can connect to a listener.
- The broker runs as your user, not isolated.
- A sandbox can use every secret of its namespace against the upstreams of its namespace. That is the purpose; it
  cannot read a value.
- The broker cannot tell sandboxes apart: all requests come from the Mac itself. The port decides the namespace, so
  sandboxes of one project share its secrets.
