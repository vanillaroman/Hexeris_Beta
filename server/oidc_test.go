package main

// Checks for provider sign-in. There are no cosmetic tests here: every
// skipped ID-token check is a way to sign in under someone else's name.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ─── Fixture: our own provider for the duration of the test ───────────────

type fakeIDP struct {
	key    *rsa.PrivateKey
	kid    string
	issuer string
	aud    string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("key generation: %v", err)
	}
	return &fakeIDP{key: k, kid: "test-kid", issuer: "https://sso.example.com/realms/hexeris", aud: "hexeris"}
}

// jwks returns the key set exactly as a provider publishes it.
func (f *fakeIDP) jwks() jwkSet {
	return jwkSet{Keys: []jwkKey{{
		Kid: f.kid, Kty: "RSA", Alg: "RS256", Use: "sig",
		N: base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes()),
	}}}
}

func (f *fakeIDP) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return s
}

func (f *fakeIDP) validClaims(nonce string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": f.issuer, "aud": f.aud, "sub": "u-1",
		"exp":   time.Now().Add(5 * time.Minute).Unix(),
		"iat":   time.Now().Unix(),
		"nonce": nonce, "email": "grace@example.com", "email_verified": true,
	}
}

// The keys are put into the JWKS cache directly — the test checks token
// verification, not a network round-trip.
func primeJWKS(t *testing.T, f *fakeIDP, url string) {
	t.Helper()
	keys, err := parseJWKS(f.jwks())
	if err != nil {
		t.Fatalf("parsing JWKS: %v", err)
	}
	jwksMu.Lock()
	jwksCache, jwksURLUsed = keys, url
	jwksUntil = time.Now().Add(time.Hour)
	jwksMu.Unlock()
	t.Cleanup(func() {
		jwksMu.Lock()
		jwksCache, jwksURLUsed, jwksUntil = nil, "", time.Time{}
		jwksMu.Unlock()
	})
}

// ─── ID-token verification ────────────────────────────────────────────────

// Every case below is a way to sign in under someone else's name if the check
// is skipped. So what is tested is not whether the happy path works but what
// does NOT pass.
func TestVerifyIDTokenRejectsBadTokens(t *testing.T) {
	f := newFakeIDP(t)
	const jwksURL = "https://sso.example.com/keys"
	primeJWKS(t, f, jwksURL)

	cfg := oidcConfig{Issuer: f.issuer, ClientID: f.aud}
	disco := &oidcDiscovery{JWKSURL: jwksURL}
	ctx := context.Background()
	const nonce = "n-12345"

	// A control: a correct token must pass, otherwise every rejection below
	// proves nothing.
	if _, err := verifyIDToken(ctx, cfg, disco, f.sign(t, f.validClaims(nonce)), nonce); err != nil {
		t.Fatalf("a correct token was rejected: %v", err)
	}

	bad := map[string]func(jwt.MapClaims){
		"foreign issuer":   func(c jwt.MapClaims) { c["iss"] = "https://evil.example.com" },
		"foreign audience": func(c jwt.MapClaims) { c["aud"] = "another-app" },
		"expired":          func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-10 * time.Minute).Unix() },
		"no exp":           func(c jwt.MapClaims) { delete(c, "exp") },
		"foreign nonce":    func(c jwt.MapClaims) { c["nonce"] = "someone-elses" },
		"no nonce":         func(c jwt.MapClaims) { delete(c, "nonce") },
	}
	for name, spoil := range bad {
		c := f.validClaims(nonce)
		spoil(c)
		if _, err := verifyIDToken(ctx, cfg, disco, f.sign(t, c), nonce); err == nil {
			t.Errorf("%s: the token was accepted", name)
		}
	}

	// A signature by a FOREIGN key is the central case: without signature
	// verification any correctly shaped JSON would do.
	other := newFakeIDP(t)
	other.kid = f.kid // pretend to be the same key
	if _, err := verifyIDToken(ctx, cfg, disco, other.sign(t, other.validClaims(nonce)), nonce); err == nil {
		t.Error("a token signed by a foreign key was accepted")
	}

	// alg=none is the classic attempt to bypass signature verification entirely.
	none := jwt.NewWithClaims(jwt.SigningMethodNone, f.validClaims(nonce))
	none.Header["kid"] = f.kid
	raw, err := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err == nil {
		if _, err := verifyIDToken(ctx, cfg, disco, raw, nonce); err == nil {
			t.Error("a token with alg=none was accepted")
		}
	}
}

// ─── Parsing JWKS ─────────────────────────────────────────────────────────

func TestParseJWKS(t *testing.T) {
	f := newFakeIDP(t)
	keys, err := parseJWKS(f.jwks())
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if keys[f.kid] == nil {
		t.Fatal("the key was not parsed")
	}
	if keys[f.kid].N.Cmp(f.key.N) != 0 || keys[f.kid].E != f.key.E {
		t.Fatal("the parsed key does not match the original")
	}

	// An encryption key is not used to verify signatures. Accepting it silently
	// would mean a sign-in failure at the least convenient moment.
	encOnly := f.jwks()
	encOnly.Keys[0].Use = "enc"
	if _, err := parseJWKS(encOnly); err == nil {
		t.Error("a set with a single encryption key was accepted as usable")
	}

	// A weak key. Accepting a short signature would mean trusting it completely.
	weak, _ := rsa.GenerateKey(rand.Reader, 1024)
	weakSet := jwkSet{Keys: []jwkKey{{
		Kid: "weak", Kty: "RSA", Use: "sig",
		N: base64.RawURLEncoding.EncodeToString(weak.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(weak.E)).Bytes()),
	}}}
	if _, err := parseJWKS(weakSet); err == nil {
		t.Error("a 1024-bit key was accepted")
	}

	// An empty set is an error, not "zero keys, carry on".
	if _, err := parseJWKS(jwkSet{}); err == nil {
		t.Error("an empty key set was accepted")
	}

	// Not everyone pads base64 the same way — both forms must be accepted.
	padded := f.jwks()
	padded.Keys[0].N = base64.URLEncoding.EncodeToString(f.key.N.Bytes())
	if _, err := parseJWKS(padded); err != nil {
		t.Errorf("padded base64 was not accepted: %v", err)
	}
}

// ─── Who gets access ──────────────────────────────────────────────────────

func TestOIDCResolveUser(t *testing.T) {
	defer os.Unsetenv("ALLOWED_EMAIL_DOMAINS")
	cfg := oidcConfig{UsernameClaim: "email"}

	os.Unsetenv("ALLOWED_EMAIL_DOMAINS")
	u, err := oidcResolveUser(jwt.MapClaims{"email": "Grace@Example.COM", "email_verified": true}, cfg)
	if err != nil {
		t.Fatalf("an ordinary sign-in: %v", err)
	}
	if u != "grace@example.com" {
		t.Errorf("the name was not lowercased: %q", u)
	}

	// At some providers an unverified address is set by the user themselves — it
	// can be used to impersonate someone else's employee.
	if _, err := oidcResolveUser(jwt.MapClaims{"email": "a@b.com", "email_verified": false}, cfg); err == nil {
		t.Error("an unverified address was accepted")
	}
	// Google sends a string, Keycloak a boolean — both must be accepted.
	if _, err := oidcResolveUser(jwt.MapClaims{"email": "a@b.com", "email_verified": "true"}, cfg); err != nil {
		t.Errorf("a string email_verified was not accepted: %v", err)
	}
	if _, err := oidcResolveUser(jwt.MapClaims{"email": "a@b.com", "email_verified": "false"}, cfg); err == nil {
		t.Error("a string email_verified=false was accepted")
	}

	// A missing claim means "not configured", not "an empty name".
	if _, err := oidcResolveUser(jwt.MapClaims{"sub": "u1"}, cfg); err == nil {
		t.Error("a token with no user name was accepted")
	}

	// The domain allowlist: exactly the mechanism used for Google sign-in.
	os.Setenv("ALLOWED_EMAIL_DOMAINS", "example.com, partner.org")
	if _, err := oidcResolveUser(jwt.MapClaims{"email": "grace@example.com", "email_verified": true}, cfg); err != nil {
		t.Errorf("an allowed domain was rejected: %v", err)
	}
	if _, err := oidcResolveUser(jwt.MapClaims{"email": "mallory@evil.com", "email_verified": true}, cfg); err == nil {
		t.Error("a foreign domain got through — SSO would create anyone")
	}
	// A substring must not pass as a domain.
	if _, err := oidcResolveUser(jwt.MapClaims{"email": "mallory@notexample.com", "email_verified": true}, cfg); err == nil {
		t.Error("a domain was accepted on a substring match")
	}
}

// ─── Single use ───────────────────────────────────────────────────────────

// The state and the exchange code must work EXACTLY once: a reused state is
// a replay of someone else's sign-in, a reused code is a second token issued
// for a single sign-in.
func TestOIDCStateAndTicketAreSingleUse(t *testing.T) {
	putOIDCState("s1", &pendingOIDC{nonce: "n", verifier: "v", created: time.Now()})
	if takeOIDCState("s1") == nil {
		t.Fatal("the state was not found the first time")
	}
	if takeOIDCState("s1") != nil {
		t.Error("the state worked a second time")
	}
	if takeOIDCState("never existed") != nil {
		t.Error("a state was returned for an unknown key")
	}

	// An expired state is no good even when the key is right.
	putOIDCState("s2", &pendingOIDC{nonce: "n", created: time.Now().Add(-2 * oidcStateTTL)})
	if takeOIDCState("s2") != nil {
		t.Error("an expired state was accepted")
	}

	putTicket("t1", oidcTicket{token: "jwt", username: "grace", created: time.Now()})
	if got, ok := takeTicket("t1"); !ok || got.token != "jwt" {
		t.Fatal("the exchange code did not work the first time")
	}
	if _, ok := takeTicket("t1"); ok {
		t.Error("the exchange code worked a second time")
	}
	putTicket("t2", oidcTicket{token: "jwt", created: time.Now().Add(-2 * oidcTicketTTL)})
	if _, ok := takeTicket("t2"); ok {
		t.Error("an expired exchange code was accepted")
	}
}

// ─── Configuration ────────────────────────────────────────────────────────

// The switch is off by default, and a missing setting is named explicitly:
// an administrator reads the answer instead of guessing from the log.
func TestOIDCSettings(t *testing.T) {
	envs := []string{"OIDC_ENABLED", "OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_REDIRECT_URL", "OIDC_SCOPES", "OIDC_USERNAME_CLAIM"}
	for _, e := range envs {
		os.Unsetenv(e)
	}
	defer func() {
		for _, e := range envs {
			os.Unsetenv(e)
		}
	}()

	if oidcEnabled() {
		t.Fatal("provider sign-in is enabled without an explicit variable")
	}
	if _, p := oidcSettings(); !strings.Contains(p, "disabled") {
		t.Errorf("the reason must name the switch that is off: %q", p)
	}

	os.Setenv("OIDC_ENABLED", "true")
	for _, step := range []struct{ set, wantMissing string }{
		{"", "OIDC_ISSUER"},
		{"OIDC_ISSUER=https://sso.example.com/realms/x", "OIDC_CLIENT_ID"},
		{"OIDC_CLIENT_ID=hexeris", "OIDC_CLIENT_SECRET"},
		{"OIDC_CLIENT_SECRET=s3cret", "OIDC_REDIRECT_URL"},
	} {
		if step.set != "" {
			kv := strings.SplitN(step.set, "=", 2)
			os.Setenv(kv[0], kv[1])
		}
		if _, p := oidcSettings(); !strings.Contains(p, step.wantMissing) {
			t.Errorf("the missing variable %s is not named, got: %q", step.wantMissing, p)
		}
	}

	os.Setenv("OIDC_REDIRECT_URL", "https://hexeris.example.com/auth/oidc/callback")
	cfg, p := oidcSettings()
	if p != "" {
		t.Fatalf("a complete configuration was rejected: %q", p)
	}
	if cfg.Scopes != "openid email profile" || cfg.UsernameClaim != "email" {
		t.Errorf("the defaults were not applied: %+v", cfg)
	}

	// An issuer without a scheme is a common typo and must not be accepted
	// silently: discovery would go nowhere with an obscure network error.
	os.Setenv("OIDC_ISSUER", "sso.example.com/realms/x")
	if _, p := oidcSettings(); p == "" {
		t.Error("an issuer without a scheme was accepted")
	}
	// A trailing slash is normalised, otherwise discovery goes to //.well-known.
	os.Setenv("OIDC_ISSUER", "https://sso.example.com/realms/x/")
	if cfg, _ := oidcSettings(); strings.HasSuffix(cfg.Issuer, "/") {
		t.Error("the trailing slash in the issuer was not removed")
	}
}

// The PKCE transform must be S256 and must change with the verifier.
func TestPKCEChallenge(t *testing.T) {
	a := pkceChallenge("verifier-one")
	b := pkceChallenge("verifier-two")
	if a == b {
		t.Fatal("different verifiers produced the same challenge")
	}
	if a != pkceChallenge("verifier-one") {
		t.Fatal("the transform is not deterministic")
	}
	if strings.ContainsAny(a, "+/=") {
		t.Errorf("the challenge is not unpadded base64url: %q", a)
	}
	// The known value from RFC 7636, appendix B: a guarantee that this really is
	// S256 and not "some hash or other".
	const rfcVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const rfcChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := pkceChallenge(rfcVerifier); got != rfcChallenge {
		t.Errorf("does not match the RFC 7636 example: got %q", got)
	}
}
