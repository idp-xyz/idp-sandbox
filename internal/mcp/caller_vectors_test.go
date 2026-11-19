package mcp

// The conformance vectors of the caller assertion (idp-mcp ADR-0001 D44): the file idp-mcp generates with its real
// signer, published as idp-contracts/vectors/caller-assertion/v1.json and kept byte-identical in idp-mcp, idp-sandbox
// and idp-web by idp-contracts/tools/validate.py. Every case is decided twice, by the verifier itself and by the
// server's tools/call, and must come out as the file says: the same caller for an acceptance, a refusal for the stated
// reason otherwise. The README next to the published file describes the format.

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const callerVectorsFormat = "idp.telos.caller-assertion-vectors/v1"

type callerVectors struct {
	Format   string `json:"format"`
	Audience string `json:"audience"`
	Keys     []struct {
		Name string `json:"name"`
		PEM  string `json:"pem"`
	} `json:"keys"`
	Cases []struct {
		Name  string `json:"name"`
		Steps []struct {
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
			Now    string                     `json:"now"`
			Expect struct {
				Outcome string `json:"outcome"`
				Caller  *struct {
					Subject string   `json:"subject"`
					Tenant  string   `json:"tenant"`
					Roles   []string `json:"roles"`
					Client  string   `json:"client"`
					Actor   string   `json:"actor"`
				} `json:"caller"`
				Reason string `json:"reason"`
			} `json:"expect"`
		} `json:"steps"`
	} `json:"cases"`
}

// What this verifier says for each reason of the vocabulary. A refusal must match its own reason and no other, so the
// reasons stay told apart: a verifier that refused an expired assertion as a bad signature would not pass.
var callerVectorReasons = map[string][]string{
	"no_assertion":     {"no assertion"},
	"malformed":        {"not a compact JWS", "not base64url", "claims are not an object"},
	"header":           {"header alg"},
	"unknown_key":      {"does not hold"},
	"bad_signature":    {"signature"},
	"audience":         {", not for "},
	"expired":          {"expired"},
	"issued_in_future": {"future"},
	"lifetime":         {"too long"},
	"missing_claim":    {"no subject or id"},
	"request_mismatch": {"another request"},
	"replay":           {"presented before"},
}

func reasonsOf(err error) []string {
	var out []string
	for reason, words := range callerVectorReasons {
		for _, w := range words {
			if strings.Contains(err.Error(), w) {
				out = append(out, reason)
				break
			}
		}
	}
	return out
}

func TestEveryCaseOfTheCallerAssertionVectorsComesOutAsTheFileSays(t *testing.T) {
	raw, err := os.ReadFile("testdata/caller-assertion-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f callerVectors
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Format != callerVectorsFormat || len(f.Keys) == 0 || len(f.Cases) == 0 || f.Audience == "" {
		t.Fatalf("not a file of format %s", callerVectorsFormat)
	}
	var keys []byte
	for _, k := range f.Keys {
		keys = append(keys, k.PEM...)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			// one verifier for the verifier path and one behind a server for the other: each has its own table of
			// the ids it has seen, so a replay in the file is a replay for both
			var now time.Time
			direct, err := NewCallerVerifier(keys, f.Audience)
			if err != nil {
				t.Fatal(err)
			}
			direct.now = func() time.Time { return now }
			behind, _ := NewCallerVerifier(keys, f.Audience)
			behind.now = func() time.Time { return now }
			var got Caller
			ran := false
			var tools []Tool
			names := map[string]bool{}
			for _, s := range c.Steps {
				var name string
				_ = json.Unmarshal(s.Params["name"], &name)
				if name != "" && !names[name] {
					names[name] = true
					tools = append(tools, Tool{Name: name, InputSchema: map[string]any{"type": "object"},
						Call: func(r *http.Request, _ map[string]json.RawMessage) Result {
							got, ran = CallerFrom(r), true
							return Result{Text: "ran"}
						}})
				}
			}
			srv := New("t", "1", "", tools)
			srv.Callers = behind

			for i, s := range c.Steps {
				where := c.Name + " step " + string(rune('1'+i))
				if now, err = time.Parse(time.RFC3339, s.Now); err != nil {
					t.Fatalf("%s: %v", where, err)
				}
				caller, err := direct.Verify(assertionOf(s.Params), s.Method, s.Params)
				switch s.Expect.Outcome {
				case "accept":
					want := s.Expect.Caller
					if err != nil || caller.Subject != want.Subject || caller.Tenant != want.Tenant || caller.Client != want.Client || caller.Actor != want.Actor ||
						strings.Join(caller.Roles, ",") != strings.Join(want.Roles, ",") || caller.Roles == nil {
						t.Fatalf("%s: want %+v accepted, got %+v and %v", where, *want, caller, err)
					}
				case "refuse":
					if got := reasonsOf(orNone(err)); err == nil || len(got) != 1 || got[0] != s.Expect.Reason {
						t.Fatalf("%s: want a refusal for %q, got %v (reasons %v)", where, s.Expect.Reason, err, got)
					}
				default:
					t.Fatalf("%s: outcome %q", where, s.Expect.Outcome)
				}

				// the same step through the server, where a call is a tools/call: the verdict is the same, and a
				// refusal means the tool did not run
				if s.Method != "tools/call" {
					continue
				}
				got, ran = Caller{}, false
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": "tools/call", "params": s.Params})
				w, m := post(t, srv, map[string]string{"MCP-Protocol-Version": ProtocolVersion}, string(body))
				if s.Expect.Outcome == "accept" {
					want := s.Expect.Caller
					if w.Code != 200 || m["error"] != nil || !ran || got.Subject != want.Subject || got.Tenant != want.Tenant || got.Client != want.Client || got.Actor != want.Actor {
						t.Fatalf("%s: the server answered %d %v, the tool saw %+v", where, w.Code, m, got)
					}
				} else if msg, _ := m["error"].(map[string]any); w.Code != http.StatusForbidden || msg == nil || msg["message"] != ErrAssertion.Error() || ran {
					t.Fatalf("%s: the server answered %d %v (the tool ran: %v), want a 403", where, w.Code, m, ran)
				}
			}
		})
	}
}

// assertionOf is what the server reads: _meta["io.idp.telos/caller-assertion"], a string.
func assertionOf(params map[string]json.RawMessage) string {
	var meta map[string]json.RawMessage
	var a string
	_ = json.Unmarshal(params["_meta"], &meta)
	_ = json.Unmarshal(meta[MetaCallerAssertion], &a)
	return a
}

func orNone(err error) error {
	if err == nil {
		return os.ErrNotExist
	}
	return err
}
