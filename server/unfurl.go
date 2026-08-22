package main

// Link preview (Open Graph unfurl).
//
// SECURITY: this endpoint fetches an arbitrary user-supplied URL server-side,
// which is a classic SSRF vector. Defences, in order:
//   1. Scheme allowlist: only http/https.
//   2. A custom dialer (safeDialControl) re-checks the ACTUAL resolved IP at
//      connect time for every dial, including each redirect hop. This defeats
//      DNS-rebinding (where the hostname resolves to a public IP during the
//      pre-check but a private one at connect). Private / loopback / link-local
//      / unspecified / multicast / CGNAT ranges are refused.
//   3. Redirects capped at 3; every hop is re-validated by the same dialer.
//   4. 5s timeout, response body capped at 512 KB, only text/html parsed.
// Results are cached in memory (TTL + hard cap) so repeated links are cheap and
// don't let a chat hammer third-party hosts through us.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var unfurlLimiter = newLimiter(40, defaultWindowSecs) // 40 unfurls / user / 10 min

type unfurlResult struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Image       string `json:"image"`
	Site        string `json:"site"`
}

type unfurlCacheEntry struct {
	res unfurlResult
	at  time.Time
}

var (
	unfurlMu    sync.Mutex
	unfurlCache = map[string]unfurlCacheEntry{}
)

const (
	unfurlTTL      = 6 * time.Hour
	unfurlCacheCap = 500
	unfurlMaxBody  = 512 << 10 // 512 KB
)

// isDisallowedIP reports whether an IP must never be reached by the unfurler.
func isDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// 100.64.0.0/10 (CGNAT) — not covered by IsPrivate.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return true
	}
	return false
}

func newSafeHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 4 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil || len(ips) == 0 {
				return nil, &net.AddrError{Err: "cannot resolve host", Addr: host}
			}
			for _, ip := range ips {
				if isDisallowedIP(ip) {
					return nil, &net.AddrError{Err: "blocked address", Addr: ip.String()}
				}
			}
			// Dial the first allowed IP explicitly so we connect to exactly the
			// address we validated (no second, unvalidated resolution).
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
		TLSHandshakeTimeout: 4 * time.Second,
		DisableKeepAlives:   true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return http.ErrUseLastResponse
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return &url.Error{Op: "redirect", URL: req.URL.String(), Err: errBlockedScheme}
			}
			return nil
		},
	}
}

var errBlockedScheme = &net.AddrError{Err: "blocked scheme"}

func unfurlHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if unfurlLimiter.isBlocked(username) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	raw := r.URL.Query().Get("url")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}
	unfurlLimiter.recordFailure(username)

	// Cache hit?
	unfurlMu.Lock()
	if e, ok := unfurlCache[raw]; ok && time.Since(e.at) < unfurlTTL {
		unfurlMu.Unlock()
		writeJSON(w, e.res)
		return
	}
	unfurlMu.Unlock()

	client := newSafeHTTPClient()
	req, _ := http.NewRequest(http.MethodGet, raw, nil)
	req.Header.Set("User-Agent", "HexerisBot/1.0 (+link-preview)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	// 2xx only: after the redirect limit CheckRedirect returns
	// ErrUseLastResponse and the 3xx response itself lands here, where
	// there is nothing to preview but a Location header.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "html") {
		http.Error(w, "not previewable", http.StatusUnprocessableEntity)
		return
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, unfurlMaxBody))
	res := parseOpenGraph(string(body), u)

	unfurlMu.Lock()
	if len(unfurlCache) >= unfurlCacheCap {
		unfurlCache = map[string]unfurlCacheEntry{} // simplest bounded eviction
	}
	unfurlCache[raw] = unfurlCacheEntry{res: res, at: time.Now()}
	unfurlMu.Unlock()

	writeJSON(w, res)
}

var (
	reMeta     = regexp.MustCompile(`(?is)<meta\s+[^>]*>`)
	reTitleTag = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reAttrProp = regexp.MustCompile(`(?is)(?:property|name)\s*=\s*["']([^"']+)["']`)
	reAttrCont = regexp.MustCompile(`(?is)content\s*=\s*["']([^"']*)["']`)
)

func parseOpenGraph(html string, base *url.URL) unfurlResult {
	res := unfurlResult{URL: base.String(), Site: base.Hostname()}
	for _, tag := range reMeta.FindAllString(html, -1) {
		pm := reAttrProp.FindStringSubmatch(tag)
		cm := reAttrCont.FindStringSubmatch(tag)
		if pm == nil || cm == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(pm[1]))
		val := htmlUnescape(strings.TrimSpace(cm[1]))
		switch key {
		case "og:title", "twitter:title":
			if res.Title == "" {
				res.Title = val
			}
		case "og:description", "twitter:description", "description":
			if res.Description == "" {
				res.Description = val
			}
		case "og:image", "twitter:image", "twitter:image:src":
			if res.Image == "" {
				res.Image = absoluteURL(base, val)
			}
		case "og:site_name":
			res.Site = val
		}
	}
	if res.Title == "" {
		if tm := reTitleTag.FindStringSubmatch(html); tm != nil {
			res.Title = htmlUnescape(strings.TrimSpace(tm[1]))
		}
	}
	// Only surface an image URL we'd also be willing to load (http/https).
	if res.Image != "" {
		if iu, err := url.Parse(res.Image); err != nil || (iu.Scheme != "http" && iu.Scheme != "https") {
			res.Image = ""
		}
	}
	res.Title = clip(res.Title, 200)
	res.Description = clip(res.Description, 300)
	return res
}

func absoluteURL(base *url.URL, ref string) string {
	if ref == "" {
		return ""
	}
	if u, err := url.Parse(ref); err == nil {
		return base.ResolveReference(u).String()
	}
	return ref
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}

var htmlEntities = strings.NewReplacer(
	"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&#x27;", "'", "&nbsp;", " ",
)

func htmlUnescape(s string) string { return htmlEntities.Replace(s) }
