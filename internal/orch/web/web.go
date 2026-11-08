// Package web embeds the browser console (static HTML, CSS and JS) and serves
// it as a single-page application. It is mounted on the HTTP listener only
// (never the unix socket), inside the security middleware of package api, so
// the CSP, nosniff and Host/Origin checks apply to these responses too.
//
// Every file is served from an in-memory table built at start-up from the
// embedded tree, so a request path is only ever a lookup key: there is no
// file-system access to traverse. The SPA fallback (index.html) answers any
// GET/HEAD path that is not an asset; the caller routes /api/ elsewhere.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed static
var staticFS embed.FS

// asset is one embedded file ready to serve.
type asset struct {
	data  []byte
	ctype string
	etag  string
}

// contentTypes is explicit so the result never depends on the host's mime
// database. Scripts must be text/javascript for <script type="module">.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".svg":   "image/svg+xml",
	".json":  "application/json; charset=utf-8",
	".txt":   "text/plain; charset=utf-8",
	".ico":   "image/x-icon",
	".png":   "image/png",
	".woff2": "font/woff2",
}

const indexName = "index.html"

// load reads every file under root of fsys into the lookup table, keyed by
// the URL path ("/app.js"). A file of an unknown type is an error so a stray
// file cannot be served with a guessed type.
func load(fsys fs.FS, root string) (map[string]*asset, error) {
	sub, err := fs.Sub(fsys, root)
	if err != nil {
		return nil, err
	}
	out := map[string]*asset{}
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ct, ok := contentTypes[strings.ToLower(path.Ext(p))]
		if !ok {
			return &fs.PathError{Op: "web: unknown content type", Path: p, Err: fs.ErrInvalid}
		}
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out["/"+p] = &asset{data: b, ctype: ct, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		return nil
	})
	return out, err
}

// Handler serves the embedded console. It panics at start-up if the embedded
// tree is unusable (a build-time mistake, caught by the tests).
func Handler() http.Handler {
	assets, err := load(staticFS, "static")
	if err != nil {
		panic("web: " + err.Error())
	}
	return newHandler(assets)
}

func newHandler(assets map[string]*asset) http.Handler {
	index := assets["/"+indexName]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "허용되지 않는 메서드입니다", http.StatusMethodNotAllowed)
			return
		}
		// path.Clean collapses "//" and resolves "..", and URL.Path is already
		// percent-decoded, so "%2e%2e" cannot reach past the root either. The
		// result is only a map key.
		key := path.Clean("/" + r.URL.Path)
		a := assets[key]
		if key == "/" || key == "/"+indexName {
			a = index
		}
		if a == nil {
			a = index // SPA fallback: the hash router decides what to show
		}
		if a == nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", a.ctype)
		h.Set("ETag", a.etag)
		// Filenames are not hashed, so nothing may be cached without
		// revalidation; the ETag makes the revalidation cheap.
		h.Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.data))
	})
}
