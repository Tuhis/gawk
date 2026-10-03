package dashboard

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/oidcauth"
)

// docs/55 D5 / TO3: the built bundle contains no construct the read
// listener's Content-Security-Policy would block.
//
// The policy is oidcauth's (docs/42 §4.8's set, `connect-src` carrying the
// issuer origin), stamped on every read-listener response in every mode. It
// leaves `script-src` to `default-src 'self'`, so NO inline script, NO eval
// and NO string-compiled code can run — and a page that leans on any of them
// does not degrade, it breaks, typically with nothing on screen but a console
// error. That is a bad thing to discover on the reference deployment, which
// is why this is a static check over what ships rather than an assumption.
//
// What the policy DOES allow is asserted too (TestTheCSPThisCheckAssumes), so
// the day it is loosened or tightened this file is re-read rather than
// silently checking the wrong thing.
//
// Inline `style` attributes are allowed (`style-src 'self' 'unsafe-inline'`)
// and the SPA and ECharts' tooltip both use them; `data:` images are allowed
// (`img-src 'self' data:`). Neither is flagged here. ECharts renders to canvas
// and, as built, contains neither `eval` nor `new Function` — the
// tree-shaken `echarts/core` import in ui/src/charts/echarts.ts is what keeps
// it that way, and this test is what notices if a full-bundle import comes
// back.
//
// Named to match CI's `-run 'TestNoExternalAssetReferences|…'` filter in the
// `telemetry-ui` job, the only job that builds the bundle; in the `telemetry`
// job it skips, like the asset tests beside it.

// cspViolations returns the policy-violating constructs in one shipped file.
// Split out from the walk so the patterns themselves can be tested against
// fixtures (TestTheCSPCheckCatchesWhatItClaimsTo): a regex that matched
// nothing would pass every bundle.
func cspViolations(name, body string) []string {
	var out []string
	add := func(what string, re *regexp.Regexp) {
		if m := re.FindString(body); m != "" {
			out = append(out, what+": "+strings.TrimSpace(m))
		}
	}
	switch path.Ext(name) {
	case ".html":
		// A <script> with a body is inline script. Vite emits exactly one
		// module script with a src and no body.
		add("inline <script>", reInlineScript)
		add("inline event handler attribute", reHandlerAttr)
		add("javascript: URL", reJSURLAttr)
		add("off-origin src/href", reAbsoluteRef)
		add("<iframe>/<object>/<embed>", reFrameish)
		add("<base>", reBase)
	case ".js":
		add("eval", reEval)
		add("Function constructor", reFunctionCtor)
		add("string-compiled timer", reStringTimer)
		// worker-src falls back to script-src 'self': a blob: or data: worker
		// is blocked, and nothing here should need a worker at all.
		add("Worker", reWorker)
		// WebAssembly compilation needs 'wasm-unsafe-eval', which the policy
		// does not grant.
		add("WebAssembly", reWasm)
	case ".css":
		// font-src and img-src are 'self' (+ data: for images): any
		// off-origin url() or @import is blocked.
		add("off-origin url()", reCSSRemoteURL)
		add("off-origin @import", reCSSImport)
	}
	return out
}

var (
	reInlineScript = regexp.MustCompile(`(?is)<script\b[^>]*>\s*[^<\s][^<]*</script>`)
	reHandlerAttr  = regexp.MustCompile(`(?i)<[a-z][^>]*\son[a-z]+\s*=`)
	reJSURLAttr    = regexp.MustCompile(`(?i)(?:src|href|action)\s*=\s*["']?\s*javascript:`)
	reAbsoluteRef  = regexp.MustCompile(`(?i)(?:src|href)\s*=\s*["']?\s*(?:[a-z][a-z0-9+.-]*:)?//`)
	reFrameish     = regexp.MustCompile(`(?i)<(?:iframe|object|embed)\b`)
	reBase         = regexp.MustCompile(`(?i)<base\b`)

	// An identifier boundary on the left: `isEval(`, `obj.eval(` and
	// `myFunction(` are not what the policy blocks.
	reEval         = regexp.MustCompile(`(?:^|[^\w$.])eval\s*\(`)
	reFunctionCtor = regexp.MustCompile(`(?:^|[^\w$.])(?:new\s+)?Function\s*\(`)
	reStringTimer  = regexp.MustCompile("(?:^|[^\\w$.])set(?:Timeout|Interval)\\s*\\(\\s*[\"'`]")
	reWorker       = regexp.MustCompile(`(?:^|[^\w$.])(?:new\s+)?(?:Shared)?Worker\s*\(|importScripts\s*\(`)
	reWasm         = regexp.MustCompile(`WebAssembly\.(?:compile|instantiate|Module)`)

	reCSSRemoteURL = regexp.MustCompile(`(?i)url\(\s*["']?\s*(?:[a-z][a-z0-9+.-]*:)?//`)
	reCSSImport    = regexp.MustCompile(`(?i)@import\s+(?:url\()?\s*["']?\s*(?:[a-z][a-z0-9+.-]*:)?//`)
)

func TestNoExternalAssetReferencesAndNoCSPViolations(t *testing.T) {
	requireBuilt(t)
	checked := 0
	err := fs.WalkDir(Assets(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(Assets(), p)
		if err != nil {
			return err
		}
		switch path.Ext(p) {
		case ".html", ".js", ".css":
			checked++
		}
		for _, v := range cspViolations(p, string(body)) {
			t.Errorf("%s: %s", p, v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// index.html, app.js, app.css at the least. Fewer means the walk is
	// looking at the placeholder, not a bundle.
	if checked < 3 {
		t.Fatalf("checked %d html/js/css files; the bundle is not where this test looks", checked)
	}
}

// The fixtures: each line is one thing the policy blocks, and each must be
// caught. The clean ones are what Vite and the bundled libraries actually
// emit, and must not be.
func TestTheCSPCheckCatchesWhatItClaimsTo(t *testing.T) {
	bad := []struct{ name, body string }{
		{"index.html", `<script>window.x = 1</script>`},
		{"index.html", `<script type="module">import "./a.js"</script>`},
		{"index.html", `<body onload="start()">`},
		{"index.html", `<a href="javascript:void(0)">`},
		{"index.html", `<script src="https://cdn.example/x.js"></script>`},
		{"index.html", `<link rel="stylesheet" href="//fonts.example/x.css">`},
		{"index.html", `<iframe src="./x.html"></iframe>`},
		{"index.html", `<base href="/">`},
		{"app.js", `var f=eval("1+1")`},
		{"app.js", `x=new Function("return this")()`},
		{"app.js", `x=Function("return this")()`},
		{"app.js", "setTimeout(`go()`,10)"},
		{"app.js", `new Worker(URL.createObjectURL(b))`},
		{"app.js", `WebAssembly.instantiate(buf)`},
		{"app.css", `@font-face{src:url(https://fonts.example/a.woff2)}`},
		{"app.css", `.a{background:url("//cdn.example/x.png")}`},
		{"app.css", `@import url(https://fonts.example/x.css);`},
		{"app.css", `@import "//fonts.example/x.css";`},
	}
	for _, c := range bad {
		if len(cspViolations(c.name, c.body)) == 0 {
			t.Errorf("%s: not flagged: %s", c.name, c.body)
		}
	}
	good := []struct{ name, body string }{
		{"index.html", `<script type="module" crossorigin src="./assets/app.js"></script>
<link rel="stylesheet" crossorigin href="./assets/app.css"><div id="root"></div>`},
		// React assigns handlers as properties, which the policy allows.
		{"app.js", `t.onclick=en;r.onload=t;isFunction(x);typeof e=="function";o.eval(x)`},
		{"app.js", `setTimeout(()=>f(),10);el.style.color="red";el.innerHTML='<div style="color:red">'`},
		{"app.css", `.a{background:url("data:image/svg+xml;base64,AAAA")}.b{background:url(./img.png)}`},
		{"app.css", `@import "./same-origin.css";`},
	}
	for _, c := range good {
		if v := cspViolations(c.name, c.body); len(v) != 0 {
			t.Errorf("%s: false positive %v on: %s", c.name, v, c.body)
		}
	}
}

// The policy the check above is written against. If this fails, the policy
// changed: re-read cspViolations before updating the expectations here.
func TestTheCSPThisCheckAssumes(t *testing.T) {
	rec := httptest.NewRecorder()
	oidcauth.SecurityHeaders("https://idp.example/realms/r")(http.NotFoundHandler()).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	directives := map[string]string{}
	for _, d := range strings.Split(csp, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	want := map[string]string{
		"default-src": "'self'",
		"connect-src": "'self' https://idp.example",
		"style-src":   "'self' 'unsafe-inline'",
		"img-src":     "'self' data:",
		"font-src":    "'self'",
	}
	for name, value := range want {
		if directives[name] != value {
			t.Errorf("%s = %q, want %q (policy: %s)", name, directives[name], value, csp)
		}
	}
	// Scripts fall back to default-src: no inline, no eval, no wasm.
	for _, name := range []string{"script-src", "script-src-elem", "script-src-attr", "worker-src"} {
		if v, ok := directives[name]; ok {
			t.Errorf("%s = %q is set; this check assumes scripts inherit default-src 'self'", name, v)
		}
	}
	if strings.Contains(csp, "unsafe-eval") {
		t.Errorf("the policy grants unsafe-eval: %s", csp)
	}
}
