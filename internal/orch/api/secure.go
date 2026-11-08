package api

// HTTP listener security (design 7 and 9, Task 21).
//
// The loopback HTTP listener is reached only through `tailscale serve`, which
// terminates TLS and forwards to 127.0.0.1. Paths under /api go to the same
// handler as the unix socket; every other path is the embedded web console
// (splitWeb in daemon.go, never served on the socket). Both are wrapped in
// this chain (outermost first):
//
//	secureHeaders -> hostCheck -> noCORS -> originCheck -> limitBody -> jsonOnly -> rateLimit -> handler
//
// The socket keeps its behaviour (trusted by peer uid) and gets limitBody only.
// No CORS header is ever sent: the console is same-origin.

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// ContentSecurityPolicy starts from the design's `default-src 'self';
	// frame-ancestors 'none'` and spells out the rest: no plugins, no <base>,
	// no form posts (the shell uses fetch), images may be data: URLs.
	ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	// PermissionsPolicy denies powerful features the console never uses.
	// publickey-credentials-get is deliberately not listed: passkeys need it.
	PermissionsPolicy = "camera=(), microphone=(), geolocation=(), payment=(), usb=()"
	// HSTSValue: the console is only served over HTTPS (tailscale serve).
	HSTSValue = "max-age=31536000"

	// TailscaleUserHeader is set by `tailscale serve` to the tailnet login of
	// the browser's user. It is trusted (as a rate-limit key only) when the TCP
	// peer is loopback, which is always true for this listener: it binds only
	// loopback addresses and must be reached only through tailscale serve.
	TailscaleUserHeader = "Tailscale-User-Login"

	// Rate limits on /api/v1/auth/*: a token bucket per key and a global one.
	DefaultAuthRatePerKey = 10   // requests per minute per user / address
	DefaultAuthRateGlobal = 120  // requests per minute, all keys together
	DefaultRateKeys       = 4096 // keys remembered (LRU beyond)

	// MinSessionTTL and MaxSessionTTL bound --session-ttl (the idle expiry of a
	// browser session; the absolute cap SessionMaxLifetime stays).
	MinSessionTTL = 5 * time.Minute
	MaxSessionTTL = SessionMaxLifetime
)

// refuse writes a middleware refusal in the shape of the path's API: the v1
// envelope under /api/v1, the flat {"error","code"} form elsewhere. Messages
// are fixed texts; nothing from the request is echoed.
func refuse(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	if isV1Path(r.URL.Path) {
		writeV1Err(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// secureHeaders sets the browser hardening headers on every response.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", PermissionsPolicy)
		h.Set("Strict-Transport-Security", HSTSValue)
		next.ServeHTTP(w, r)
	})
}

// hostOnly returns the lower-cased host of a Host header value or an
// allowlist entry, without the port.
func hostOnly(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	return strings.ToLower(h)
}

// hostCheck refuses a request whose Host (port ignored, case-insensitive) is
// not on the allowlist: 421 bad_host. This stops DNS rebinding.
func hostCheck(allowed map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "" || !allowed[hostOnly(r.Host)] {
			refuse(w, r, http.StatusMisdirectedRequest, "bad_host", "허용되지 않은 Host입니다")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// noCORS answers CORS preflights and other non-API methods with 405; no
// Access-Control-* header is ever written anywhere.
func noCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions, http.MethodTrace, http.MethodConnect:
			refuse(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "허용되지 않는 메서드입니다")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// needsOriginCheck: every request that can change state, and GET/HEAD
// /api/v1/auth/session, which rotates the session's CSRF token (CF16).
func needsOriginCheck(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return r.URL.Path == v1Prefix+"/auth/session"
	}
	return true
}

// originCheck is the CSRF origin layer:
//   - an Origin header must equal the console origin exactly (403 bad_origin);
//   - without Origin, Sec-Fetch-Site (sent by every current browser) must be
//     same-origin or none;
//   - with neither header the caller is not a browser. It is admitted only
//     when it carries no session cookie: a cookie is the ambient credential
//     CSRF abuses, while bearer-token and credential-less callers (the CLI's
//     `orch login`) have none.
func originCheck(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !needsOriginCheck(r) {
			next.ServeHTTP(w, r)
			return
		}
		ok := false
		if o := r.Header.Values("Origin"); len(o) > 0 {
			ok = len(o) == 1 && origin != "" && o[0] == origin
		} else if s := r.Header.Values("Sec-Fetch-Site"); len(s) > 0 {
			ok = len(s) == 1 && (s[0] == "same-origin" || s[0] == "none")
		} else {
			_, err := r.Cookie(SessionCookieName)
			ok = errors.Is(err, http.ErrNoCookie)
		}
		if !ok {
			refuse(w, r, http.StatusForbidden, "bad_origin", "허용되지 않은 출처의 요청입니다")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitBody caps request bodies: a declared length over max is refused up
// front (413 body_too_large), and the body is wrapped in MaxBytesReader so a
// chunked body cannot exceed it either (the handlers map that error to 413).
func limitBody(max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > max {
			refuse(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "요청 본문이 너무 큽니다")
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// jsonOnly requires `Content-Type: application/json` (optionally with
// charset=utf-8) on a POST/PUT/PATCH that has a body (CF10). An empty body
// (logout and other action POSTs) needs no type.
func jsonOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			if r.ContentLength != 0 && !isJSONType(r.Header.Values("Content-Type")) {
				refuse(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "요청 본문은 application/json이어야 합니다")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isJSONType(vals []string) bool {
	if len(vals) != 1 {
		return false
	}
	mt, params, err := mime.ParseMediaType(vals[0])
	if err != nil || mt != "application/json" {
		return false
	}
	for k, v := range params {
		if k != "charset" || !strings.EqualFold(v, "utf-8") {
			return false
		}
	}
	return true
}

// rateLimit throttles /api/v1/auth/* (login, registration, session, logout):
// per key, then globally (429 rate_limited with Retry-After). Other routes are
// not limited: they need a credential, and the console polls them.
//
// credKey, when not nil, recognises a request that carries a live session or
// bearer token and returns a key derived from that credential (CF21). Such a
// request is limited on that key only and skips the global ceiling: a local
// process that invents Tailscale-User-Login values can drain the global bucket
// (and could drain the admin's own header key by sending the admin's login),
// but it cannot lock a logged-in admin out of /auth/session or /auth/signers.
func rateLimit(l *rateLimiter, credKey func(*http.Request) (string, bool), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, v1Prefix+"/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		key, global := "", true
		if credKey != nil {
			if k, ok := credKey(r); ok {
				key, global = k, false
			}
		}
		if global {
			key = rateKey(r)
		}
		if ok, wait := l.allow(key, global); !ok {
			secs := int(math.Ceil(wait.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			refuse(w, r, http.StatusTooManyRequests, "rate_limited", "요청이 너무 많습니다. 잠시 후 다시 시도하세요")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateKey is the limiter key. Behind tailscale serve every TCP peer is
// 127.0.0.1, so the peer address alone would make one global bucket (a login
// DoS, CF12): when the peer is loopback, the tailnet login header that
// tailscale serve sets is used instead. A non-loopback peer (impossible for
// this listener) is keyed by its address and the header is ignored.
func rateKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if u := r.Header.Values(TailscaleUserHeader); len(u) == 1 && u[0] != "" && len(u[0]) <= 256 {
			return "user:" + u[0]
		}
	}
	return "addr:" + host
}

// rateLimiter is a token bucket per key (capacity = per-minute rate, refilled
// continuously) plus one global bucket. At most maxKeys keys are kept; a new
// key evicts the least recently used one. Eviction resets that key's bucket
// (fails open per key), which is why the global bucket exists: it bounds the
// total no matter how many keys an attacker invents.
type rateLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	perKey  float64
	global  float64
	window  time.Duration
	maxKeys int
	keys    map[string]*list.Element // of *rateBucket
	lru     *list.List               // front = most recently used
	all     rateBucket
}

type rateBucket struct {
	key    string
	tokens float64
	last   time.Time
}

func newRateLimiter(perKey, global, maxKeys int) *rateLimiter {
	return &rateLimiter{now: time.Now, perKey: float64(perKey), global: float64(global), window: time.Minute,
		maxKeys: maxKeys, keys: map[string]*list.Element{}, lru: list.New(), all: rateBucket{tokens: float64(global)}}
}

func (l *rateLimiter) refill(b *rateBucket, capacity float64, now time.Time) {
	if b.last.IsZero() {
		b.tokens, b.last = capacity, now
		return
	}
	if d := now.Sub(b.last); d > 0 {
		b.tokens = math.Min(capacity, b.tokens+d.Seconds()*capacity/l.window.Seconds())
		b.last = now
	}
}

// wait is how long until b holds one token.
func (l *rateLimiter) wait(b *rateBucket, capacity float64) time.Duration {
	need := 1 - b.tokens
	if need <= 0 {
		return 0
	}
	return time.Duration(need * float64(l.window) / capacity)
}

func (l *rateLimiter) bucket(key string, now time.Time) *rateBucket {
	if e, ok := l.keys[key]; ok {
		l.lru.MoveToFront(e)
		return e.Value.(*rateBucket)
	}
	for l.lru.Len() >= l.maxKeys {
		old := l.lru.Back()
		l.lru.Remove(old)
		delete(l.keys, old.Value.(*rateBucket).key)
	}
	b := &rateBucket{key: key}
	l.keys[key] = l.lru.PushFront(b)
	return b
}

// allow takes one token from key's bucket and, when global is true, from the
// global bucket, or none; on refusal it returns how long to wait. A
// credentialed caller (global == false) neither needs nor takes a global token.
func (l *rateLimiter) allow(key string, global bool) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.bucket(key, now)
	l.refill(b, l.perKey, now)
	l.refill(&l.all, l.global, now)
	if b.tokens < 1 {
		return false, l.wait(b, l.perKey)
	}
	if global && l.all.tokens < 1 {
		return false, l.wait(&l.all, l.global)
	}
	b.tokens--
	if global {
		l.all.tokens--
	}
	return true, 0
}

func (l *rateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}

// lookupIPAddr resolves a listen host name (tests swap it).
var lookupIPAddr = net.DefaultResolver.LookupIPAddr

// ResolveLoopbackListen checks an --http-listen value and returns the address
// to bind: "ip:port" with a loopback IP (127.0.0.0/8 or ::1). An empty host,
// an unspecified address (0.0.0.0, ::), any other IP, and a host name that
// resolves to anything but loopback addresses (every address is checked) are
// refused. A host name is bound by the IP it resolved to, so a later DNS
// answer cannot move the listener.
func ResolveLoopbackListen(ctx context.Context, listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("--http-listen %q는 호스트:포트 형식이어야 합니다", listen)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || strconv.FormatUint(p, 10) != port {
		return "", fmt.Errorf("--http-listen %q의 포트가 올바르지 않습니다", listen)
	}
	if host == "" {
		return "", fmt.Errorf("--http-listen %q: 호스트가 비어 있으면 모든 주소에 바인딩되므로 거부합니다(127.0.0.1을 쓰세요)", listen)
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		addrs, err := lookupIPAddr(ctx, host)
		if err != nil || len(addrs) == 0 {
			return "", fmt.Errorf("--http-listen %q: 호스트 이름을 확인할 수 없습니다", listen)
		}
		for _, a := range addrs {
			if a.Zone != "" {
				return "", fmt.Errorf("--http-listen %q: 루프백이 아닌 주소로 확인됩니다", listen)
			}
			ips = append(ips, a.IP)
		}
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return "", fmt.Errorf("--http-listen %q: 루프백(127.0.0.0/8, ::1)이 아닌 주소에는 바인딩하지 않습니다", listen)
		}
	}
	return net.JoinHostPort(ips[0].String(), port), nil
}

// normalizeAllowedHosts validates --allowed-host entries (a host name or IP,
// optionally with a port that is ignored) and returns the lower-cased set.
func normalizeAllowedHosts(hosts []string) (map[string]bool, error) {
	set := map[string]bool{}
	for _, h := range hosts {
		n := hostOnly(h)
		if strings.ContainsAny(h, "/ \t@?#") || !validHostName(n) {
			return nil, fmt.Errorf("--allowed-host %q는 호스트 이름이어야 합니다(스킴·경로 없이)", h)
		}
		set[n] = true
	}
	return set, nil
}

func validHostName(h string) bool {
	if net.ParseIP(h) != nil {
		return true
	}
	if h == "" || len(h) > 253 || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") || strings.Contains(h, "..") {
		return false
	}
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// ValidateHTTPConfig checks the HTTP listener settings before anything is
// opened: an enabled listener needs the WebAuthn origin and RP ID, at least
// one --allowed-host, and the origin's host among them (otherwise every
// browser request would be 421). It returns the address to bind ("" when the
// listener is off) and the normalized host allowlist.
func ValidateHTTPConfig(ctx context.Context, listen string, allowedHosts []string, rpID, origin string) (string, map[string]bool, error) {
	if listen == "" {
		return "", nil, nil
	}
	if err := ValidateWebAuthnConfig(rpID, origin); err != nil {
		return "", nil, err
	}
	if rpID == "" || origin == "" {
		return "", nil, errors.New("--http-listen에는 --origin과 --rp-id가 필요합니다")
	}
	if len(allowedHosts) == 0 {
		return "", nil, errors.New("--http-listen에는 --allowed-host가 하나 이상 필요합니다(콘솔의 호스트 이름)")
	}
	set, err := normalizeAllowedHosts(allowedHosts)
	if err != nil {
		return "", nil, err
	}
	u, err := url.Parse(origin)
	if err != nil || !set[strings.ToLower(u.Hostname())] {
		return "", nil, fmt.Errorf("--origin의 호스트가 --allowed-host 목록에 없습니다(허용: %s)", strings.Join(sortedKeys(set), ", "))
	}
	bind, err := ResolveLoopbackListen(ctx, listen)
	if err != nil {
		return "", nil, err
	}
	return bind, set, nil
}

// ValidateSessionTTL checks --session-ttl (0 means SessionIdleTTL).
func ValidateSessionTTL(d time.Duration) error {
	if d == 0 {
		return nil
	}
	if d < MinSessionTTL || d > MaxSessionTTL {
		return fmt.Errorf("--session-ttl은 %s 이상 %s 이하여야 합니다", MinSessionTTL, MaxSessionTTL)
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
