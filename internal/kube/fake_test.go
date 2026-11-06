package kube

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// outcome is what the fake cluster does with a Job.
type outcome struct {
	stdout, stderr string
	exit           int
	termReason     string // terminated.reason of the container, e.g. OOMKilled
	deadline       bool   // the Job fails with DeadlineExceeded
	hang           bool   // the container keeps running
	noMarker       bool   // the log has no stderr section (killed before it was printed)
	pull           string // the container waits with this reason, for example ImagePullBackOff
}

type obj = map[string]any

// fakeAPI is just enough of a Kubernetes API server: PVC, ConfigMap, Job and Pod create/get/list/delete,
// pod logs and /readyz, with a Job controller that turns each created Job into one pod per the outcome.
type fakeAPI struct {
	t      *testing.T
	srv    *httptest.Server
	token  string
	behave func(job obj, cmd []string) outcome

	mu       sync.Mutex
	objs     map[string]map[string]obj // resource -> name -> object
	calls    []string
	deletes  []string // "resource/name?query"
	logLimit []string
	ready    int
	history  map[string][]obj
}

var routeRE = regexp.MustCompile(`^/(?:api/v1|apis/batch/v1)/namespaces/([^/]+)/(persistentvolumeclaims|configmaps|pods|jobs)(?:/([^/]+)(/log)?)?$`)

const testNS = "ns-test"

func newFake(t *testing.T, behave func(job obj, cmd []string) outcome) *fakeAPI {
	f := &fakeAPI{t: t, token: "tok", behave: behave, ready: 200, history: map[string][]obj{}, objs: map[string]map[string]obj{
		"persistentvolumeclaims": {}, "configmaps": {}, "pods": {}, "jobs": {}}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func status(w http.ResponseWriter, code int, reason string) {
	writeJSON(w, code, obj{"kind": "Status", "status": "Failure", "reason": reason, "message": "fake " + reason, "code": code})
}

func at(o any, path ...string) any {
	for _, p := range path {
		m, ok := o.(map[string]any)
		if !ok {
			return nil
		}
		o = m[p]
	}
	return o
}

func matches(labels map[string]any, selector string) bool {
	for _, term := range strings.Split(selector, ",") {
		if term == "" {
			continue
		}
		k, v, eq := strings.Cut(term, "=")
		got, ok := labels[k]
		if !ok || (eq && got != v) {
			return false
		}
	}
	return true
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		status(w, 401, "Unauthorized")
		return
	}
	if r.URL.Path == "/readyz" {
		w.WriteHeader(f.ready)
		return
	}
	m := routeRE.FindStringSubmatch(r.URL.Path)
	if m == nil || m[1] != testNS {
		status(w, 404, "NotFound")
		return
	}
	res, name, isLog := m[2], m[3], m[4] != ""
	store := f.objs[res]
	switch {
	case r.Method == http.MethodPost && name == "":
		var o obj
		if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
			status(w, 400, "BadRequest")
			return
		}
		n, _ := at(o, "metadata", "name").(string)
		if _, dup := store[n]; dup || n == "" {
			status(w, 409, "AlreadyExists")
			return
		}
		store[n] = o
		f.history[res] = append(f.history[res], o)
		if res == "jobs" {
			f.runJob(o)
		}
		writeJSON(w, 201, o)
	case r.Method == http.MethodGet && name == "":
		var items []any
		names := make([]string, 0, len(store))
		for n := range store {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			labels, _ := at(store[n], "metadata", "labels").(map[string]any)
			if matches(labels, r.URL.Query().Get("labelSelector")) {
				items = append(items, store[n])
			}
		}
		writeJSON(w, 200, obj{"items": items})
	case r.Method == http.MethodGet && isLog:
		o, ok := store[name]
		if !ok {
			status(w, 404, "NotFound")
			return
		}
		text, _ := o["_log"].(string)
		if lim, err := strconv.Atoi(r.URL.Query().Get("limitBytes")); err == nil && lim < len(text) {
			text = text[:lim]
		}
		f.logLimit = append(f.logLimit, r.URL.Query().Get("limitBytes"))
		_, _ = io.WriteString(w, text)
	case r.Method == http.MethodGet:
		o, ok := store[name]
		if !ok {
			status(w, 404, "NotFound")
			return
		}
		writeJSON(w, 200, o)
	case r.Method == http.MethodDelete:
		if _, ok := store[name]; !ok {
			status(w, 404, "NotFound")
			return
		}
		f.deletes = append(f.deletes, res+"/"+name+"?"+r.URL.RawQuery)
		delete(store, name)
		if res == "jobs" { // background propagation: the pods go with their job
			for pn, p := range f.objs["pods"] {
				if at(p, "metadata", "labels", "job-name") == name {
					delete(f.objs["pods"], pn)
				}
			}
		}
		writeJSON(w, 200, obj{"kind": "Status", "status": "Success"})
	default:
		status(w, 405, "MethodNotAllowed")
	}
}

// runJob is the Job controller: one pod, its state and log, and the Job's conditions.
func (f *fakeAPI) runJob(job obj) {
	name := at(job, "metadata", "name").(string)
	cmd := []string{}
	for _, c := range at(job, "spec", "template", "spec", "containers").([]any)[0].(obj)["command"].([]any) {
		cmd = append(cmd, c.(string))
	}
	oc := f.behave(job, cmd)
	labels := obj{"job-name": name}
	for k, v := range at(job, "spec", "template", "metadata", "labels").(obj) {
		labels[k] = v
	}
	cs := obj{"name": "run"}
	phase := "Succeeded"
	var conds []any
	switch {
	case oc.pull != "":
		cs["state"] = obj{"waiting": obj{"reason": oc.pull}}
		phase = "Pending"
	case oc.hang:
		cs["state"] = obj{"running": obj{}}
		phase = "Running"
	default:
		term := obj{"exitCode": oc.exit, "reason": "Completed"}
		if oc.termReason != "" {
			term["reason"] = oc.termReason
		}
		cs["state"] = obj{"terminated": term}
		switch {
		case oc.deadline:
			phase = "Failed"
			conds = []any{obj{"type": "Failed", "status": "True", "reason": "DeadlineExceeded"}}
		case oc.exit != 0:
			phase = "Failed"
			conds = []any{obj{"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded"}}
		default:
			conds = []any{obj{"type": "Complete", "status": "True"}}
		}
	}
	log := oc.stdout
	if !oc.noMarker {
		if m := regexp.MustCompile(`--IDP-STDERR-([0-9a-f]+)--`).FindStringSubmatch(strings.Join(cmd, " ")); m != nil {
			log += "\n--IDP-STDERR-" + m[1] + "--\n" + oc.stderr
		}
	}
	pn := name + "-x7k2p"
	f.objs["pods"][pn] = obj{"metadata": obj{"name": pn, "labels": labels},
		"status": obj{"phase": phase, "containerStatuses": []any{cs}}, "_log": log}
	job["status"] = obj{"conditions": conds}
}

// snapshot helpers (the tests read the store after the fact)
func (f *fakeAPI) count(res string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.objs[res])
}

func (f *fakeAPI) put(res, name string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := obj{}
	for k, v := range labels {
		l[k] = v
	}
	f.objs[res][name] = obj{"metadata": obj{"name": name, "labels": l}}
}

func (f *fakeAPI) has(res, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objs[res][name]
	return ok
}

func (f *fakeAPI) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// created returns every object of the resource ever created (deleted ones included).
func (f *fakeAPI) created(res string) []obj {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]obj(nil), f.history[res]...)
}

func (f *fakeAPI) newKube(t *testing.T, mod func(*Config)) *Kube {
	t.Helper()
	cfg := Config{APIServer: f.srv.URL, Token: f.token, Namespace: testNS, HelperImage: "alpine:test", Owner: "me",
		WorkspaceBytes: 64 << 20, Poll: 10 * time.Millisecond, Grace: 5 * time.Second}
	if mod != nil {
		mod(&cfg)
	}
	k, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var _ = fmt.Sprint

func newTLS(t *testing.T, f *fakeAPI) *httptest.Server {
	s := httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(s.Close)
	return s
}

func writeCA(t *testing.T, path string, f *fakeAPI) {
	t.Helper()
	der := f.srv.Certificate().Raw
	pem := "-----BEGIN CERTIFICATE-----\n" + wrap64(base64.StdEncoding.EncodeToString(der)) + "-----END CERTIFICATE-----\n"
	if err := os.WriteFile(path, []byte(pem), 0o600); err != nil {
		t.Fatal(err)
	}
}

func wrap64(s string) string {
	var b strings.Builder
	for len(s) > 64 {
		b.WriteString(s[:64] + "\n")
		s = s[64:]
	}
	return b.String() + s + "\n"
}
