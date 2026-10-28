# baoctx

Switch between OpenBao instances from the command line, like `kubectx` does for Kubernetes.

You list your instances in a config file and give each one a name. `baoctx use prod` then sets `BAO_ADDR`, `BAO_NAMESPACE` and `BAO_TOKEN` in your shell. The `VAULT_*` variables are set too. If a context has a username and password instead of a token, baoctx logs in with userpass and uses the token it gets back.

## Install

There are no releases yet, so build it from source. You need Go 1.22 or newer.

```
go build -o baoctx .
```

Copy the binary somewhere on your `PATH`. To build for another platform:

```
GOOS=linux   GOARCH=amd64 go build -o dist/baoctx-linux-amd64 .
GOOS=windows GOARCH=amd64 go build -o dist/baoctx-windows-amd64.exe .
```

## Config

The config file is `~/.config/baoctx/config.json` on Linux and `%AppData%\baoctx\config.json` on Windows. Set `BAOCTX_CONFIG` to use a different path.

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

YAML works too (`config.yaml` or `config.yml` in the same directory). It supports nested maps, plain or quoted strings and `#` comments, but no lists or flow style (`{}`/`[]`). Quote values that start with a special character such as `*` or `!`.

```yaml
contexts:
  prod:
    address: https://bao.example.com:8200
    namespace: team-a
    token: env:PROD_TOKEN
```

If both a JSON and a YAML file exist, they are merged per context and per field, and the YAML value wins where both set the same field. A `BAOCTX_CONFIG` file is used on its own, and a `.yaml`/`.yml` extension selects the YAML parser.

`address` is required. `namespace` is optional. Use either `token` or `username` + `password`, not both.

A value like `env:PROD_TOKEN` is read from that environment variable when you use the context. Prefer it for secrets. Any value written directly in the file is stored as plain text, so on Linux keep the file at `chmod 600`.

## Usage

```
baoctx list                  # show contexts, the active one is marked with *
baoctx current               # print the active context
baoctx use prod              # print the export statements for your shell
baoctx exec staging -- bao status    # run one command with that context
```

`use` only prints. To apply it to your current shell, eval it:

```
eval "$(baoctx use prod)"
```

Or install a small wrapper function so `baoctx use prod` works on its own. Add one of these to your shell config:

```
eval "$(baoctx init bash)"                                 # ~/.bashrc
eval "$(baoctx init zsh)"                                  # ~/.zshrc
baoctx init fish | source                                  # ~/.config/fish/config.fish
baoctx init powershell | Out-String | Invoke-Expression    # $PROFILE
```

Without the wrapper, `use` picks the shell from `$SHELL` on Linux and assumes PowerShell on Windows. Pass `--shell bash|zsh|fish|powershell|cmd` to choose one.

With a username and password, every `use` and `exec` logs in again. Tokens are not cached.

Exit codes: 0 on success, 1 on a runtime error (unknown context, failed login), 2 on a usage or config error. `exec` returns the exit code of the command it ran.

See [SPEC.md](SPEC.md) for details.
