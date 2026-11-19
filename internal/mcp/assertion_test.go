package mcp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
	"time"
)

// An assertion made by idp-mcp's gateway (its CallerSigner, at 2026-11-17T00:00:00Z) for the request below, with the
// public half of the key that signed it. The same request and digest are pinned in idp-mcp's
// TestRequestDigestIsTheOneUpstreamsCompute, so the two repositories cannot drift apart unnoticed.
const (
	vectorKey = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEohuazOuwtXVv4bbe+34tuxPRZYT0
MhfvC5orxYuBtlzQDkcbdyY8ArPLyQmVNAZRh6jp2RnXFF/+CLtd4X/dUA==
-----END PUBLIC KEY-----
`
	vectorAssertion = "eyJhbGciOiJFUzI1NiIsImtpZCI6ImdTUDRRczdHbUJKbTdnbmtZSEhvZDRzNWN2YmRUalZfYWhxcUR2Q0F1M1EiLCJ0eXAiOiJpZHAtY2FsbGVyK2p3dCJ9.eyJhY3RvciI6ImlkcC1ob3N0IiwiYXVkIjoiaHR0cDovL3NhbmRib3g6ODU3MC9tY3AiLCJjbGllbnQiOiJpZHAtaG9zdCIsImV4cCI6MTc5NDg3MzY2MCwiaWF0IjoxNzk0ODczNjAwLCJpc3MiOiJpZHAtbWNwLWdhdGV3YXk6ZzEiLCJqdGkiOiJDWWNhYlJPczRBNjZBMFN3IiwicmVxIjoic2hhMjU2Ojg4NTFiNDQ1MTliNmMwNDgwODhiZmQ2NjA5MDU4MWI0MzNiNzQ4OTI1NzUwMWJiMmE5MWUxN2UwZTg0YTViMzMiLCJyb2xlcyI6WyJvcGVyYXRvciJdLCJzdWIiOiJzdmMtb3BlcmF0b3IiLCJ0ZW5hbnRfaWQiOiJ0MSJ9.oSghV4NExUaodJA0St5d2ItxL69AsLxugvttrFzFlL0sbNB2VGC-B2FQwTphO15IsJN7fUZi-GrhyTLuDT2X5w"
	vectorAudience  = "http://sandbox:8570/mcp"
	vectorDigest    = "sha256:8851b44519b6c048088bfd66090581b433b7489257501bb2a91e17e0e84a5b33"
	vectorParams    = `{"name":"sandbox_run","arguments":{"workspaceId":"0123456789abcdef0123456789abcdef","language":"python","code":"print('<a> & é')","timeoutSeconds":5.0},` + meta + `}`
)

var vectorTime = time.Date(2026, 11, 17, 0, 0, 20, 0, time.UTC)

func params(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()
	var p map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func vectorVerifier(t *testing.T) *CallerVerifier {
	t.Helper()
	v, err := NewCallerVerifier([]byte(vectorKey), vectorAudience)
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return vectorTime }
	return v
}

// What the gateway signs, this server accepts: the same canonical digest, the same key id, the same claims.
func TestTheGatewaysAssertionIsAccepted(t *testing.T) {
	if d, err := requestDigest("tools/call", params(t, vectorParams)); err != nil || d != vectorDigest {
		t.Fatalf("digest %s %v, the gateway computes %s", d, err, vectorDigest)
	}
	// the digest is of the JSON value, not of its bytes: other spacing, key order, escapes and _meta do not change it
	respelled := `{ "arguments": {"timeoutSeconds":5.0, "code":"print('\u003ca\u003e \u0026 \u00e9')", "language":"python","workspaceId":"0123456789abcdef0123456789abcdef"}, "_meta":{"x":1}, "name":"sandbox_run"}`
	if d, _ := requestDigest("tools/call", params(t, respelled)); d != vectorDigest {
		t.Fatalf("the same request spelled differently has digest %s", d)
	}
	v := vectorVerifier(t)
	c, err := v.Verify(vectorAssertion, "tools/call", params(t, vectorParams))
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "svc-operator" || c.Tenant != "t1" || len(c.Roles) != 1 || c.Roles[0] != "operator" || c.Client != "idp-host" || c.Actor != "idp-host" || c.Owner() != "t1/svc-operator" {
		t.Fatalf("%+v", c)
	}
	// once
	if _, err := v.Verify(vectorAssertion, "tools/call", params(t, vectorParams)); err == nil || !strings.Contains(err.Error(), "before") {
		t.Fatalf("a replayed assertion: %v", err)
	}
}

// forge makes an assertion with a key of the test's own: what someone on the sandbox's network could do.
func forge(t *testing.T, key *ecdsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	input := enc(h) + "." + enc(c)
	sum := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + enc(sig)
}

func TestForgedAndMalformedAssertionsAreRefused(t *testing.T) {
	mine, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&mine.PublicKey)
	mineKid, _ := keyID(&mine.PublicKey)
	gatewayKid := vectorVerifier(t).KeyIDs()[0]
	p := params(t, vectorParams)
	claims := func() map[string]any {
		return map[string]any{"aud": vectorAudience, "iat": vectorTime.Unix(), "exp": vectorTime.Add(time.Minute).Unix(), "jti": "j1",
			"sub": "admin", "tenant_id": "t1", "roles": []string{"admin"}, "req": vectorDigest}
	}
	head := func(kid string) map[string]any {
		return map[string]any{"alg": "ES256", "typ": assertionType, "kid": kid}
	}

	// a well-formed assertion signed with a key the server holds is accepted (so the refusals below are for their reasons)
	both, err := NewCallerVerifier(append([]byte(vectorKey), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})...), vectorAudience)
	if err != nil || len(both.KeyIDs()) != 2 {
		t.Fatalf("two keys (rotation): %v", err)
	}
	both.now = func() time.Time { return vectorTime }
	if c, err := both.Verify(forge(t, mine, head(mineKid), claims()), "tools/call", p); err != nil || c.Subject != "admin" {
		t.Fatalf("%v %+v", err, c)
	}
	if _, err := both.Verify(vectorAssertion, "tools/call", p); err != nil {
		t.Fatalf("the other key of the two: %v", err)
	}

	v := vectorVerifier(t)
	refused := map[string]string{
		"unknown key":             forge(t, mine, head(mineKid), claims()),
		"own key, gateway's kid":  forge(t, mine, head(gatewayKid), claims()),
		"alg none":                base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"idp-caller+jwt","kid":"`+gatewayKid+`"}`)) + "." + strings.Split(vectorAssertion, ".")[1] + ".",
		"another typ":             forge(t, mine, map[string]any{"alg": "ES256", "typ": "JWT", "kid": mineKid}, claims()),
		"claims swapped":          strings.Split(vectorAssertion, ".")[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"admin"}`)) + "." + strings.Split(vectorAssertion, ".")[2],
		"signature of wrong size": strings.Split(vectorAssertion, ".")[0] + "." + strings.Split(vectorAssertion, ".")[1] + ".AAAA",
		"not a JWS":               "attachment:1",
		"empty":                   "",
		"too long":                vectorAssertion + strings.Repeat("A", maxAssertion),
	}
	for name, a := range refused {
		if _, err := v.Verify(a, "tools/call", p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// claims the gateway never makes, signed with a held key
	for name, change := range map[string]func(map[string]any){
		"no subject":      func(c map[string]any) { c["sub"] = "" },
		"no id":           func(c map[string]any) { delete(c, "jti") },
		"no expiry":       func(c map[string]any) { delete(c, "exp") },
		"valid for a day": func(c map[string]any) { c["exp"] = vectorTime.Add(24 * time.Hour).Unix() },
		"no request":      func(c map[string]any) { delete(c, "req") },
	} {
		c := claims()
		c["jti"] = name
		change(c)
		if _, err := both.Verify(forge(t, mine, head(mineKid), c), "tools/call", p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewCallerVerifier([]byte(vectorKey), ""); err == nil {
		t.Error("a verifier without an audience")
	}
	if _, err := NewCallerVerifier([]byte("no key here"), vectorAudience); err == nil {
		t.Error("a verifier without a key")
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	d384, _ := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	if _, err := NewCallerVerifier(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: d384}), vectorAudience); err == nil {
		t.Error("a P-384 key")
	}
}

// With a verifier the server takes the caller from the assertion only: the unsigned caller a gateway without a key (or
// anyone else on the network) sends names nobody.
func TestTheServerTakesTheCallerFromTheAssertionOnly(t *testing.T) {
	var got Caller
	s := New("t", "1", "", []Tool{{Name: "sandbox_run", InputSchema: map[string]any{"type": "object"},
		Call: func(r *http.Request, _ map[string]json.RawMessage) Result {
			got = CallerFrom(r)
			return Result{Text: "ran"}
		}}})
	s.Callers = vectorVerifier(t)
	h := map[string]string{"MCP-Protocol-Version": ProtocolVersion}
	unsigned := strings.Replace(vectorParams, `"_meta":{`, `"_meta":{"io.idp.telos/caller":{"subject":"admin","tenant":"t1","roles":["admin"]},`, 1)
	w, m := post(t, s, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+unsigned+`}`)
	if w.Code != http.StatusForbidden || m["error"] == nil || got.Subject != "" || strings.Contains(w.Body.String(), "ran") {
		t.Fatalf("an unsigned caller ran the tool: %d %v %+v", w.Code, m, got)
	}
	if msg := m["error"].(map[string]any)["message"]; msg != ErrAssertion.Error() {
		t.Fatalf("the refusal says %q", msg)
	}
	signed := strings.Replace(unsigned, `"_meta":{`, `"_meta":{"io.idp.telos/caller-assertion":"`+vectorAssertion+`",`, 1)
	w, m = post(t, s, h, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":`+signed+`}`)
	if w.Code != 200 || m["error"] != nil || got.Subject != "svc-operator" || got.Tenant != "t1" {
		t.Fatalf("%d %v: the caller is %+v, not the one the gateway signed", w.Code, m, got)
	}
	// the listing needs no caller: the gateway's drift check has none
	if w, m := post(t, s, h, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{`+meta+`}}`); w.Code != 200 || m["result"] == nil {
		t.Fatalf("%d %v", w.Code, m)
	}
	// without a verifier nothing changes: the forwarded caller is taken as it arrives
	got = Caller{}
	s.Callers = nil
	if w, _ := post(t, s, h, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":`+unsigned+`}`); w.Code != 200 || got.Subject != "admin" {
		t.Fatalf("%d %+v", w.Code, got)
	}
}
