package egress

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestACredentialSaysTheTenantOnTheRunsNetworkUntilItExpires(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	local := netip.MustParseAddr("172.20.0.2")
	cred, err := Mint(testKey, Claims{Tenant: "acme", Proxy: "172.20.0.2", Expires: now.Add(time.Minute).Unix(), Workspace: "0a1b2c3d"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(cred, "@:/ +=") {
		t.Fatalf("the credential does not fit the password of a URL as it is: %q", cred)
	}
	c, err := Verify(testKey, cred, now, local)
	if err != nil || c.Tenant != "acme" || c.Workspace != "0a1b2c3d" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := Verify(testKey, cred, now, netip.MustParseAddr("::ffff:172.20.0.2")); err != nil {
		t.Fatalf("the same address, IPv4-mapped: %v", err)
	}
	payload, sig, _ := strings.Cut(cred, ".")
	other, _ := Mint(testKey, Claims{Tenant: "globex", Proxy: "172.20.0.2", Expires: now.Add(time.Minute).Unix()})
	otherPayload, _, _ := strings.Cut(other, ".")
	for name, c := range map[string]struct {
		cred  string
		key   []byte
		now   time.Time
		local netip.Addr
		want  error
	}{
		"none":                     {"", testKey, now, local, ErrNoCredential},
		"not a credential":         {"hunter2", testKey, now, local, ErrBadCredential},
		"another tenant's claims":  {otherPayload + "." + sig, testKey, now, local, ErrBadCredential},
		"a signature cut short":    {payload + "." + sig[:len(sig)-2], testKey, now, local, ErrBadCredential},
		"another key":              {cred, []byte("ffffffffffffffffffffffffffffffff"), now, local, ErrBadCredential},
		"at its expiry":            {cred, testKey, now.Add(time.Minute), local, ErrExpired},
		"on another run's network": {cred, testKey, now, netip.MustParseAddr("172.21.0.2"), ErrOtherNetwork},
		"on an address not known":  {cred, testKey, now, netip.Addr{}, ErrOtherNetwork},
		"signed, but no JSON":      {b64.EncodeToString([]byte("x")) + "." + b64.EncodeToString(sign(testKey, b64.EncodeToString([]byte("x")))), testKey, now, local, ErrBadCredential},
	} {
		if _, err := Verify(c.key, c.cred, c.now, c.local); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

// D22: on Kubernetes every run reaches the proxy on one address, so the service names none. Such a credential is still
// the service's, and still ends; and a run cannot turn its own, tied to an address, into one that is not.
func TestACredentialThatNamesNoAddressIsGoodOnAny(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cred, err := Mint(testKey, Claims{Tenant: "acme", Expires: now.Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	for _, local := range []netip.Addr{netip.MustParseAddr("192.168.82.7"), netip.MustParseAddr("fd00::7"), {}} {
		if c, err := Verify(testKey, cred, now, local); err != nil || c.Tenant != "acme" || c.Proxy != "" {
			t.Errorf("on %v: %+v %v", local, c, err)
		}
	}
	if _, err := Verify(testKey, cred, now.Add(time.Minute), netip.MustParseAddr("192.168.82.7")); !errors.Is(err, ErrExpired) {
		t.Errorf("past its time: %v", err)
	}
	if _, err := Verify([]byte("ffffffffffffffffffffffffffffffff"), cred, now, netip.Addr{}); !errors.Is(err, ErrBadCredential) {
		t.Errorf("another key: %v", err)
	}
	tied, _ := Mint(testKey, Claims{Tenant: "acme", Proxy: "172.20.0.2", Expires: now.Add(time.Minute).Unix()})
	_, sig, _ := strings.Cut(tied, ".")
	free, _, _ := strings.Cut(cred, ".")
	if _, err := Verify(testKey, free+"."+sig, now, netip.MustParseAddr("172.21.0.2")); !errors.Is(err, ErrBadCredential) {
		t.Errorf("the claims of a credential without an address under the signature of one with: %v", err)
	}
}

func TestMintAndLoadKeyRefuseWhatCannotWork(t *testing.T) {
	if _, err := Mint([]byte("short"), Claims{Proxy: "10.0.0.1"}); err == nil {
		t.Error("a short key signed")
	}
	if _, err := Mint(testKey, Claims{Proxy: "proxy"}); err == nil {
		t.Error("a credential for a name instead of an address")
	}
	dir := t.TempDir()
	good, short := filepath.Join(dir, "good"), filepath.Join(dir, "short")
	_ = os.WriteFile(good, []byte("  "+string(testKey)+"\n"), 0o600)
	_ = os.WriteFile(short, []byte("abc\n"), 0o600)
	if k, err := LoadKey(good); err != nil || string(k) != string(testKey) {
		t.Errorf("%q %v", k, err)
	}
	if _, err := LoadKey(short); err == nil {
		t.Error("a short key was loaded")
	}
	if _, err := LoadKey(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing key was loaded")
	}
}

func TestTenantsFile(t *testing.T) {
	def, tenants, err := ParseTenants([]byte(`
default:
  hosts: [PyPI.org, files.pythonhosted.org]
tenants:
  acme:
    hosts: ["*.acme.example", pypi.org]
    ports: [443]
  globex:
    hosts: []
`))
	if err != nil {
		t.Fatal(err)
	}
	if def == nil || !def.Allowed("pypi.org", 80) || def.Allowed("x.acme.example", 443) {
		t.Errorf("default: %+v", def)
	}
	if a := tenants["acme"]; !a.Allowed("x.acme.example", 443) || a.Allowed("pypi.org", 80) || a.Allowed("files.pythonhosted.org", 443) {
		t.Errorf("acme has its own list and port, and nothing of the default: %+v", a)
	}
	if g, ok := tenants["globex"]; !ok || g.Allowed("pypi.org", 443) {
		t.Errorf("globex is named with nothing: %+v %v", g, ok)
	}
	if def, tenants, err := ParseTenants([]byte("tenants: {acme: {hosts: [a.test]}}")); err != nil || def != nil || len(tenants) != 1 {
		t.Errorf("a file without a default: %v %v %v", def, tenants, err)
	}
	if def, tenants, err := ParseTenants([]byte("# nobody yet\n")); err != nil || def != nil || len(tenants) != 0 {
		t.Errorf("an empty file: %v %v %v", def, tenants, err)
	}
	for name, doc := range map[string]string{
		"a key that is not known":   "tenants: {acme: {host: [a.test]}}",
		"a wildcard for everything": "default: {hosts: ['*']}",
		"a wildcard in the middle":  "default: {hosts: ['a.*.test']}",
		"a URL":                     "default: {hosts: ['https://a.test']}",
		"a port out of range":       "default: {hosts: [a.test], ports: [70000]}",
		"not YAML":                  "default: [",
	} {
		if _, _, err := ParseTenants([]byte(doc)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
