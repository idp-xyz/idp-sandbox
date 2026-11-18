package mcp

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// MetaCallerAssertion is where idp-mcp's gateway puts its signature over the caller (idp-mcp ADR-0001 D44): a compact
// JWS (ES256, typ idp-caller+jwt) naming the caller, this server's endpoint as the audience, a digest of the request
// and a lifetime of a minute. With the gateway's public key this server no longer has to trust its network to know
// that the caller came from the gateway (ADR-0001 D18).
const MetaCallerAssertion = "io.idp.telos/caller-assertion"

const (
	assertionType   = "idp-caller+jwt"
	assertionSkew   = 30 * time.Second // the gateway's clock and this one may differ by this much
	assertionMaxTTL = 5 * time.Minute  // an assertion valid for longer is not one the gateway makes
	maxAssertion    = 8192
)

// ErrAssertion: the request carries no assertion this server accepts. The reason goes to the log, not to the caller.
var ErrAssertion = errors.New("the caller assertion is missing or invalid")

// CallerVerifier checks the gateway's assertions. Keys are the gateway's public keys (several while one is rotated),
// audience is this server's endpoint as it is registered in idp-mcp.
type CallerVerifier struct {
	keys     map[string]*ecdsa.PublicKey // by RFC 7638 thumbprint, the assertion's kid
	audience string
	now      func() time.Time

	mu   sync.Mutex
	seen map[string]int64 // jti -> when it may be forgotten (unix seconds)
}

// NewCallerVerifier reads one or more PEM public keys (PKIX, P-256).
func NewCallerVerifier(pemBytes []byte, audience string) (*CallerVerifier, error) {
	if audience == "" {
		return nil, errors.New("no audience: the endpoint this server is registered under in idp-mcp")
	}
	v := &CallerVerifier{keys: map[string]*ecdsa.PublicKey{}, audience: audience, now: time.Now, seen: map[string]int64{}}
	for rest := pemBytes; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("not a PKIX public key: %w", err)
		}
		pub, ok := k.(*ecdsa.PublicKey)
		if !ok || pub.Curve != elliptic.P256() {
			return nil, errors.New("the key is not an EC key on P-256")
		}
		kid, err := keyID(pub)
		if err != nil {
			return nil, err
		}
		v.keys[kid] = pub
	}
	if len(v.keys) == 0 {
		return nil, errors.New("no public key found")
	}
	return v, nil
}

// KeyIDs of the keys this verifier accepts, for the operator's log.
func (v *CallerVerifier) KeyIDs() []string {
	out := make([]string, 0, len(v.keys))
	for k := range v.keys {
		out = append(out, k)
	}
	return out
}

func keyID(pub *ecdsa.PublicKey) (string, error) {
	raw, err := pub.Bytes() // 0x04 || X || Y
	if err != nil || len(raw) != 65 {
		return "", errors.New("not an uncompressed P-256 point")
	}
	enc := base64.RawURLEncoding.EncodeToString
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + enc(raw[1:33]) + `","y":"` + enc(raw[33:]) + `"}`))
	return enc(sum[:]), nil
}

// canonical is idp-mcp's canonical JSON (release.Canonical, copied: ADR-0001 D10): keys sorted, no insignificant
// whitespace, no HTML escaping, numbers as written.
func canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// requestDigest is what the gateway signed about the request: the method and the params without _meta.
func requestDigest(method string, params map[string]json.RawMessage) (string, error) {
	p := make(map[string]json.RawMessage, len(params))
	for k, v := range params {
		if k != "_meta" {
			p[k] = v
		}
	}
	b, err := canonical(map[string]any{"method": method, "params": p})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type assertionClaims struct {
	Audience any      `json:"aud"`
	Issued   int64    `json:"iat"`
	Expires  int64    `json:"exp"`
	ID       string   `json:"jti"`
	Subject  string   `json:"sub"`
	Tenant   string   `json:"tenant_id"`
	Roles    []string `json:"roles"`
	Client   string   `json:"client"`
	Actor    string   `json:"actor"`
	Request  string   `json:"req"`
}

func (c assertionClaims) forAudience(aud string) bool {
	switch a := c.Audience.(type) {
	case string:
		return a == aud
	case []any:
		for _, x := range a {
			if s, _ := x.(string); s == aud {
				return true
			}
		}
	}
	return false
}

// Verify returns the caller an assertion names, if the gateway signed it for this server and for this request, it has
// not expired, and it was not presented before. The error says why not (for the log).
func (v *CallerVerifier) Verify(assertion, method string, params map[string]json.RawMessage) (Caller, error) {
	if assertion == "" {
		return Caller{}, errors.New("no assertion")
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 || len(assertion) > maxAssertion {
		return Caller{}, errors.New("not a compact JWS")
	}
	dec := base64.RawURLEncoding.DecodeString
	hraw, err1 := dec(parts[0])
	craw, err2 := dec(parts[1])
	sig, err3 := dec(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return Caller{}, errors.New("not base64url")
	}
	var h struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(hraw, &h) != nil || h.Alg != "ES256" || h.Typ != assertionType {
		return Caller{}, fmt.Errorf("header alg %q typ %q", h.Alg, h.Typ)
	}
	key := v.keys[h.Kid]
	if key == nil {
		return Caller{}, fmt.Errorf("signed with a key this server does not hold (kid %q)", h.Kid)
	}
	if len(sig) != 64 {
		return Caller{}, errors.New("signature length")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return Caller{}, errors.New("signature does not verify")
	}
	var c assertionClaims
	if json.Unmarshal(craw, &c) != nil {
		return Caller{}, errors.New("claims are not an object")
	}
	now := v.now()
	switch {
	case !c.forAudience(v.audience):
		return Caller{}, fmt.Errorf("made for %v, not for %s", c.Audience, v.audience)
	case c.Expires == 0 || now.After(time.Unix(c.Expires, 0).Add(assertionSkew)):
		return Caller{}, errors.New("expired")
	case c.Issued == 0 || time.Unix(c.Issued, 0).After(now.Add(assertionSkew)):
		return Caller{}, errors.New("issued in the future")
	case c.Expires-c.Issued > int64(assertionMaxTTL/time.Second) || c.Expires < c.Issued:
		return Caller{}, errors.New("valid for too long")
	case c.Subject == "" || c.ID == "":
		return Caller{}, errors.New("no subject or id")
	}
	want, err := requestDigest(method, params)
	if err != nil || c.Request != want {
		return Caller{}, errors.New("made for another request")
	}
	if !v.first(c.ID, c.Expires+int64(assertionSkew/time.Second), now.Unix()) {
		return Caller{}, errors.New("presented before")
	}
	roles := c.Roles
	if roles == nil {
		roles = []string{}
	}
	return Caller{Subject: c.Subject, Tenant: c.Tenant, Roles: roles, Client: c.Client, Actor: c.Actor}, nil
}

// first records id until forget and reports whether it had not been seen. The table holds at most the assertions of the
// last minute and a half; it is cleared of expired ids whenever it has grown.
func (v *CallerVerifier) first(id string, forget, now int64) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, dup := v.seen[id]; dup {
		return false
	}
	if len(v.seen) >= 4096 {
		for k, t := range v.seen {
			if t < now {
				delete(v.seen, k)
			}
		}
	}
	v.seen[id] = forget
	return true
}
