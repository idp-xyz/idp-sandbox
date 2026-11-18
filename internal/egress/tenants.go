package egress

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// The proxy's per-tenant allow-lists (ADR-0001 D21), a YAML file the operator keeps:
//
//	default:                 # tenants not named below; without it they get IDP_EGRESS_HOSTS / IDP_EGRESS_PORTS
//	  hosts: [pypi.org, files.pythonhosted.org]
//	tenants:
//	  7b0c0a52-...:          # the tenant id as the gateway forwards it
//	    hosts: [pypi.org, "*.acme.example"]
//	    ports: [443]
//	  globex:
//	    hosts: []            # nothing
//
// A tenant that is named gets its own list and nothing of the default one. Ports left out are 443 and 80.
type tenantsFile struct {
	Default *filePolicy           `yaml:"default"`
	Tenants map[string]filePolicy `yaml:"tenants"`
}

type filePolicy struct {
	Hosts []string `yaml:"hosts"`
	Ports []int    `yaml:"ports"`
}

var hostPattern = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

func (f filePolicy) policy(where string) (Policy, error) {
	p := Policy{Ports: f.Ports}
	for _, h := range f.Hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if !hostPattern.MatchString(h) {
			return Policy{}, fmt.Errorf("%s: %q is not a host name or *.suffix", where, h)
		}
		p.Hosts = append(p.Hosts, h)
	}
	for _, n := range f.Ports {
		if n < 1 || n > 65535 {
			return Policy{}, fmt.Errorf("%s: port %d", where, n)
		}
	}
	return p, nil
}

// ParseTenants reads the per-tenant file: the default list (nil when the file has none) and the tenants' own.
func ParseTenants(b []byte) (*Policy, map[string]Policy, error) {
	var f tenantsFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) { // an empty file names nobody
		return nil, nil, err
	}
	var def *Policy
	if f.Default != nil {
		p, err := f.Default.policy("default")
		if err != nil {
			return nil, nil, err
		}
		def = &p
	}
	tenants := map[string]Policy{}
	for id, fp := range f.Tenants {
		if strings.TrimSpace(id) == "" {
			return nil, nil, fmt.Errorf("a tenant without an id")
		}
		p, err := fp.policy("tenant " + id)
		if err != nil {
			return nil, nil, err
		}
		tenants[id] = p
	}
	return def, tenants, nil
}

// LoadTenants reads the file at path.
func LoadTenants(path string) (*Policy, map[string]Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	def, tenants, err := ParseTenants(b)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return def, tenants, nil
}
