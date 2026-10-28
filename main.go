// baoctx switches between OpenBao clusters/instances. See SPEC.md.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const usage = `usage:
  baoctx list
  baoctx current
  baoctx use <name> [--shell bash|zsh|fish|powershell|cmd]
  baoctx exec <name> -- <cmd> [args...]
  baoctx init bash|zsh|fish|powershell`

type Context struct {
	Address   string `json:"address"`
	Namespace string `json:"namespace"`
	Token     string `json:"token"`
	Username  string `json:"username"`
	Password  string `json:"password"`
}

type Config struct {
	Contexts map[string]Context `json:"contexts"`
}

type kv struct{ k, v string }

// usageError marks usage/config errors (exit 2); everything else exits 1.
type usageError struct{ error }

func usageErr(format string, a ...any) error { return usageError{fmt.Errorf(format, a...)} }

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "list":
		err = cmdList(rest, stdout, stderr)
	case "current":
		err = cmdCurrent(rest, stdout)
	case "use":
		err = cmdUse(rest, stdout, stderr)
	case "exec":
		return cmdExec(rest, stdout, stderr)
	case "init":
		err = cmdInit(rest, stdout)
	default:
		err = usageErr("unknown command %q\n%s", cmd, usage)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return 0
}

func fail(w io.Writer, err error) int {
	fmt.Fprintln(w, "baoctx:", err)
	var u usageError
	if errors.As(err, &u) {
		return 2
	}
	return 1
}

// configPaths returns the config files to load, lowest precedence first.
func configPaths() ([]string, error) {
	if p := os.Getenv("BAOCTX_CONFIG"); p != "" {
		return []string{p}, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, usageErr("cannot locate config dir: %v", err)
	}
	var paths []string
	for _, n := range []string{"config.json", "config.yml", "config.yaml"} {
		p := filepath.Join(dir, "baoctx", n)
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
	}
	if paths == nil { // let the read below report the missing file
		paths = []string{filepath.Join(dir, "baoctx", "config.json")}
	}
	return paths, nil
}

func loadConfig(stderr io.Writer) (Config, error) {
	paths, err := configPaths()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Contexts: map[string]Context{}}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return Config{}, usageErr("read config: %v", err)
		}
		if w := permWarning(path); w != "" {
			fmt.Fprintln(stderr, "baoctx: warning:", w)
		}
		c, err := parseConfig(path, b)
		if err != nil {
			return Config{}, usageErr("parse %s: %v", path, err)
		}
		for name, x := range c.Contexts {
			cfg.Contexts[name] = cfg.Contexts[name].overlay(x)
		}
	}
	for name, x := range cfg.Contexts {
		if err := x.validate(); err != nil {
			return Config{}, usageErr("context %q: %v", name, err)
		}
	}
	return cfg, nil
}

// parseConfig decodes a config file, as YAML or JSON depending on its extension.
func parseConfig(path string, b []byte) (c Config, err error) {
	if ext := strings.ToLower(filepath.Ext(path)); ext == ".yaml" || ext == ".yml" {
		tree, err := parseYAML(string(b))
		if err != nil {
			return c, err
		}
		if b, err = json.Marshal(tree); err != nil {
			return c, err
		}
	}
	return c, json.Unmarshal(b, &c)
}

// parseYAML reads the YAML subset configs need: nested maps of string scalars
// (plain, "double" or 'single' quoted) and # comments. Anything else is an error.
// ponytail: no lists, flow style, anchors, block scalars or quoted keys containing ": "; use JSON for those.
func parseYAML(src string) (map[string]any, error) {
	type frame struct {
		keyIndent, indent int // indent of the key that opened the frame / of its own keys (-1: not seen yet)
		m                 map[string]any
		parent            map[string]any
		key               string
	}
	root := map[string]any{}
	stack := []*frame{{keyIndent: -1, indent: -1, m: root}}
	for i, line := range strings.Split(src, "\n") {
		line = strings.TrimRight(stripComment(line), " \t\r")
		if i == 0 {
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		if strings.TrimSpace(line) == "" || line == "---" {
			continue
		}
		body := strings.TrimLeft(line, " ")
		n := len(line) - len(body)
		bad := func(format string, a ...any) error {
			return fmt.Errorf("line %d: %s", i+1, fmt.Sprintf(format, a...))
		}
		if body[0] == '\t' || body == "-" || strings.HasPrefix(body, "- ") {
			return nil, bad("unsupported YAML (tab indent or list)")
		}
		j := strings.Index(body+" ", ": ")
		if j < 0 {
			return nil, bad("want key: value")
		}
		k, v := strings.TrimSpace(body[:j]), strings.TrimSpace(body[j+1:])
		var err error
		if k == "" {
			return nil, bad("empty key")
		}
		if k, err = scalar(k); err != nil {
			return nil, bad("%v", err)
		}
		for len(stack) > 1 && n <= stack[len(stack)-1].keyIndent {
			stack = stack[:len(stack)-1]
		}
		top := stack[len(stack)-1]
		switch {
		case top.indent < 0:
			top.indent = n
			if top.m == nil {
				top.m = map[string]any{}
				top.parent[top.key] = top.m
			}
		case n != top.indent:
			return nil, bad("bad indentation")
		}
		if _, dup := top.m[k]; dup {
			return nil, bad("duplicate key %q", k)
		}
		if v == "" { // empty value, or a map if deeper lines follow
			top.m[k] = ""
			stack = append(stack, &frame{keyIndent: n, indent: -1, parent: top.m, key: k})
			continue
		}
		if top.m[k], err = scalar(v); err != nil {
			return nil, bad("%v", err)
		}
	}
	return root, nil
}

// stripComment cuts a trailing `# comment` that is not inside quotes.
func stripComment(s string) string {
	var inS, inD bool
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case inD && c == '\\':
			i++
		case c == '"' && !inS:
			inD = !inD
		case c == '\'' && !inD:
			inS = !inS
		case c == '#' && !inS && !inD && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

// scalar decodes a YAML string scalar.
func scalar(v string) (string, error) {
	switch {
	case v == "~" || v == "null":
		return "", nil
	case v[0] == '"':
		return strconv.Unquote(v)
	case v[0] == '\'':
		if len(v) < 2 || v[len(v)-1] != '\'' {
			return "", fmt.Errorf("unterminated quote in %s", v)
		}
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'"), nil
	case strings.ContainsRune("{[|>&*!%@`", rune(v[0])):
		return "", fmt.Errorf("unsupported YAML %q (quote the value?)", v)
	}
	return v, nil
}

func (c Context) validate() error {
	switch {
	case c.Address == "":
		return errors.New("address is required")
	case c.Token != "" && (c.Username != "" || c.Password != ""):
		return errors.New("use either token or username+password, not both")
	case c.Token == "" && c.Username == "" && c.Password == "":
		return errors.New("token or username+password is required")
	case c.Token == "" && (c.Username == "" || c.Password == ""):
		return errors.New("username and password must be set together")
	}
	return nil
}

func (c *Context) fields() []*string {
	return []*string{&c.Address, &c.Namespace, &c.Token, &c.Username, &c.Password}
}

// overlay returns c with every non-empty field of o taking precedence.
func (c Context) overlay(o Context) Context {
	for i, p := range o.fields() {
		if *p != "" {
			*c.fields()[i] = *p
		}
	}
	return c
}

// resolve replaces every `env:VAR` value with the value of $VAR.
func (c Context) resolve() (Context, error) {
	for _, p := range c.fields() {
		if name, ok := strings.CutPrefix(*p, "env:"); ok {
			if *p = os.Getenv(name); *p == "" {
				return c, usageErr("environment variable %s is not set", name)
			}
		}
	}
	return c, nil
}

// login performs a userpass login and returns the client token.
func login(c Context) (string, error) {
	body, _ := json.Marshal(map[string]string{"password": c.Password})
	req, err := http.NewRequest("POST", strings.TrimRight(c.Address, "/")+"/v1/auth/userpass/login/"+url.PathEscape(c.Username), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", c.Namespace)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("login failed: HTTP %d", resp.StatusCode) // body may be sensitive
	}
	var r struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil || r.Auth.ClientToken == "" {
		return "", errors.New("login: no auth.client_token in response")
	}
	return r.Auth.ClientToken, nil
}

// contextEnv loads the config and returns the variables to set and unset for the named context.
func contextEnv(name string, stderr io.Writer) (set []kv, unset []string, err error) {
	cfg, err := loadConfig(stderr)
	if err != nil {
		return nil, nil, err
	}
	c, ok := cfg.Contexts[name]
	if !ok {
		return nil, nil, fmt.Errorf("unknown context %q", name)
	}
	if c, err = c.resolve(); err != nil {
		return nil, nil, err
	}
	tok := c.Token
	if tok == "" {
		if tok, err = login(c); err != nil {
			return nil, nil, err
		}
	}
	set = []kv{{"BAO_ADDR", c.Address}, {"VAULT_ADDR", c.Address}}
	if c.Namespace != "" {
		set = append(set, kv{"BAO_NAMESPACE", c.Namespace}, kv{"VAULT_NAMESPACE", c.Namespace})
	} else {
		unset = []string{"BAO_NAMESPACE", "VAULT_NAMESPACE"}
	}
	return append(set, kv{"BAO_TOKEN", tok}, kv{"VAULT_TOKEN", tok}, kv{"BAOCTX_CURRENT", name}), unset, nil
}

var shells = []string{"bash", "zsh", "fish", "powershell", "cmd"}

func defaultShell() string {
	if runtime.GOOS == "windows" {
		return "powershell" // cmd needs --shell cmd
	}
	switch s := filepath.Base(os.Getenv("SHELL")); {
	case s == "pwsh":
		return "powershell"
	case slices.Contains(shells, s):
		return s
	}
	return "bash"
}

func quote(shell, v string) (string, error) {
	switch shell {
	case "bash", "zsh":
		return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'", nil
	case "fish":
		return "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(v) + "'", nil
	case "powershell": // PowerShell also treats the typographic quotes as string delimiters
		return "'" + strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’", "‚", "‚‚", "‛", "‛‛").Replace(v) + "'", nil
	case "cmd": // used inside set "K=..."
		if strings.ContainsAny(v, "\"\r\n") {
			return "", errors.New("value contains a quote or newline, which cannot be quoted for cmd")
		}
		return strings.ReplaceAll(v, "%", "%%"), nil
	}
	return "", usageErr("unknown shell %q (want %s)", shell, strings.Join(shells, ", "))
}

func format(shell string, set []kv, unset []string) (string, error) {
	eol := "\n"
	if shell == "cmd" {
		eol = "\r\n"
	}
	var sb strings.Builder
	for _, k := range unset {
		switch shell {
		case "fish":
			fmt.Fprintf(&sb, "set -e %s", k)
		case "powershell":
			fmt.Fprintf(&sb, "Remove-Item Env:%s -ErrorAction SilentlyContinue", k)
		case "cmd":
			fmt.Fprintf(&sb, `set "%s="`, k)
		default:
			fmt.Fprintf(&sb, "unset %s", k)
		}
		sb.WriteString(eol)
	}
	for _, e := range set {
		q, err := quote(shell, e.v)
		if err != nil {
			return "", err
		}
		switch shell {
		case "fish":
			fmt.Fprintf(&sb, "set -gx %s %s", e.k, q)
		case "powershell":
			fmt.Fprintf(&sb, "$env:%s=%s", e.k, q)
		case "cmd":
			fmt.Fprintf(&sb, `set "%s=%s"`, e.k, q)
		default:
			fmt.Fprintf(&sb, "export %s=%s", e.k, q)
		}
		sb.WriteString(eol)
	}
	return sb.String(), nil
}

func cmdList(args []string, stdout, stderr io.Writer) error {
	if len(args) != 0 {
		return usageErr("list takes no arguments")
	}
	cfg, err := loadConfig(stderr)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(cfg.Contexts))
	for n := range cfg.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	cur := os.Getenv("BAOCTX_CURRENT")
	for _, n := range names {
		mark := "  "
		if n == cur {
			mark = "* "
		}
		fmt.Fprintln(stdout, mark+n)
	}
	return nil
}

func cmdCurrent(args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return usageErr("current takes no arguments")
	}
	cur := os.Getenv("BAOCTX_CURRENT")
	if cur == "" {
		return errors.New("no active context")
	}
	fmt.Fprintln(stdout, cur)
	return nil
}

func cmdUse(args []string, stdout, stderr io.Writer) error {
	name, shell := "", defaultShell()
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--shell":
			if i++; i >= len(args) {
				return usageErr("--shell needs a value")
			}
			shell = args[i]
		case strings.HasPrefix(a, "--shell="):
			shell = strings.TrimPrefix(a, "--shell=")
		case strings.HasPrefix(a, "-"):
			return usageErr("unknown flag %q", a)
		case name == "":
			name = a
		default:
			return usageErr("unexpected argument %q", a)
		}
	}
	if name == "" {
		return usageErr("use needs a context name\n%s", usage)
	}
	if !slices.Contains(shells, shell) {
		return usageErr("unknown shell %q (want %s)", shell, strings.Join(shells, ", "))
	}
	set, unset, err := contextEnv(name, stderr)
	if err != nil {
		return err
	}
	out, err := format(shell, set, unset)
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, out)
	return err
}

func cmdExec(args []string, stdout, stderr io.Writer) int {
	if len(args) < 3 || args[1] != "--" {
		return fail(stderr, usageErr("usage: baoctx exec <name> -- <cmd> [args...]"))
	}
	set, unset, err := contextEnv(args[0], stderr)
	if err != nil {
		return fail(stderr, err)
	}
	drop := unset
	for _, e := range set {
		drop = append(drop, e.k)
	}
	var env []string
	for _, e := range os.Environ() {
		if k, _, _ := strings.Cut(e, "="); !slices.Contains(drop, k) {
			env = append(env, e)
		}
	}
	for _, e := range set {
		env = append(env, e.k+"="+e.v)
	}
	cmd := exec.Command(args[2], args[3:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	// Ctrl-C reaches the child too; stay alive so we can return its exit code.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
		return fail(stderr, err)
	}
	return 0
}

const wrapperPosix = `baoctx() {
  if [ "$1" = use ]; then
    local out
    out=$(command baoctx "$@" --shell %s) || return $?
    eval "$out"
  else
    command baoctx "$@"
  fi
}
`

const wrapperFish = `function baoctx
    if test "$argv[1]" = use
        set -l out (command baoctx $argv --shell fish)
        or return $status
        printf '%s\n' $out | source
    else
        command baoctx $argv
    end
end
`

const wrapperPowershell = `function baoctx {
    $bin = Get-Command baoctx -CommandType Application | Select-Object -First 1
    if ($args.Count -gt 0 -and $args[0] -eq 'use') {
        $out = & $bin @args --shell powershell
        if ($LASTEXITCODE -ne 0) { return }
        Invoke-Expression ($out | Out-String)
    } else {
        & $bin @args
    }
}
`

func cmdInit(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return usageErr("init needs a shell: bash, zsh, fish or powershell")
	}
	switch args[0] {
	case "bash", "zsh":
		fmt.Fprintf(stdout, wrapperPosix, args[0])
	case "fish":
		io.WriteString(stdout, wrapperFish)
	case "powershell":
		io.WriteString(stdout, wrapperPowershell)
	default:
		return usageErr("init: unsupported shell %q (want bash, zsh, fish or powershell)", args[0])
	}
	return nil
}
