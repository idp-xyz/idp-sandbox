package egress

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"
)

// A run that may use the network says which tenant it belongs to with a credential the sandbox service made for it
// (ADR-0001 D21): the proxy picks that tenant's allow-list. The credential is the password of the proxy URL the run gets
// (HTTP_PROXY=http://run:<credential>@<proxy>:<port>), so every HTTP client sends it as Proxy-Authorization: Basic.
//
// It is base64url(claims) "." base64url(HMAC-SHA-256(key, "idp-egress-v1\n" + base64url(claims))). The service and the
// proxy share the key; the code in a run can read its own credential and no other, and it is good until the run's time
// is up. On the Docker backend it is also good only on the address the proxy has on that run's network; on the
// Kubernetes backend every run reaches the proxy on the same address, so there the credential names none (D22).

// User is the user name of the proxy credential. It carries nothing.
const User = "run"

const credentialContext = "idp-egress-v1\n"

// Claims is what a credential says.
type Claims struct {
	Tenant    string `json:"t"`           // whose allow-list applies; "" is a caller without a tenant (the default list)
	Proxy     string `json:"a,omitempty"` // the proxy's address on the run's network: the credential works only there; "" is anywhere
	Expires   int64  `json:"e"`           // Unix seconds
	Workspace string `json:"w,omitempty"` // a short hash of the workspace, for the proxy's log
}

var (
	ErrNoCredential  = errors.New("no credential")
	ErrBadCredential = errors.New("the credential is not one this service made")
	ErrExpired       = errors.New("the credential has expired")
	ErrOtherNetwork  = errors.New("the credential was made for another run's network")
)

var b64 = base64.RawURLEncoding

func sign(key []byte, payload string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(credentialContext + payload))
	return m.Sum(nil)
}

// Mint makes the credential of one run.
func Mint(key []byte, c Claims) (string, error) {
	if len(key) < 32 {
		return "", errors.New("the egress key is shorter than 32 bytes")
	}
	if c.Proxy != "" {
		if _, err := netip.ParseAddr(c.Proxy); err != nil {
			return "", fmt.Errorf("the proxy's address: %w", err)
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payload := b64.EncodeToString(b)
	return payload + "." + b64.EncodeToString(sign(key, payload)), nil
}

// Verify checks a credential presented on a connection that arrived at the proxy's address local.
func Verify(key []byte, credential string, now time.Time, local netip.Addr) (Claims, error) {
	if credential == "" {
		return Claims{}, ErrNoCredential
	}
	payload, sig, ok := strings.Cut(credential, ".")
	if !ok {
		return Claims{}, ErrBadCredential
	}
	got, err := b64.DecodeString(sig)
	if err != nil || !hmac.Equal(got, sign(key, payload)) {
		return Claims{}, ErrBadCredential
	}
	b, err := b64.DecodeString(payload)
	if err != nil {
		return Claims{}, ErrBadCredential
	}
	var c Claims
	if err := json.Unmarshal(b, &c); err != nil {
		return Claims{}, ErrBadCredential
	}
	if now.Unix() >= c.Expires {
		return Claims{}, ErrExpired
	}
	if c.Proxy != "" { // the service tied it to one address of the proxy
		if a, err := netip.ParseAddr(c.Proxy); err != nil || a.Unmap() != local.Unmap() {
			return Claims{}, ErrOtherNetwork
		}
	}
	return c, nil
}

// LoadKey reads the key the sandbox service and the proxy share: the file's content without surrounding white space, at
// least 32 bytes (for example `openssl rand -hex 32`).
func LoadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key := []byte(strings.TrimSpace(string(b)))
	if len(key) < 32 {
		return nil, fmt.Errorf("%s: the key is shorter than 32 bytes", path)
	}
	return key, nil
}
