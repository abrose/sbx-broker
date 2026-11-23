# sbx-broker: a project marker that shows and finds the namespace

Status: approved by the owner on 2026-11-23. A short design (a small change).

## Intent

A namespace file lives on the Mac in `~/.config/sbx-broker/namespaces/`, out of reach of the sandbox. Nothing in the
project folder shows that the project has a namespace, and no command prints where its file is. The owner wants to see
from the project that a namespace exists, and to open its file quickly (owner, 2026-11-23).

## Goals

1. A file in the project root shows that the project uses the broker, and names its namespace.
2. In a project folder, the CLI finds the namespace without an argument.
3. One command opens the namespace file in the owner's editor.
4. One command sets up a new project: the marker and a commented namespace file.
5. The CLI prints the path of each namespace file.

## Non-goals

- A namespace file in the project folder. The sandbox can write the project folder, and a namespace file holds commands
  that the broker runs and upstreams that get real values (owner, 2026-11-23).
- Hiding or shadowing files in the sandbox (an ignore kit). sbx may get a host-side exclude feature
  (docker/sbx-releases#40); a kit is a boundary only if the agent cannot become root. Not now (owner, 2026-11-23).
- A flag to list all namespaces from inside a project folder. Run the command outside a project for that.

## Anti-goals

- A marker that can make the broker load, or the CLI open, a file outside the config folder.
- A marker whose content the CLI prints, so that a marker made a symlink to a Mac file cannot leak that file.
- A change in the behavior of `serve`.

## Approaches

The owner picked approach 1 on 2026-11-23.

1. **A marker file that holds only the namespace name (picked).** The CLI maps the name to a fixed path in the config
   folder. The agent can change the name, but only to another namespace of the same owner: a confusion, not a leak.
2. **A marker file that holds a path.** Rejected: the agent could point it to a file it writes and fill that file with
   commands (owner, 2026-11-23).
3. **A namespace file that names its project folder.** Rejected: the project folder still shows nothing.
4. **The namespace file in the project, with an approval like `direnv allow`.** Rejected: one approval of a diff that
   the owner did not read gives the agent command execution on the Mac.

## Design

### The marker

- The file `.sbx-broker` in the project root. Its first line, with spaces trimmed, is the namespace name. More lines
  are ignored. The CLI reads at most 4 KiB.
- The CLI looks for it in the current folder, then in each parent folder, and stops at the first one it finds. The same
  way as the `.sbx-instance` marker of sbx-instances.
- The CLI refuses a marker that is a symlink or not a regular file (checked with `Lstat`).
- The CLI refuses a name that does not match `^[a-z0-9]{1,16}$`, the namespace name rule.
- An error names the marker's path and the rule, never the marker's content:
  `sbx-broker: <path>: not a valid namespace name`.
- `serve` never reads a marker.
- The marker holds no secret. The owner may commit it or keep it out of git.

### The commands

A command that takes an optional `<ns>` uses the marker when no `<ns>` is given. Outside a project folder it works as
today.

| Command | Change |
|---|---|
| `status [<ns>]` | With no `<ns>` under a marker: only that namespace, with a line `marker: <path>`. Every namespace now shows `file: <path>` |
| `log [-f] [<ns>]` | With no `<ns>` under a marker: only that namespace |
| `refresh [<ns> [<secret>]]` | `<ns>` becomes optional under a marker. To refresh one secret, give both |
| `edit [<ns>]` | New. Opens the namespace file in `$VISUAL`, else `$EDITOR`, else `vi`. Splits the editor setting on spaces (for `code --wait`) and runs it with no shell. Needs no running broker. A missing file: an error that names `sbx-broker init <ns>` |
| `init <ns>` | New. Writes `.sbx-broker` with `<ns>` in the current folder. Creates `<ns>.yaml` from a commented template (mode 0600, the folder 0700) if it does not exist. Never overwrites a namespace file. Refuses when a marker with another name exists in the current folder. Needs no running broker |

A marker that names a namespace with no file: `status` says that the namespace does not exist and names
`sbx-broker init <ns>`.

### The template of `init`

The template is valid YAML that loads with no secrets and no routes: every entry is a comment. It shows one secret with
a `command`, one HTTP route, and the placeholder form, and it links the configuration doc. After `init`, the broker
loads the empty namespace within about 2 s, and `edit` fills it.

### The docs

- README, "Use it in a project": the steps use `sbx-broker init <ns>` and `sbx-broker edit`.
- `docs/cli.md`: the marker, `edit` and `init`.
- `docs/security.md`: why the marker holds only a name.

## Proof

Unit tests of `run` with an injected current folder and environment:

- the marker in the current folder, in a parent folder, none;
- a symlinked marker, a folder named `.sbx-broker`, a name that fails the rule (`../x`, empty, 17 characters): each
  refused, and the error does not hold the content;
- `status`, `log`, `refresh` and `edit` with and without `<ns>`, under a marker and outside one;
- `init`: a new project, a present namespace file (not overwritten), a marker with another name (refused);
- `edit`: the editor setting with arguments, a missing namespace file.

## Left to the builder

- The exact text of the template and of the messages.
- How tests inject the current folder and the editor run.
