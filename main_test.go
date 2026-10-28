package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		c    Context
		want string // substring of error, "" = valid
	}{
		{"token", Context{Address: "a", Token: "t"}, ""},
		{"userpass", Context{Address: "a", Username: "u", Password: "p"}, ""},
		{"no address", Context{Token: "t"}, "address"},
		{"neither", Context{Address: "a"}, "required"},
		{"both", Context{Address: "a", Token: "t", Username: "u", Password: "p"}, "not both"},
		{"token+username", Context{Address: "a", Token: "t", Username: "u"}, "not both"},
		{"user only", Context{Address: "a", Username: "u"}, "together"},
		{"password only", Context{Address: "a", Password: "p"}, "together"},
		{"ldap", Context{Address: "a", Username: "u", Password: "p", Method: "ldap", Mount: "corp/ldap"}, ""},
		{"env method", Context{Address: "a", Username: "u", Password: "p", Method: "env:M"}, ""},
		{"unknown method", Context{Address: "a", Username: "u", Password: "p", Method: "approle"}, "unknown method"},
		{"token+method", Context{Address: "a", Token: "t", Method: "ldap"}, "only apply"},
		{"token+mount", Context{Address: "a", Token: "t", Mount: "m"}, "only apply"},
	}
	for _, tt := range tests {
		err := tt.c.validate()
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%s: got %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestResolve(t *testing.T) {
	t.Setenv("BAOCTX_T1", "secret")
	t.Setenv("BAOCTX_T2", "")
	c, err := Context{Address: "a", Token: "env:BAOCTX_T1"}.resolve()
	if err != nil || c.Token != "secret" {
		t.Errorf("got %q, %v", c.Token, err)
	}
	for _, v := range []string{"env:BAOCTX_T2", "env:BAOCTX_NOPE"} {
		_, err := Context{Address: "a", Token: v}.resolve()
		var u usageError
		if !errors.As(err, &u) {
			t.Errorf("%s: want usage error, got %v", v, err)
		}
	}
}

func TestFormat(t *testing.T) {
	const hostile = `a'b "c" $x %y \z`
	tests := []struct{ shell, want string }{
		{"bash", `export K='a'\''b "c" $x %y \z'` + "\n"},
		{"zsh", `export K='a'\''b "c" $x %y \z'` + "\n"},
		{"fish", `set -gx K 'a\'b "c" $x %y \\z'` + "\n"},
		{"powershell", `$env:K='a''b "c" $x %y \z'` + "\n"},
	}
	for _, tt := range tests {
		got, err := format(tt.shell, []kv{{"K", hostile}}, nil)
		if err != nil || got != tt.want {
			t.Errorf("%s: got %q, %v; want %q", tt.shell, got, err, tt.want)
		}
	}

	if got, _ := format("bash", []kv{{"K", "a\nb"}}, nil); got != "export K='a\nb'\n" {
		t.Errorf("bash newline: %q", got)
	}
	if got, _ := format("powershell", []kv{{"K", "a’b‘c"}}, nil); got != "$env:K='a’’b‘‘c'\n" {
		t.Errorf("powershell smart quotes: %q", got)
	}

	got, err := format("cmd", []kv{{"K", `a b $x %y 'q' \z`}}, nil)
	if want := "set \"K=a b $x %%y 'q' \\z\"\r\n"; err != nil || got != want {
		t.Errorf("cmd: got %q, %v; want %q", got, err, want)
	}
	for _, v := range []string{`a"b`, "a\nb", "a\rb"} {
		if _, err := format("cmd", []kv{{"K", v}}, nil); err == nil {
			t.Errorf("cmd: %q should be rejected", v)
		}
	}

	unsets := map[string]string{
		"bash":       "unset N\n",
		"zsh":        "unset N\n",
		"fish":       "set -e N\n",
		"powershell": "Remove-Item Env:N -ErrorAction SilentlyContinue\n",
		"cmd":        "set \"N=\"\r\n",
	}
	for shell, want := range unsets {
		if got, err := format(shell, nil, []string{"N"}); err != nil || got != want {
			t.Errorf("%s unset: got %q, %v; want %q", shell, got, err, want)
		}
	}
}

// loginServer answers logins on the userpass, ldap and team/ldap mounts for user "a/b" with password "pw".
func loginServer(t *testing.T, ns *string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*ns = r.Header.Get("X-Vault-Namespace")
		var body struct{ Password string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		ok := false
		for _, m := range []string{"userpass", "ldap", "team/ldap"} {
			ok = ok || r.URL.EscapedPath() == "/v1/auth/"+m+"/login/a%2Fb"
		}
		if r.Method != "POST" || !ok || body.Password != "pw" {
			w.WriteHeader(403)
			fmt.Fprint(w, "SECRET-DETAIL")
			return
		}
		fmt.Fprint(w, `{"auth":{"client_token":"tok-123"}}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestLogin(t *testing.T) {
	var ns string
	s := loginServer(t, &ns)

	tok, err := login(Context{Address: s.URL + "/", Namespace: "team-a", Username: "a/b", Password: "pw"})
	if err != nil || tok != "tok-123" || ns != "team-a" {
		t.Errorf("got %q, %v, ns %q", tok, err, ns)
	}

	_, err = login(Context{Address: s.URL, Username: "a/b", Password: "wrong"})
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "SECRET-DETAIL") {
		t.Errorf("non-2xx: got %v", err)
	}
	if ns != "" {
		t.Errorf("namespace header sent without namespace: %q", ns)
	}

	for _, c := range []Context{{Method: "ldap"}, {Mount: "/team/ldap/"}, {Method: "okta", Mount: "ldap"}} {
		c.Address, c.Username, c.Password = s.URL, "a/b", "pw"
		if tok, err := login(c); err != nil || tok != "tok-123" {
			t.Errorf("%+v: got %q, %v", c, tok, err)
		}
	}
	// okta defaults to the okta mount, which the server doesn't have
	if _, err := login(Context{Address: s.URL, Username: "a/b", Password: "pw", Method: "okta"}); err == nil {
		t.Error("okta default mount should miss")
	}
	var u usageError
	if _, err := login(Context{Address: s.URL, Username: "a/b", Password: "pw", Method: "bogus"}); !errors.As(err, &u) {
		t.Errorf("bad method: want usage error, got %v", err)
	}
}

func writeConfig(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BAOCTX_CONFIG", p)
}

func runCmd(args ...string) (code int, out, errOut string) {
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

func TestRun(t *testing.T) {
	var ns string
	s := loginServer(t, &ns)
	t.Setenv("BAOCTX_TOK", "tok-env")
	writeConfig(t, fmt.Sprintf(`{"contexts":{
		"prod":    {"address":"https://bao.example.com:8200","namespace":"team-a","token":"env:BAOCTX_TOK"},
		"staging": {"address":%q,"username":"a/b","password":"pw"}
	}}`, s.URL))

	code, out, _ := runCmd("use", "prod", "--shell", "bash")
	want := "export BAO_ADDR='https://bao.example.com:8200'\nexport VAULT_ADDR='https://bao.example.com:8200'\n" +
		"export BAO_NAMESPACE='team-a'\nexport VAULT_NAMESPACE='team-a'\n" +
		"export BAO_TOKEN='tok-env'\nexport VAULT_TOKEN='tok-env'\nexport BAOCTX_CURRENT='prod'\n"
	if code != 0 || out != want {
		t.Errorf("use prod: code %d, out %q", code, out)
	}

	code, out, _ = runCmd("use", "--shell=fish", "staging")
	if code != 0 || !strings.HasPrefix(out, "set -e BAO_NAMESPACE\nset -e VAULT_NAMESPACE\n") || !strings.Contains(out, "set -gx BAO_TOKEN 'tok-123'\n") {
		t.Errorf("use staging: code %d, out %q", code, out)
	}

	t.Setenv("BAOCTX_CURRENT", "staging")
	if code, out, _ = runCmd("list"); code != 0 || out != "  prod\n* staging\n" {
		t.Errorf("list: code %d, out %q", code, out)
	}
	if code, out, _ = runCmd("current"); code != 0 || out != "staging\n" {
		t.Errorf("current: code %d, out %q", code, out)
	}
	t.Setenv("BAOCTX_CURRENT", "")
	if code, out, _ = runCmd("current"); code != 1 || out != "" {
		t.Errorf("current unset: code %d, out %q", code, out)
	}

	exitCodes := []struct {
		want int
		args []string
	}{
		{1, []string{"use", "nope", "--shell", "bash"}},
		{2, nil},
		{2, []string{"bogus"}},
		{2, []string{"use"}},
		{2, []string{"use", "prod", "--shell", "tcsh"}},
		{2, []string{"use", "prod", "--bogus"}},
		{2, []string{"exec", "prod", "echo"}},
		{2, []string{"init", "tcsh"}},
	}
	for _, tt := range exitCodes {
		if code, out, _ := runCmd(tt.args...); code != tt.want || out != "" {
			t.Errorf("%v: code %d, out %q; want %d", tt.args, code, out, tt.want)
		}
	}
}

func TestRunConfigErrors(t *testing.T) {
	t.Setenv("BAOCTX_NOPE", "")
	for name, cfg := range map[string]string{
		"bad json":    `{`,
		"no address":  `{"contexts":{"x":{"token":"t"}}}`,
		"both auth":   `{"contexts":{"x":{"address":"a","token":"t","username":"u","password":"p"}}}`,
		"unset env":   `{"contexts":{"x":{"address":"a","token":"env:BAOCTX_NOPE"}}}`,
		"empty creds": `{"contexts":{"x":{"address":"a"}}}`,
	} {
		writeConfig(t, cfg)
		if code, out, _ := runCmd("use", "x", "--shell", "bash"); code != 2 || out != "" {
			t.Errorf("%s: code %d, out %q; want 2", name, code, out)
		}
	}
	t.Setenv("BAOCTX_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	if code, _, _ := runCmd("list"); code != 2 {
		t.Errorf("missing config: code %d, want 2", code)
	}
}

func TestExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	t.Setenv("BAO_NAMESPACE", "stale")
	writeConfig(t, `{"contexts":{"p":{"address":"http://x","token":"tok"}}}`)

	code, out, _ := runCmd("exec", "p", "--", "sh", "-c", `printf '%s|%s|%s|%s' "$BAO_TOKEN" "$VAULT_ADDR" "$BAOCTX_CURRENT" "${BAO_NAMESPACE-unset}"`)
	if code != 0 || out != "tok|http://x|p|unset" {
		t.Errorf("env: code %d, out %q", code, out)
	}
	if code, _, _ = runCmd("exec", "p", "--", "sh", "-c", "exit 7"); code != 7 {
		t.Errorf("exit code: got %d, want 7", code)
	}
	if code, _, _ = runCmd("exec", "p", "--", "baoctx-no-such-binary"); code != 1 {
		t.Errorf("missing binary: got %d, want 1", code)
	}
}

func TestPermWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no permission check on Windows")
	}
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, nil, 0o644)
	if permWarning(p) == "" {
		t.Error("0644 should warn")
	}
	os.Chmod(p, 0o600)
	if w := permWarning(p); w != "" {
		t.Errorf("0600 should not warn: %s", w)
	}
}

func TestParseYAML(t *testing.T) {
	ok := `# comment
---
contexts:
  prod:            # trailing comment
    address: https://bao.example.com:8200
    namespace:
    token: "a # b: c\"d"
  stg:
    address: 'it''s # fine'
    password: p#w
`
	tree, err := parseYAML(ok)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(tree)
	want := `{"contexts":{"prod":{"address":"https://bao.example.com:8200","namespace":"","token":"a # b: c\"d"},"stg":{"address":"it's # fine","password":"p#w"}}}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}

	for name, src := range map[string]string{
		"list":        "contexts:\n  - a\n",
		"flow":        "contexts: {a: b}\n",
		"anchor":      "a: &x 1\n",
		"block":       "a: |\n  x\n",
		"tab":         "a:\n\tb: c\n",
		"duplicate":   "a: 1\na: 2\n",
		"bad indent":  "a: 1\n  b: 2\n",
		"no colon":    "a\n",
		"empty key":   ": x\n",
		"open quote":  "a: 'x\n",
		"bad dquote":  "a: \"x\n",
		"uneven keys": "a:\n  b: 1\n   c: 2\n",
	} {
		if _, err := parseYAML(src); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestConfigMerge(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("AppData", dir)
	t.Setenv("BAOCTX_CONFIG", "")
	files := func(j, y string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "baoctx"), 0o700); err != nil {
			t.Fatal(err)
		}
		for n, body := range map[string]string{"config.json": j, "config.yaml": y} {
			p := filepath.Join(dir, "baoctx", n)
			os.Remove(p)
			if body != "" {
				if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	files(`{"contexts":{
		"both":     {"address":"http://json","namespace":"ns","token":"t"},
		"onlyjson": {"address":"http://j","token":"t"}}}`,
		"contexts:\n  both:\n    address: http://yaml\n  onlyyaml:\n    address: http://y\n    token: t\n")
	cfg, err := loadConfig(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Contexts["both"], (Context{Address: "http://yaml", Namespace: "ns", Token: "t"}); got != want {
		t.Errorf("both: got %+v, want %+v", got, want)
	}
	if len(cfg.Contexts) != 3 {
		t.Errorf("want 3 contexts, got %v", cfg.Contexts)
	}

	files("", "contexts:\n  y:\n    address: http://y\n    token: t\n") // YAML alone
	if code, out, _ := runCmd("list"); code != 0 || out != "  y\n" {
		t.Errorf("yaml only: code %d, out %q", code, out)
	}

	files(`{"contexts":{"x":{"address":"a","token":"t"}}}`, "contexts:\n  x:\n    username: u\n    password: p\n")
	if code, _, _ := runCmd("list"); code != 2 {
		t.Errorf("token+password across files: code %d, want 2", code)
	}

	files("", "")
	if code, _, _ := runCmd("list"); code != 2 {
		t.Errorf("no files: code %d, want 2", code)
	}

	p := filepath.Join(dir, "explicit.yml")
	os.WriteFile(p, []byte("contexts:\n  e:\n    address: a\n    token: t\n"), 0o600)
	files(`{"contexts":{"other":{"address":"a","token":"t"}}}`, "")
	t.Setenv("BAOCTX_CONFIG", p)
	if code, out, _ := runCmd("list"); code != 0 || out != "  e\n" {
		t.Errorf("explicit .yml: code %d, out %q", code, out)
	}
}
