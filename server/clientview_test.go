package main

// What the server sees about a client — and why exactly that.
//
// The "everyone is 127.0.0.1 in the sign-in log" fault is fixed by editing the
// configuration on the machine whose proxy sits in front of THIS application.
// With no feedback it is easy to edit the wrong location, the wrong file or
// even the wrong machine, and that is only verified later and indirectly. So
// what is checked here is not only the address but the hint: it is the only
// thing that points a person at the right place.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func clientViewFor(remote string, headers map[string]string) map[string]any {
	req := httptest.NewRequest(http.MethodGet, "/healthz?v=1", nil)
	req.RemoteAddr = remote
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return clientView(req)
}

func TestClientViewExplainsWhatServerSees(t *testing.T) {
	origTrusted := trustedProxyIPs
	t.Cleanup(func() { trustedProxyIPs = origTrusted })
	trustedProxyIPs = map[string]bool{"127.0.0.1": true}

	// 1. The proxy is trusted but does not pass the client address — the very
	//    fault. The hint must name the header: without it "ip=127.0.0.1" reports
	//    the symptom and says nothing about the cause.
	v := clientViewFor("127.0.0.1:5000", nil)
	if v["ip"] != "127.0.0.1" {
		t.Fatalf("with no headers the address must be the proxy address, got %v", v["ip"])
	}
	note, _ := v["note"].(string)
	if !strings.Contains(note, "X-Forwarded-For") {
		t.Fatalf("the hint does not name the header: %q", note)
	}
	if !strings.Contains(note, "THIS app") {
		t.Fatalf("the hint does not say WHICH application's proxy to fix: %q", note)
	}

	// 2. The header came from a trusted proxy — take the client address.
	v = clientViewFor("127.0.0.1:5000", map[string]string{"X-Forwarded-For": "203.0.113.55"})
	if v["ip"] != "203.0.113.55" {
		t.Fatalf("the address from XFF was not picked up: %v", v["ip"])
	}
	if v["proxy_trusted"] != true {
		t.Fatal("a trusted proxy is marked as untrusted")
	}

	// 3. A proxy chain: the client is FIRST in the list, the rest are transit.
	v = clientViewFor("127.0.0.1:5000",
		map[string]string{"X-Forwarded-For": "203.0.113.55, 5.129.234.70"})
	if v["ip"] != "203.0.113.55" {
		t.Fatalf("the chain yielded %v rather than the client", v["ip"])
	}

	// 4. A header from an UNtrusted source is ignored — otherwise anyone could
	//    claim a different address and bypass ADMIN_ALLOWED_IPS. The hint must
	//    then explain why the header did not take effect: silence here looks
	//    like "the server is broken".
	v = clientViewFor("203.0.113.9:5000", map[string]string{"X-Forwarded-For": "10.0.0.1"})
	if v["ip"] != "203.0.113.9" {
		t.Fatalf("a header from an untrusted source was believed: %v", v["ip"])
	}
	note, _ = v["note"].(string)
	if !strings.Contains(note, "TRUSTED_PROXY_IPS") {
		t.Fatalf("the hint does not explain why the header was ignored: %q", note)
	}

	// 5. A direct connection with no proxy is its own case, not a "fault".
	v = clientViewFor("203.0.113.9:5000", nil)
	if v["ip"] != "203.0.113.9" {
		t.Fatalf("direct connection: expected the client address, got %v", v["ip"])
	}
	note, _ = v["note"].(string)
	if strings.Contains(note, "X-Forwarded-For") {
		t.Fatalf("a direct connection is advised to fix a proxy: %q", note)
	}

	// The response must reveal nothing beyond the caller's own data.
	for _, k := range []string{"ip", "remote_addr", "x_forwarded_for", "x_real_ip", "proxy_trusted", "note"} {
		if _, ok := v[k]; !ok {
			t.Fatalf("the response has no %q field", k)
		}
	}
	if len(v) != 6 {
		t.Fatalf("the response has %d fields instead of 6 — check nothing extra was added: %v", len(v), v)
	}
}
