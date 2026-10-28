# baoctx — Specification

`baoctx` is a tiny CLI for switching between OpenBao clusters/instances, in the spirit of `kubectx`. It reads named contexts from a config file and either prints environment exports for your shell (`use`) or runs one command with that environment (`exec`).

## Goals

- Very simple and lightweight: one static binary, Go stdlib only, no runtime.
- Works on Linux and Windows (bash, zsh, fish, PowerShell, cmd).
- Contexts configured with address, namespace, and either a token or username + password.
- `VAULT_*` compatibility variables.

## Non-goals (v1)

- Managing secrets, token caching/renewal, OS keychain integration.
- TLS/CA configuration (use the standard `BAO_CACERT` etc. in your environment).
- `add` / `rm` subcommands (edit the config file by hand).
- Interactive picker / TUI.

## Config

JSON and/or YAML, stdlib only. Location:

- `BAOCTX_CONFIG` if set: that single file, no merging. A `.yaml`/`.yml` extension selects the YAML parser, anything else JSON.
- Otherwise every one of `config.json`, `config.yml`, `config.yaml` that exists in `os.UserConfigDir()/baoctx/`
  (`~/.config/baoctx/` on Linux, `%AppData%\baoctx\` on Windows), merged in that order (later wins). If none exist, that is a config error naming `config.json`.

Merging is per context and per field: contexts from all files are combined; for a context present in several files, every non-empty field of the later file overrides the earlier one (so YAML wins over JSON). Validation runs on the merged result, e.g. a JSON `token` plus a YAML `username`/`password` for the same context is a config error. An empty value cannot unset a field from another file.

YAML is a subset: nested maps, plain / `"double"` / `'single'` quoted string scalars, `#` comments, `---`. Lists, flow style (`{}`/`[]`), anchors/aliases/tags, block scalars (`|`/`>`), tab indentation, duplicate keys and bad indentation are config errors with the line number. Values starting with one of `{[|>&*!%@` or a backtick must be quoted. `~` and `null` mean empty.

```json
{
  "contexts": {
    "prod": {
      "address": "https://bao.example.com:8200",
      "namespace": "team-a",
      "token": "env:PROD_TOKEN"
    },
    "staging": {
      "address": "https://stg.example.com:8200",
      "username": "alice",
      "password": "s3cret"
    }
  }
}
```

The same config in YAML:

```yaml
contexts:
  prod:
    address: https://bao.example.com:8200
    namespace: team-a
    token: env:PROD_TOKEN
```

| Field       | Required | Notes                                                         |
|-------------|----------|---------------------------------------------------------------|
| `address`   | yes      | Base URL of the OpenBao server.                               |
| `namespace` | no       | Sent as `BAO_NAMESPACE` / `X-Vault-Namespace`.                |
| `token`     | *        | Use this **or** `username` + `password`, exactly one method.  |
| `username`  | *        | Userpass auth; requires `password`.                           |
| `password`  | *        | Userpass auth; requires `username`.                           |

Validated on load; a context violating the table is a config error (exit 2). Any string value of the form `env:VAR` is replaced by the value of environment variable `VAR` at use time (config error, exit 2, if unset).

On Unix, warn on stderr if the file is readable by group/other (looser than `0600`). No check on Windows.

## Commands

| Command | Behaviour |
|---------|-----------|
| `baoctx list` | Context names, one per line, sorted; the active one (`$BAOCTX_CURRENT`) as `* name`, others as `  name`. |
| `baoctx current` | Prints `$BAOCTX_CURRENT` (empty + exit 1 if unset). |
| `baoctx use <name> [--shell S]` | Prints statements setting `BAO_ADDR`, `BAO_NAMESPACE`, `BAO_TOKEN`, `BAOCTX_CURRENT` for shell `S` ∈ `bash`, `zsh`, `fish`, `powershell`, `cmd`. Default: on Windows `powershell` (cmd needs `--shell cmd`); on Unix the `$SHELL` basename (fallback `bash`). Unsets `BAO_NAMESPACE` if the context has none. |
| `baoctx exec <name> -- <cmd> [args...]` | Runs `cmd` with the same variables in its environment; stdio passed through; exits with the child's exit code. |
| `baoctx init <shell>` | Prints a wrapper function for `bash`, `zsh`, `fish` or `powershell` so `baoctx use <name>` applies directly to the current shell (wrapper evals `baoctx use --shell <shell>` output, so it doesn't depend on `$SHELL`; other subcommands pass through). |

Stdout carries only machine-consumable output (`use`, `init`, `list`, `current`); diagnostics and errors go to stderr.

## Auth resolution

Shared by `use` and `exec`:

1. Context has `token` → use it as-is.
2. Context has `username` + `password` → `POST {address}/v1/auth/userpass/login/{username}` with JSON body `{"password": "..."}` and header `X-Vault-Namespace: <namespace>` when set. The token is `auth.client_token` from the response.

No caching: every `use`/`exec` with username/password performs one login. `net/http` with a 10 s timeout. Non-2xx responses are runtime errors (exit 1); the response body is not echoed (may contain sensitive detail), only the status code.

## Environment variables

Set for the target: `BAO_ADDR`, `BAO_NAMESPACE` (when set), `BAO_TOKEN`, `BAOCTX_CURRENT` (context name), plus the `VAULT_ADDR`, `VAULT_NAMESPACE`, `VAULT_TOKEN` compatibility copies with identical values. `BAO_NAMESPACE`/`VAULT_NAMESPACE` are unset when the context has no namespace.
Read by baoctx: `BAOCTX_CONFIG`, `BAOCTX_CURRENT`, `SHELL`, plus any `env:` references.

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Runtime error (unknown context, login failed, network error); `exec` returns the child's code instead |
| 2 | Usage or config error (bad args, missing/invalid config file) |

## Layout and build

- `main.go` — everything (CLI dispatch, config incl. YAML subset parser and merge, login, shell formatting); `main_test.go`.
- `perm_unix.go` / `perm_windows.go` — config file permission check, build-tagged.
- `go.mod` — no third-party dependencies.

```
go build -o baoctx .
GOOS=linux   GOARCH=amd64 go build -o dist/baoctx-linux-amd64 .
GOOS=windows GOARCH=amd64 go build -o dist/baoctx-windows-amd64.exe .
```

## Testing

- Table tests: config validation (missing address, both/neither auth methods, unknown `env:` var) and per-shell export formatting including quoting of special characters.
- `httptest` server for the userpass login path (success, non-2xx, namespace header sent).
- Manual: `eval "$(baoctx use prod)"` then `bao status`; `baoctx exec staging -- bao status`.

## Security notes

- Credentials are stored in plaintext in the config file: `0600` on Unix, per-user `%AppData%` ACLs on Windows. Prefer `token` with `env:` references over literal passwords.
- `use` output contains `BAO_TOKEN`; do not log or share it. Nothing else prints secrets.
- Shell output is quoted per shell to prevent injection through config values. For `cmd` (`set "X=..."`), `%` is emitted as `%%`; values containing `"`, CR or LF cannot be quoted safely and make `use --shell cmd` fail (exit 1).
