package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"picbox/frontend"
)

// The web UI is the original React app built as a static bundle (see
// frontend/README.md). The Go binary embeds the built files through
// the frontend package and serves one HTML shell for every original page route;
// the browser then reads and writes the JSON API only.
//
// Security posture:
//   - script-src stays 'self': the bundle has one external module script and no
//     inline JavaScript, so no nonce/unsafe-inline is needed.
//   - style-src needs 'unsafe-inline' because the reused React components style
//     themselves with the style attribute (dialogs, tables, progress bars).
//   - img-src allows data: and blob: for the local upload preview and clipboard
//     paste support; remote origins are not allowed.
//   - connect-src stays 'self'; the UI never talks to third-party endpoints.
//
// Pages are registered explicitly. A GET/HEAD navigation that matches no route
// is answered by a thin wrapper around the mux with HTTP 404 plus the same
// shell (the original Next.js app rendered app/not-found.tsx for it), so link
// checkers and crawlers still see the real status. /api/ prefixes and /assets/
// keep their own responses and never receive the page shell.

const webContentSecurityPolicy = "default-src 'none'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
	"connect-src 'self'; form-action 'self'; base-uri 'none'; " +
	"frame-ancestors 'none'; object-src 'none'"

const (
	webIndexFile    = "dist/index.html"
	webAssetsPrefix = "dist/assets/"
	webHTMLMimeType = "text/html; charset=utf-8"
)

// webAssetContentTypes only allows the flat asset extensions the bundle emits.
// Anything else under /assets/ is a 404 instead of a file probe.
var webAssetContentTypes = map[string]string{
	".css": "text/css; charset=utf-8",
	".js":  "text/javascript; charset=utf-8",
	".svg": "image/svg+xml; charset=utf-8",
}

// webImageIDPattern mirrors the identifiers handed out by the image worker and
// keeps arbitrary path fragments from being rendered as an image page. The
// page itself only receives the static shell; the id is read from the URL by
// the client, so it never reaches the HTML.
var webImageIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,64}$`)

// webPageRoutes are the original page routes the static shell implements.
// "$" (the home page) is registered separately because its trailing slash is
// already canonical.
var webPageRoutes = []string{
	"/dashboard",
	"/admin",
	"/forgot-password",
	"/reset-password",
	"/verify-email",
}

// registerWebRoutes wires the embedded static bundle onto the app mux.
func (a *App) registerWebRoutes() {
	a.mux.HandleFunc("GET /{$}", a.handleWebPage)
	for _, page := range webPageRoutes {
		a.mux.HandleFunc("GET "+page, a.handleWebPage)
		// Next.js runs with trailingSlash:false, so a registered page route with
		// an extra slash answers 308 to the canonical path (query preserved).
		a.mux.HandleFunc("GET "+page+"/{$}", a.handleWebPageTrailingSlash)
	}
	a.mux.HandleFunc("GET /images/{id}", a.handleWebImagePage)
	a.mux.HandleFunc("GET /images/{id}/{$}", a.handleWebPageTrailingSlash)
	a.mux.HandleFunc("GET /assets/{name}", a.handleWebAsset)
	// Runtime UI configuration. The upload ceiling is an operator setting
	// (UPLOAD_MAX_BYTES), so the browser must read it instead of depending on a
	// value baked in at build time; changing it must not require rebuilding the
	// bundle. Only non-sensitive values are exposed here.
	a.mux.HandleFunc("GET /api/config", a.handleWebConfig)
}

// webUnknownNavigation wraps the whole mux. A navigation (GET/HEAD) that matches
// no registered route answers 404 and still receives the shell, because the
// original Next.js app rendered app/not-found.tsx for it; a bare router 404
// loses the page entirely. Routing itself is not re-implemented here: the mux's
// own lookup decides. Matched routes, method mismatches (405 + Allow), the /api/
// JSON 404 and the mux's path-cleanup redirects all keep flowing through
// ServeMux, so their behaviour cannot drift from the route table.
func (a *App) webUnknownNavigation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && a.webNavigationUnmatched(r) {
			a.handleWebNotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) webNavigationUnmatched(r *http.Request) bool {
	// Handler returns an empty pattern for "no route", but also for a few mux
	// answers that are not 404s; the checks below keep those on the mux path.
	if _, pattern := a.mux.Handler(r); pattern != "" {
		return false
	}
	// /assets/* is a subresource namespace, not a navigation: an unknown asset
	// keeps its plain 404 instead of an HTML shell a <script>/<link> cannot use.
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		return false
	}
	// An unclean path ("//", "./" or "../") gets a redirect from the mux even
	// when nothing matches after cleaning; do not replace that with a 404 page.
	return webCleanPath(r.URL.EscapedPath()) == r.URL.EscapedPath()
}

// webCleanPath mirrors net/http's internal cleanPath so the check above can tell
// a genuine "no route" answer from the mux's relative-path redirect.
func webCleanPath(escaped string) string {
	if escaped == "" {
		return "/"
	}
	if escaped[0] != '/' {
		escaped = "/" + escaped
	}
	cleaned := path.Clean(escaped)
	if escaped[len(escaped)-1] == '/' && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned
}

func (a *App) handleWebConfig(w http.ResponseWriter, r *http.Request) {
	// writeJSON already sets Cache-Control: no-store and the JSON content type.
	//
	// mailEnabled is the same predicate the mail endpoints use (SMTP configured
	// AND BASEURL set). Without it the UI can only discover an unconfigured
	// deployment by walking into a request that always answers 503
	// MAIL_UNAVAILABLE. It is a single capability boolean: host, port, account
	// and password stay server-side, and the registration/activation-code path
	// does not read it at all.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"uploadMaxBytes": a.imagesMaxUploadBytes(),
		"mailEnabled":    a.mailAvailable(),
	})
}

func (a *App) handleWebPage(w http.ResponseWriter, r *http.Request) {
	a.serveWebPage(w, r, http.StatusOK)
}

// handleWebNotFound answers an unknown navigation: the original app has no page
// for it, so the document status is 404, but the shell is still served and the
// client-side router renders the same React not-found page as the original
// app/not-found.tsx. Serving the shell keeps the CSP and security headers of
// every other page; serving it with 200 would lie about the route existing.
func (a *App) handleWebNotFound(w http.ResponseWriter, r *http.Request) {
	a.serveWebPage(w, r, http.StatusNotFound)
}

// handleWebPageTrailingSlash mirrors the original Next.js canonicalisation: 308
// to the same route without the trailing slash, keeping the raw query string so
// a bookmarked link with filters does not lose them. The target is derived from
// the request path alone, so this cannot become an open redirect.
func (a *App) handleWebPageTrailingSlash(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSuffix(r.URL.EscapedPath(), "/")
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || target == "" {
		a.handleWebNotFound(w, r)
		return
	}
	if raw := r.URL.RawQuery; raw != "" {
		target += "?" + raw
	}
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("Location", target)
	w.WriteHeader(http.StatusPermanentRedirect)
}

// handleWebImagePage decides the document status of the detail page before the
// shell is written: a missing, trashed or hidden image is invisible to this
// caller in the original page (it called notFound()), so the initial GET/HEAD
// answers 404 instead of the 200 the shell alone would produce. Visibility is
// not re-implemented here: it is the same shared rule /api/images/{id}/meta
// uses, including "the uploader's own cookie session still sees their hidden
// image" and "an API key never does".
func (a *App) handleWebImagePage(w http.ResponseWriter, r *http.Request) {
	imageID := r.PathValue("id")
	if !webImageIDPattern.MatchString(imageID) {
		// No such identifier can exist, so this is an unknown page path.
		a.handleWebNotFound(w, r)
		return
	}

	status := http.StatusOK
	var requestErr *imagesError
	// 只做记录 + 可见性判断：标签与对外 URL 由 meta 接口在浏览器请求时补充。
	if _, err := a.imagesLoadVisibleRecord(r, imageID); err != nil {
		switch {
		case errors.As(err, &requestErr) && requestErr.status == http.StatusNotFound:
			status = http.StatusNotFound
		default:
			// The shell is still the right document; it reports the failure itself
			// (the /meta request the page makes fails the same way). Do not turn a
			// backend error into a 404 that claims the image does not exist.
			log.Printf("web: 图片详情页可见性检查失败 %s: %v", imageID, err)
			status = http.StatusInternalServerError
		}
	}
	a.serveWebPage(w, r, status)
}

func (a *App) handleWebAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		http.NotFound(w, r)
		return
	}
	contentType, ok := webAssetContentTypes[strings.ToLower(path.Ext(name))]
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.serveWebFile(w, r, webAssetsPrefix+name, contentType, "no-cache")
}

// serveWebPage writes the shell with an explicit status: 200 for a route the
// UI implements, 404 for an unknown or invisible page.
func (a *App) serveWebPage(w http.ResponseWriter, r *http.Request, status int) {
	a.serveWebFileStatus(w, r, webIndexFile, webHTMLMimeType, "no-store", status)
}

// serveWebFile writes one embedded file with the security headers every page
// and asset needs. http.ServeContent handles HEAD, ranges and conditional
// requests, so HEAD gets the same headers and status without a body.
func (a *App) serveWebFile(w http.ResponseWriter, r *http.Request, name, contentType, cacheControl string) {
	a.serveWebFileStatus(w, r, name, contentType, cacheControl, http.StatusOK)
}

// serveWebFileStatus serves an embedded file as status. A non-200 status has no
// cache validator: a 304 must never stand in for "this route does not exist",
// and the body is still written so the browser can render the not-found page.
func (a *App) serveWebFileStatus(w http.ResponseWriter, r *http.Request, name, contentType, cacheControl string, status int) {
	data, err := frontend.Dist.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	header := w.Header()
	header.Set("Content-Security-Policy", webContentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Cache-Control", cacheControl)
	header.Set("Content-Type", contentType)

	if status != http.StatusOK {
		header.Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
		return
	}

	sum := sha256.Sum256(data)
	header.Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)

	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}
