package main

// The one-time code algorithm.
//
// The main check here is the reference values from RFC 6238 (Appendix B). A
// home-grown TOTP fails quietly: codes are generated, they look plausible, and
// the divergence is only discovered when an employee cannot sign in with their
// Google Authenticator. Checking against the reference is the only thing that
// separates "works" from "emits six digits".

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// rfcSecret — "12345678901234567890" from RFC 6238, in base32.
var rfcSecret = base32.StdEncoding.WithPadding(base32.NoPadding).
	EncodeToString([]byte("12345678901234567890"))

// THE MAIN POINT: matching the RFC 6238 reference for SHA-1.
//
// The Appendix B values are trimmed to six digits — the RFC table gives them
// as eight, while apps show six, i.e. the last six.
func TestTOTPMatchesRFC6238(t *testing.T) {
	cases := []struct {
		unix int64
		want string // the last 6 digits of the reference 8-digit code
	}{
		{59, "287082"},          // 94287082
		{1111111109, "081804"},  // 07081804
		{1111111111, "050471"},  // 14050471
		{1234567890, "005924"},  // 89005924
		{2000000000, "279037"},  // 69279037
		{20000000000, "353130"}, // 65353130
	}
	for _, c := range cases {
		got, err := totpCodeAt(rfcSecret, c.unix/totpStepSeconds)
		if err != nil {
			t.Fatalf("t=%d: %v", c.unix, err)
		}
		if got != c.want {
			t.Errorf("t=%d: code %s, RFC reference %s — apps will not accept this code",
				c.unix, got, c.want)
		}
	}
}

func TestTOTPSecretIsUsableAndUnique(t *testing.T) {
	a, err := totpNewSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := totpNewSecret()
	if a == b {
		t.Fatal("two calls gave the same secret — the second factor would be shared")
	}
	if len(a) != 32 {
		t.Errorf("secret length %d characters, expected 32 (160 bits in base32)", len(a))
	}
	if strings.ContainsAny(a, "=") {
		t.Error("the secret contains a padding character — apps show it to the user")
	}
	if _, err := totpCodeAt(a, 1); err != nil {
		t.Fatalf("a fresh secret cannot be used to compute a code: %v", err)
	}
}

// The tolerance is a minute and a half, no more and no less. Less, and sign-in
// breaks on phone clock drift; more, and a glimpsed code lives too long.
func TestTOTPAcceptsSkewWindowAndNothingBeyond(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cur := totpStep(now)

	for _, d := range []int64{-1, 0, 1} {
		code, _ := totpCodeAt(rfcSecret, cur+d)
		step, ok := totpVerify(rfcSecret, code, now)
		if !ok {
			t.Errorf("a code from window %+d was rejected — sign-in breaks on clock drift", d)
		}
		if step != cur+d {
			t.Errorf("the window was determined as %d, expected %d", step, cur+d)
		}
	}
	for _, d := range []int64{-2, 2, 10, -100} {
		code, _ := totpCodeAt(rfcSecret, cur+d)
		if _, ok := totpVerify(rfcSecret, code, now); ok {
			t.Errorf("a code from window %+d was accepted — it expired long ago", d)
		}
	}
}

func TestTOTPRejectsMalformedInput(t *testing.T) {
	now := time.Unix(1700000000, 0)
	valid, _ := totpCodeAt(rfcSecret, totpStep(now))

	for _, bad := range []string{"", "12345", "1234567", "abcdef", valid + "0", "0" + valid} {
		if _, ok := totpVerify(rfcSecret, bad, now); ok {
			t.Errorf("the invalid code %q was accepted", bad)
		}
	}
	// Apps display the spaces themselves ("123 456") and people copy them along
	// with the code — that is not a typing error.
	spaced := valid[:3] + " " + valid[3:]
	if _, ok := totpVerify(rfcSecret, spaced, now); !ok {
		t.Errorf("the spaced code %q was rejected — that is exactly how apps show it", spaced)
	}
	// A different secret does not work.
	other, _ := totpNewSecret()
	if _, ok := totpVerify(other, valid, now); ok {
		t.Error("a code from another secret was accepted")
	}
}

// The app URI: if it is built wrongly the account is added, codes are
// produced — and do not work. Diagnosing that afterwards is practically
// impossible.
func TestTOTPURIIsScannable(t *testing.T) {
	uri := totpURI("chat.example.com", "ivanov", rfcSecret)

	if !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatalf("the wrong scheme: %q", uri)
	}
	for _, want := range []string{
		"secret=" + rfcSecret,
		"issuer=chat.example.com",
		"algorithm=SHA1",
		"digits=6",
		"period=30",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("the URI has no %q: %s", want, uri)
		}
	}
	// The issuer in the label — old apps read only that.
	if !strings.Contains(uri, "chat.example.com:ivanov") {
		t.Errorf("the issuer is not duplicated in the label: %s", uri)
	}
}
