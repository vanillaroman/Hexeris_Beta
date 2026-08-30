package main

// The provider's keys (JWKS) for verifying an ID token signature.
//
// ═══ WHY FORTY LINES OF OUR OWN RATHER THAN A LIBRARY ═════════════════════
//
// A JWKS is JSON with a modulus and an exponent in base64url. Parsing it takes
// as much as calling someone else's code would, and a dependency in a project
// sold on ease of operation and auditability costs more (see the history of
// the binary growing fourfold from one library).
//
// ═══ KEY ROTATION ═════════════════════════════════════════════════════════
//
// Providers change keys without warning. So the cache does not merely live on
// a timer: an unknown kid forces an immediate re-read of the JWKS. Without
// that, a rotation at the customer would mean sign-in breaking for everyone,
// fixable only by restarting the server.

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

type jwkKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwkSet struct {
	Keys []jwkKey `json:"keys"`
}

var (
	jwksMu      sync.Mutex
	jwksCache   map[string]*rsa.PublicKey // kid -> key
	jwksURLUsed string
	jwksUntil   time.Time
	// A guard so a stream of unknown kids does not become a stream of requests
	// to the provider: refreshed at most once a minute.
	jwksLastFetch time.Time
)

const jwksTTL = 12 * time.Hour

// oidcKeyFor returns the key for a kid, re-reading the JWKS if needed.
func oidcKeyFor(ctx context.Context, jwksURL, kid, alg string) (*rsa.PublicKey, error) {
	if !strings.HasPrefix(alg, "RS") {
		return nil, fmt.Errorf("unsupported signing algorithm %q", alg)
	}

	jwksMu.Lock()
	fresh := jwksURLUsed == jwksURL && time.Now().Before(jwksUntil)
	if fresh {
		if k, ok := jwksCache[kid]; ok {
			jwksMu.Unlock()
			return k, nil
		}
		// An unknown key — the provider may have just rotated. A refresh is
		// attempted, but at most once a minute.
		if time.Since(jwksLastFetch) < time.Minute {
			jwksMu.Unlock()
			return nil, fmt.Errorf("unknown signing key %q", kid)
		}
	}
	jwksMu.Unlock()

	keys, err := fetchJWKS(ctx, jwksURL)
	if err != nil {
		return nil, err
	}

	jwksMu.Lock()
	jwksCache, jwksURLUsed = keys, jwksURL
	jwksUntil = time.Now().Add(jwksTTL)
	jwksLastFetch = time.Now()
	k, ok := keys[kid]
	// If no kid is given at all and the provider has exactly one key, take it.
	// Small Keycloak deployments do this, and rejecting them would be formally
	// correct and useless.
	if !ok && kid == "" && len(keys) == 1 {
		for _, only := range keys {
			k, ok = only, true
		}
	}
	jwksMu.Unlock()

	if !ok {
		return nil, fmt.Errorf("the provider has no signing key with id %q", kid)
	}
	return k, nil
}

func fetchJWKS(ctx context.Context, jwksURL string) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := oidcHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot fetch the provider's signing keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the key endpoint answered %d", resp.StatusCode)
	}
	var set jwkSet
	if err := json.NewDecoder(io.LimitReader(resp.Body, 512*1024)).Decode(&set); err != nil {
		return nil, fmt.Errorf("the key set is not valid JSON: %w", err)
	}
	return parseJWKS(set)
}

// parseJWKS is separated from the network so parsing can be tested without
// standing up a provider.
func parseJWKS(set jwkSet) (map[string]*rsa.PublicKey, error) {
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		// Only what can verify a signature is taken. An encryption key
		// (use=enc) is no good for verification, and accepting it silently would
		// mean a failure at the least convenient moment.
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		pub, err := rsaKeyFromJWK(k)
		if err != nil {
			// One unusable key must not break the whole set: a provider may keep
			// keys of other types alongside.
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("the provider published no usable RSA signing keys")
	}
	return out, nil
}

func rsaKeyFromJWK(k jwkKey) (*rsa.PublicKey, error) {
	// base64url WITHOUT padding, as RFC 7518 requires. Some providers do send
	// it with '=', so both forms are accepted.
	dec := func(s string) ([]byte, error) {
		if strings.ContainsAny(s, "=") {
			return base64.URLEncoding.DecodeString(s)
		}
		return base64.RawURLEncoding.DecodeString(s)
	}
	nb, err := dec(k.N)
	if err != nil {
		return nil, fmt.Errorf("modulus: %w", err)
	}
	eb, err := dec(k.E)
	if err != nil {
		return nil, fmt.Errorf("exponent: %w", err)
	}
	if len(nb) == 0 || len(eb) == 0 {
		return nil, errors.New("empty modulus or exponent")
	}
	// A key shorter than 2048 bits must not be accepted: a signature of that
	// length is not considered strong today, and accepting it would mean
	// trusting it completely.
	if len(nb) < 256 {
		return nil, fmt.Errorf("modulus is only %d bits", len(nb)*8)
	}
	e := new(big.Int).SetBytes(eb)
	if !e.IsInt64() || e.Int64() < 3 {
		return nil, errors.New("implausible public exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(e.Int64())}, nil
}
