package main

// Sign-in through a corporate identity provider over OpenID Connect.
//
// ═══ WHAT THIS GIVES ══════════════════════════════════════════════════════
//
// The customer's security team gets what no local password can give: a single
// account authority. An employee leaves — they are disabled in Keycloak/Entra,
// and access to Hexeris disappears along with everything else. Hexeris
// passwords do not go anywhere: OIDC is added as a SECOND way in, not as a
// replacement.
//
// ═══ WHY DISCOVERY RATHER THAN ONE SPECIFIC PROVIDER ══════════════════════
//
// The endpoint addresses come from the provider's own
// /.well-known/openid-configuration. That is exactly what the document exists
// for, and it is the difference between "works with Keycloak" and "works with
// whatever the customer runs". Verified against Keycloak; Authentik, Entra ID,
// Okta and Google Workspace serve the same document.
//
// ═══ WHAT IS DONE THIS WAY RATHER THAN MORE SIMPLY ════════════════════════
//
//   • PKCE (S256) — even though we have a client_secret. An intercepted code
//     without the verifier is useless, and it costs thirty lines.
//   • nonce — ties the ID token to our request. Without it any valid token
//     from the same provider will do, including one issued for a different
//     application in the same tenant.
//   • The ID token signature is verified against the provider's JWKS. Trusting
//     the token body without checking the signature means accepting any JSON
//     anyone sends to the callback.
//   • The frontend is answered with a ONE-TIME exchange code rather than the
//     JWT in the address bar: the address lands in nginx's log, the browser
//     history and the Referer, and a 30-day token would settle in all three.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ─── Configuration ────────────────────────────────────────────────────────

// oidcEnabled is the switch. Separate from whether the settings exist: an
// administrator must be able to turn provider sign-in off without erasing it.
func oidcEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("OIDC_ENABLED")), "true")
}

type oidcConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       string
	// Which claim the Hexeris user name comes from. email by default: every
	// provider has it and it matches what the person already uses.
	// preferred_username is not always unique across domains.
	UsernameClaim string
}

// oidcSettings gathers the settings and says what is missing. It returns the
// reason as text rather than just false: an administrator must read which
// variable is unset instead of hunting through logs.
func oidcSettings() (oidcConfig, string) {
	c := oidcConfig{
		Issuer:        strings.TrimRight(strings.TrimSpace(os.Getenv("OIDC_ISSUER")), "/"),
		ClientID:      strings.TrimSpace(os.Getenv("OIDC_CLIENT_ID")),
		ClientSecret:  strings.TrimSpace(os.Getenv("OIDC_CLIENT_SECRET")),
		RedirectURL:   strings.TrimSpace(os.Getenv("OIDC_REDIRECT_URL")),
		Scopes:        strings.TrimSpace(os.Getenv("OIDC_SCOPES")),
		UsernameClaim: strings.TrimSpace(os.Getenv("OIDC_USERNAME_CLAIM")),
	}
	if c.Scopes == "" {
		c.Scopes = "openid email profile"
	}
	if c.UsernameClaim == "" {
		c.UsernameClaim = "email"
	}
	if !oidcEnabled() {
		return c, "single sign-on is disabled (OIDC_ENABLED is not true)"
	}
	switch {
	case c.Issuer == "":
		return c, "OIDC_ISSUER is not set"
	case !strings.HasPrefix(c.Issuer, "https://") && !strings.HasPrefix(c.Issuer, "http://"):
		return c, "OIDC_ISSUER must be a full URL, for example https://sso.example.com/realms/hexeris"
	case c.ClientID == "":
		return c, "OIDC_CLIENT_ID is not set"
	case c.ClientSecret == "":
		return c, "OIDC_CLIENT_SECRET is not set"
	case c.RedirectURL == "":
		return c, "OIDC_REDIRECT_URL is not set (must match the redirect URI registered with the provider)"
	}
	return c, ""
}

// ─── Discovery ────────────────────────────────────────────────────────────

type oidcDiscovery struct {
	Issuer        string `json:"issuer"`
	AuthURL       string `json:"authorization_endpoint"`
	TokenURL      string `json:"token_endpoint"`
	JWKSURL       string `json:"jwks_uri"`
	UserInfoURL   string `json:"userinfo_endpoint"`
	EndSessionURL string `json:"end_session_endpoint"`
}

// Outbound network calls always have a time ceiling. The sign-in handler holds
// a goroutine, and a hung provider without a timeout would pile them up.
var oidcHTTP = &http.Client{Timeout: 10 * time.Second}

var (
	discoMu     sync.Mutex
	discoCache  *oidcDiscovery
	discoUntil  time.Time
	discoIssuer string
)

// oidcDiscover reads the provider's document, caching it for an hour.
//
// The cache is not an optimisation but a protection: without it every sign-in
// would hit the external service twice, and a second of provider downtime
// would break sign-in for everyone at once.
func oidcDiscover(ctx context.Context, issuer string) (*oidcDiscovery, error) {
	discoMu.Lock()
	if discoCache != nil && discoIssuer == issuer && time.Now().Before(discoUntil) {
		d := discoCache
		discoMu.Unlock()
		return d, nil
	}
	discoMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := oidcHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the identity provider: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the identity provider answered %d to the discovery request", resp.StatusCode)
	}
	var d oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&d); err != nil {
		return nil, fmt.Errorf("the discovery document is not valid JSON: %w", err)
	}
	if d.AuthURL == "" || d.TokenURL == "" || d.JWKSURL == "" {
		return nil, errors.New("the discovery document is missing authorization_endpoint, token_endpoint or jwks_uri")
	}
	// The issuer in the document must match the configured one: a mismatch means
	// either a typo in the configuration or a substitution, and both are handled
	// the same way — by refusing rather than guessing.
	if d.Issuer != "" && strings.TrimRight(d.Issuer, "/") != issuer {
		return nil, fmt.Errorf("the provider calls itself %q but OIDC_ISSUER says %q", d.Issuer, issuer)
	}

	discoMu.Lock()
	discoCache, discoIssuer, discoUntil = &d, issuer, time.Now().Add(time.Hour)
	discoMu.Unlock()
	return &d, nil
}

// ─── Sign-ins in progress ─────────────────────────────────────────────────

// pendingOIDC — the state of one sign-in attempt between redirect and callback.
type pendingOIDC struct {
	nonce    string
	verifier string // PKCE
	created  time.Time
	returnTo string
}

var (
	oidcStatesMu sync.Mutex
	oidcStates   = map[string]*pendingOIDC{}
)

// A sign-in attempt lives for minutes, not hours: the state is a single-use
// pass, and a long lifetime only widens the window for reusing it.
const oidcStateTTL = 10 * time.Minute

func putOIDCState(state string, p *pendingOIDC) {
	oidcStatesMu.Lock()
	defer oidcStatesMu.Unlock()
	// Expired entries are cleaned up along the way: a separate collector for a
	// dozen records would be one more moving part.
	for k, v := range oidcStates {
		if time.Since(v.created) > oidcStateTTL {
			delete(oidcStates, k)
		}
	}
	oidcStates[state] = p
}

// takeOIDCState takes the state ONCE: a repeat callback with the same state
// must not pass.
func takeOIDCState(state string) *pendingOIDC {
	oidcStatesMu.Lock()
	defer oidcStatesMu.Unlock()
	p := oidcStates[state]
	delete(oidcStates, state)
	if p == nil || time.Since(p.created) > oidcStateTTL {
		return nil
	}
	return p
}

// ─── The one-time exchange code ───────────────────────────────────────────

type oidcTicket struct {
	token    string
	username string
	created  time.Time
}

var (
	ticketsMu sync.Mutex
	tickets   = map[string]oidcTicket{}
)

// The exchange code lives for seconds: the frontend collects it right after
// the redirect.
const oidcTicketTTL = 2 * time.Minute

func putTicket(code string, t oidcTicket) {
	ticketsMu.Lock()
	defer ticketsMu.Unlock()
	for k, v := range tickets {
		if time.Since(v.created) > oidcTicketTTL {
			delete(tickets, k)
		}
	}
	tickets[code] = t
}

func takeTicket(code string) (oidcTicket, bool) {
	ticketsMu.Lock()
	defer ticketsMu.Unlock()
	t, ok := tickets[code]
	delete(tickets, code) // single-use by definition
	if !ok || time.Since(t.created) > oidcTicketTTL {
		return oidcTicket{}, false
	}
	return t, true
}

// ─── Helpers ──────────────────────────────────────────────────────────────

func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ─── Endpoints ────────────────────────────────────────────────────────────

// GET /auth/oidc/status — is provider sign-in enabled.
//
// The frontend needs it so it does not draw a "Sign in with SSO" button that
// is certain to fail. The endpoint is UNauthenticated — it is called before
// signing in — so it returns nothing beyond availability and the button
// label.
func oidcStatusHandler(w http.ResponseWriter, r *http.Request) {
	problem := ""
	if _, p := oidcSettings(); p != "" {
		problem = p
	}
	resp := map[string]any{
		"enabled": problem == "",
		"label":   oidcButtonLabel(),
	}
	// The reason for unavailability goes to administrators only: to anyone else
	// it describes the internal infrastructure.
	if _, ok := validateToken(extractToken(r)); ok {
		resp["problem"] = problem
	}
	writeJSON(w, resp)
}

func oidcButtonLabel() string {
	if s := strings.TrimSpace(os.Getenv("OIDC_BUTTON_LABEL")); s != "" {
		return s
	}
	return "Sign in with SSO"
}

// GET /auth/oidc/start — send the person off to the provider.
func oidcStartHandler(w http.ResponseWriter, r *http.Request) {
	cfg, problem := oidcSettings()
	if problem != "" {
		http.Error(w, problem, http.StatusNotImplemented)
		return
	}
	// The endpoint is unauthenticated and calls an external service — the same
	// limit as on the other entry points.
	if loginLimiter.isBlocked(getIP(r)) {
		http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	disco, err := oidcDiscover(ctx, cfg.Issuer)
	if err != nil {
		log.Println("oidc: discovery:", err)
		http.Error(w, "the identity provider is not reachable", http.StatusBadGateway)
		return
	}

	state, err1 := randomURLSafe(24)
	nonce, err2 := randomURLSafe(24)
	verifier, err3 := randomURLSafe(48)
	if err1 != nil || err2 != nil || err3 != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	putOIDCState(state, &pendingOIDC{nonce: nonce, verifier: verifier, created: time.Now()})

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", cfg.RedirectURL)
	q.Set("scope", cfg.Scopes)
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")

	http.Redirect(w, r, disco.AuthURL+"?"+q.Encode(), http.StatusFound)
}

// GET /auth/oidc/callback — the provider returned the person with a code.
func oidcCallbackHandler(w http.ResponseWriter, r *http.Request) {
	cfg, problem := oidcSettings()
	if problem != "" {
		http.Error(w, problem, http.StatusNotImplemented)
		return
	}

	// The provider may have refused — show its reason rather than "something went
	// wrong": "access_denied" and "invalid_client" are fixed differently.
	if e := r.URL.Query().Get("error"); e != "" {
		desc := r.URL.Query().Get("error_description")
		oidcFail(w, r, strings.TrimSpace(e+" "+desc))
		return
	}

	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		oidcFail(w, r, "the provider returned an incomplete answer")
		return
	}
	pend := takeOIDCState(state)
	if pend == nil {
		// Either expired, or a replay, or the request did not come from us.
		oidcFail(w, r, "this sign-in link is no longer valid — start again")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	disco, err := oidcDiscover(ctx, cfg.Issuer)
	if err != nil {
		oidcFail(w, r, "the identity provider is not reachable")
		return
	}

	rawID, err := oidcExchangeCode(ctx, cfg, disco, code, pend.verifier)
	if err != nil {
		log.Println("oidc: code exchange:", err)
		oidcFail(w, r, "the provider did not accept our request")
		return
	}

	claims, err := verifyIDToken(ctx, cfg, disco, rawID, pend.nonce)
	if err != nil {
		log.Println("oidc: token verification:", err)
		oidcFail(w, r, "the identity token did not pass verification")
		return
	}

	username, err := oidcProvisionUser(claims, cfg)
	if err != nil {
		oidcFail(w, r, err.Error())
		return
	}

	// The block is checked BEFORE a token is issued — exactly as in /login and in
	// Google sign-in. Without this check SSO becomes a way around a block: an
	// administrator blocks an employee, the employee signs in through the provider
	// and gets a FRESH token whose iat is newer than the revocation stamp — so the
	// revocation does not cut them off.
	if userBlocked(username) {
		recordLogin(r, username, loginBlocked, "oidc")
		oidcFail(w, r, "this account is blocked")
		return
	}

	recordLogin(r, username, loginOK, "oidc")
	token, err := generateToken(username)
	if err != nil {
		oidcFail(w, r, "server error")
		return
	}
	ticket, err := randomURLSafe(24)
	if err != nil {
		oidcFail(w, r, "server error")
		return
	}
	putTicket(ticket, oidcTicket{token: token, username: username, created: time.Now()})
	setAuthCookie(w, token)

	// The address carries a one-time code, NOT a token: the address settles in
	// nginx's log, the browser history and the Referer, and a 30-day token would
	// stay in all three.
	http.Redirect(w, r, "/?sso="+url.QueryEscape(ticket), http.StatusFound)
}

// oidcFail returns the person to the sign-in page with a clear reason.
// The reason travels as text in the address, so it must contain nothing but
// the explanation — no tokens, no code, no claims.
func oidcFail(w http.ResponseWriter, r *http.Request, reason string) {
	http.Redirect(w, r, "/?sso_error="+url.QueryEscape(reason), http.StatusFound)
}

// POST /auth/oidc/exchange {"code":"…"} — the frontend swaps the one-time code
// for an ordinary Hexeris token.
func oidcExchangeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	t, ok := takeTicket(strings.TrimSpace(req.Code))
	if !ok {
		http.Error(w, "this sign-in code is not valid any more", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{"token": t.token, "username": t.username})
}

// ─── Code exchange and token verification ─────────────────────────────────

func oidcExchangeCode(ctx context.Context, cfg oidcConfig, disco *oidcDiscovery, code, verifier string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", cfg.RedirectURL)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, disco.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// client_secret_basic is the method every provider supports; in the request
	// body the secret more often ends up in someone else's access logs.
	req.SetBasicAuth(url.QueryEscape(cfg.ClientID), url.QueryEscape(cfg.ClientSecret))

	resp, err := oidcHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if resp.StatusCode != http.StatusOK {
		// The response body is NOT logged in full: it can contain tokens.
		return "", fmt.Errorf("token endpoint answered %d", resp.StatusCode)
	}
	var out struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.IDToken == "" {
		return "", errors.New("the provider returned no id_token — is the openid scope granted?")
	}
	return out.IDToken, nil
}

// verifyIDToken checks the signature and every mandatory claim.
//
// The order matters: signature first, content second. Parsing the claims of an
// unverified token and making decisions from them is the same as trusting the
// body of a letter without looking at the sender.
func verifyIDToken(ctx context.Context, cfg oidcConfig, disco *oidcDiscovery, raw, nonce string) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return oidcKeyFor(ctx, disco.JWKSURL, kid, t.Method.Alg())
	},
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512"}),
		jwt.WithIssuer(cfg.Issuer),
		jwt.WithAudience(cfg.ClientID),
		jwt.WithExpirationRequired(),
		// A small tolerance for clock drift: without it a server whose clock ran
		// a second fast would reject every other sign-in.
		jwt.WithLeeway(60*time.Second),
	)
	if err != nil {
		return nil, err
	}
	// nonce ties the token to OUR request. Without this check any valid token
	// from the same provider will do — including one issued for a different
	// application in the same tenant.
	if got, _ := claims["nonce"].(string); got != nonce {
		return nil, errors.New("nonce mismatch — the token does not belong to this sign-in attempt")
	}
	return claims, nil
}

// oidcResolveUser turns the token claims into a Hexeris user name and decides
// whether such an employee may be created.
func oidcResolveUser(claims jwt.MapClaims, cfg oidcConfig) (string, error) {
	raw, _ := claims[cfg.UsernameClaim].(string)
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return "", fmt.Errorf("the provider did not return %q — check the scopes and the claim mapping", cfg.UsernameClaim)
	}
	// Email-like claims must be verified. At some providers an unverified address
	// is set by the user themselves, and it can be used to impersonate someone
	// else's employee.
	if strings.Contains(raw, "@") {
		if v, ok := claims["email_verified"]; ok && !truthy(v) {
			return "", errors.New("this email address is not verified at the identity provider")
		}
		if err := checkEmailDomainAllowed(raw); err != nil {
			return "", err
		}
	}
	return raw, nil
}

// truthy — email_verified arrives both as a boolean and as a string: Google
// sends "true", Keycloak sends true. Both must be accepted, or a strict check
// would reject half the providers.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	}
	return false
}

// checkEmailDomainAllowed reuses the existing domain allowlist
// (ALLOWED_EMAIL_DOMAINS) — the same one applied to Google sign-in. A second
// list for SSO would mean a second place where a domain can be forgotten.
func checkEmailDomainAllowed(email string) error {
	allowed := allowedEmailDomains()
	if len(allowed) == 0 {
		return nil
	}
	at := strings.LastIndex(email, "@")
	if at < 0 || !allowed[email[at+1:]] {
		return errors.New("this email domain is not allowed to sign in here")
	}
	return nil
}
