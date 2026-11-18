"""Egress on the Kubernetes backend, on a real cluster (tests/kind/egress.sh; ADR-0001 D22): MCP calls through a
port-forward to the sandbox service as callers of several tenants, plus kubectl's view of the pods and of the proxy's log.

The origins are two Services of the test's own namespace (acme-site, globex-site) in front of one web server; the proxy
is told to accept private addresses (IDP_EGRESS_ALLOW_PRIVATE) and a test NetworkPolicy lets it reach that web server,
so that the test does not depend on the internet. Everything else is deploy/kubernetes-egress as shipped."""
import json
import os
import subprocess
import sys
import threading
import time
import urllib.request

URL = os.environ.get("SANDBOX_URL", "http://127.0.0.1:8570/mcp")   # a port-forward to svc/idp-sandbox
RUNS, SVC, TEST = "idp-sandbox-runs", "idp-sandbox", "idp-sandbox-egress-test"
META = {"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": {}}
ACME, GLOBEX = f"acme-site.{TEST}.svc.cluster.local", f"globex-site.{TEST}.svc.cluster.local"
BODY = "hello from the test origin"
failed = []


def kubectl(*args):
    return subprocess.run(["kubectl", *args], capture_output=True, text=True, timeout=60).stdout


def jsonpath(path, *args):
    out = kubectl(*args, "-o", "jsonpath=" + path).strip()
    if not out:
        sys.exit(f"kubectl {' '.join(args)}: nothing at {path}")
    return out


# addresses, not names: a run pod has no DNS
API_IP = jsonpath("{.spec.clusterIP}", "get", "svc", "kubernetes")
SANDBOX_IP = jsonpath("{.spec.clusterIP}", "-n", SVC, "get", "svc", "idp-sandbox")
PROXY_IP = jsonpath("{.spec.clusterIP}", "-n", SVC, "get", "svc", "idp-sandbox-egress")
DNS_IP = jsonpath("{.spec.clusterIP}", "-n", "kube-system", "get", "svc", "kube-dns")
WEB_IP = jsonpath("{.items[0].status.podIP}", "-n", TEST, "get", "pods", "-l", "app=web")
WEB_SVC_IP = jsonpath("{.spec.clusterIP}", "-n", TEST, "get", "svc", "acme-site")


def check(name, ok, detail=""):
    print(("PASS " if ok else "FAIL ") + name + ("" if ok else f"  {detail}"), flush=True)
    if not ok:
        failed.append(name)


def call(tool, args, tenant):
    meta = dict(META)
    meta["io.idp.telos/caller"] = {"subject": "someone-at-" + tenant, "tenant": tenant, "roles": ["operator"]}
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": tool, "arguments": args, "_meta": meta}}).encode()
    req = urllib.request.Request(URL, body, {"Content-Type": "application/json", "Accept": "application/json", "MCP-Protocol-Version": "2026-07-28",
                                              "Mcp-Method": "tools/call", "Mcp-Name": tool})
    with urllib.request.urlopen(req, timeout=180) as r:
        return json.load(r)["result"]


def structured(r):
    return r.get("structuredContent") or {}


# what the code of a run has to work with: the standard client (urllib reads HTTP_PROXY, credential included), and the
# proxy spoken to by hand
PRELUDE = '''
import base64, os, socket, urllib.error, urllib.parse, urllib.request
PROXY = urllib.parse.urlparse(os.environ.get("HTTP_PROXY", "http://0.0.0.0:1"))
def get(url):
    try:
        return urllib.request.urlopen(url, timeout=20).read().decode()
    except urllib.error.HTTPError as e:
        return "HTTP %d" % e.code
    except Exception as e:
        return "ERR %s" % type(e).__name__
def tcp(h, p):
    s = socket.socket(); s.settimeout(3)
    try:
        s.connect((h, p)); return "CONNECTED"
    except OSError:
        return "blocked"
def dns(server, name):
    """One A query over UDP, by hand: the resolver's own timeouts are longer than a run."""
    q = b"\\x12\\x34\\x01\\x00\\x00\\x01\\x00\\x00\\x00\\x00\\x00\\x00" + b"".join(bytes([len(p)]) + p.encode() for p in name.split(".")) + b"\\x00\\x00\\x01\\x00\\x01"
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3)
    try:
        s.sendto(q, (server, 53)); s.recvfrom(512); return "ANSWERED"
    except OSError:
        return "no answer"
def together(**what):
    """Each name: the result of its function, all at once (a blocked connection takes its whole timeout)."""
    import threading
    out = {}
    ts = [threading.Thread(target=lambda k=k, f=f: out.update({k: f()})) for k, f in what.items()]
    [t.start() for t in ts]; [t.join() for t in ts]
    for k in what: print(k + ":", out.get(k))
def proxy(line, password=PROXY.password, then=None):
    """One request line to the proxy with this password (None: no credential); the status line, and with `then` what
    came back through the tunnel."""
    s = socket.create_connection((PROXY.hostname, PROXY.port), timeout=5); s.settimeout(25)
    auth = "" if password is None else "Proxy-Authorization: Basic " + base64.b64encode(("run:" + password).encode()).decode() + "\\r\\n"
    s.sendall((line + " HTTP/1.1\\r\\nHost: x\\r\\n" + auth + "\\r\\n").encode())
    head = s.recv(4096).decode(errors="replace")
    status = head.split("\\r\\n")[0]
    if then and " 200 " in status:
        s.sendall(then.encode()); data = b""
        while True:
            chunk = s.recv(4096)
            if not chunk: break
            data += chunk
        status += " | " + data.decode(errors="replace").split("\\r\\n\\r\\n")[-1]
    s.close()
    return status
'''


def run(ws, tenant, code, egress=True):
    """One run, inside the service's 20 s limit."""
    args = {"workspaceId": ws, "language": "python", "code": PRELUDE + code}
    if egress:
        args["network"] = "egress"
    r = call("sandbox_run", args, tenant)
    return structured(r).get("stdout", "") + ("" if not r.get("isError") else "\nERROR " + json.dumps(r)[:400]), r


def lines(out):
    return dict(l.split(": ", 1) for l in out.splitlines() if ": " in l)


t0 = time.time()
WS = {t: structured(call("sandbox_create", {}, t)).get("workspaceId", "") for t in ("acme", "globex", "initech", "probe")}
check("workspaces", all(len(w) == 32 for w in WS.values()), WS)

# a tenant with a list of its own, through the proxy and around it
out, r = run(WS["acme"], "acme", f'''
print("own:", get("http://{ACME}/"))
print("other:", get("http://{GLOBEX}/"))
print("unlisted:", get("http://example.com/"))
print("tunnel:", proxy("CONNECT {ACME}:80", then="GET / HTTP/1.0\\r\\n\\r\\n"))
print("tunnel-other:", proxy("CONNECT {GLOBEX}:80"))
print("no-credential:", proxy("GET http://{ACME}/", password=None))
print("made-up:", proxy("GET http://{ACME}/", password="eyJ0IjoiZ2xvYmV4In0.AAAA"))
print("proxy-is:", PROXY.hostname, PROXY.port, PROXY.username)
print("sa-token:", os.path.exists("/var/run/secrets/kubernetes.io/serviceaccount/token"))
together(**{{
    "direct-origin-pod": lambda: tcp("{WEB_IP}", 8080), "direct-origin-service": lambda: tcp("{WEB_SVC_IP}", 80),
    "direct-api": lambda: tcp("{API_IP}", 443), "direct-sandbox-service": lambda: tcp("{SANDBOX_IP}", 8570),
    "direct-internet": lambda: tcp("1.1.1.1", 443), "direct-dns": lambda: tcp("{DNS_IP}", 53),
    "resolve": lambda: dns("{DNS_IP}", "{ACME}")}})
''')
got = lines(out)
check("egress-run-ran", structured(r).get("exitCode") == 0, r)
check("own-list-through-the-standard-client", got.get("own") == BODY, out)
check("another-tenants-host-refused", got.get("other") == "HTTP 403" and got.get("unlisted") == "HTTP 403", out)
check("connect-tunnel-to-own-host", " 200 " in got.get("tunnel", "") and got.get("tunnel", "").endswith(BODY), out)
check("connect-tunnel-to-another-tenants-host-refused", " 403 " in got.get("tunnel-other", ""), out)
check("no-credential-407", " 407 " in got.get("no-credential", ""), out)
check("made-up-credential-407", " 407 " in got.get("made-up", ""), out)
check("told-the-proxy-by-cluster-ip", got.get("proxy-is") == f"{PROXY_IP} 3128 run", out)
check("cannot-go-around-the-proxy", all(got.get(k) == "blocked" for k in ("direct-origin-pod", "direct-origin-service", "direct-api",
                                                                           "direct-sandbox-service", "direct-internet", "direct-dns")) and "CONNECTED" not in out, out)
check("no-dns-in-the-run", got.get("resolve") == "no answer", out)
check("no-service-account-token", got.get("sa-token") == "False", out)

# another tenant: its own list, nothing of acme's
out, _ = run(WS["globex"], "globex", f'print("own:", get("http://{GLOBEX}/"))\nprint("other:", get("http://{ACME}/"))')
got = lines(out)
check("second-tenant-own-list-only", got.get("own") == BODY and got.get("other") == "HTTP 403", out)

# a tenant the file does not name: the default list, which is empty here
out, _ = run(WS["initech"], "initech", f'print("a:", get("http://{ACME}/"))\nprint("b:", get("http://{GLOBEX}/"))')
got = lines(out)
check("unnamed-tenant-gets-the-default-list", got.get("a") == "HTTP 403" and got.get("b") == "HTTP 403", out)

# the proxy's own NetworkPolicy: this tenant's list names the API server, and the proxy (told to accept private addresses
# for this test) tries to connect; the cluster does not let it
out, _ = run(WS["probe"], "probe", f'print("api-through-proxy:", proxy("CONNECT {API_IP}:443"))')
check("proxy-cannot-reach-the-cluster-even-when-a-list-says-so", " 403 " in lines(out).get("api-through-proxy", ""), out)

# a run that did not ask: no proxy in its environment and no way to it
out, r = run(WS["acme"], "acme", f'print("proxy-env:", sorted(k for k in os.environ if "proxy" in k.lower()))\nprint("proxy:", tcp("{PROXY_IP}", 3128))', egress=False)
got = lines(out)
check("run-without-egress-cannot-reach-the-proxy", got.get("proxy-env") == "[]" and got.get("proxy") == "blocked", out)


def pods(selector):
    return json.loads(kubectl("-n", RUNS, "get", "pods", "-l", selector, "-o", "json") or '{"items":[]}')["items"]


def running(selector):
    for _ in range(60):
        items = [p for p in pods(selector) if p["status"].get("phase") == "Running" and p["status"].get("podIP")]
        if items:
            return items[0]
        time.sleep(1)
    return None


# two egress runs at once: neither takes a connection from the other
listener = {}
th = threading.Thread(target=lambda: listener.update(out=run(WS["acme"], "acme", '''
srv = socket.socket(); srv.bind(("0.0.0.0", 9000)); srv.listen(1); srv.settimeout(16)
try:
    srv.accept(); print("listener: got a connection")
except OSError:
    print("listener: nobody came")
''')[0]))
th.start()
pod = running("idp.sandbox/role=run,idp.sandbox/network=egress")
check("egress-run-pod-carries-the-label", pod is not None, pods("idp.sandbox/role=run"))
if pod:
    time.sleep(2)   # the listener is up
    out, _ = run(WS["globex"], "globex", f'print("peer:", tcp("{pod["status"]["podIP"]}", 9000))')
    # "blocked" only counts while the other run was still listening
    check("egress-runs-cannot-reach-each-other", lines(out).get("peer") == "blocked" and th.is_alive(), (out, th.is_alive()))
th.join()
check("listener-saw-nobody", "listener: nobody came" in listener.get("out", ""), listener)

# and a run that did not ask is a pod without the label
th = threading.Thread(target=lambda: run(WS["initech"], "initech", "import time; time.sleep(12)", egress=False))
th.start()
pod = running("idp.sandbox/role=run,!idp.sandbox/network")
check("plain-run-pod-has-no-network-label", pod is not None and not pods("idp.sandbox/network=egress"), pods("idp.sandbox/role=run"))
th.join()

# the proxy's log: who reached what, by tenant, and never a credential
log = [json.loads(l) for l in kubectl("-n", SVC, "logs", "deploy/idp-sandbox-egress", "--tail=-1").splitlines() if l.startswith("{")]
allows = [(l.get("tenant"), l.get("host")) for l in log if l.get("decision") == "allow"]
denies = [(l.get("tenant"), l.get("host"), l.get("why")) for l in log if l.get("decision") == "deny"]
check("log-allows-by-tenant", ("acme", ACME) in allows and ("globex", GLOBEX) in allows and not [a for a in allows if a[0] not in ("acme", "globex")], allows)
check("log-denies-by-tenant", ("acme", GLOBEX, "not on the allow-list") in denies and ("globex", ACME, "not on the allow-list") in denies
      and ("initech", ACME, "not on the allow-list") in denies and ("probe", API_IP, "no acceptable address") in denies, denies)
check("log-has-workspace-hashes-and-no-credential", all(len(l.get("workspace", "")) == 8 for l in log if l.get("decision") == "allow")
      and "Proxy-Authorization" not in json.dumps(log) and "eyJ" not in json.dumps(log), log[:3])
start = next((l for l in log if "listening" in l.get("msg", "")), {})
check("proxy-requires-the-credential", start.get("credentialRequired") is True and start.get("tenantsWithTheirOwnList") == ["acme", "globex", "probe"], start)

# the service checked its own egress runs before serving
svc_log = kubectl("-n", SVC, "logs", "deploy/idp-sandbox", "--tail=-1")
check("startup-probe-of-egress-runs", "run pods with network=egress reach the egress proxy and not the API server" in svc_log
      and "run pods are cut off from the network" in svc_log, svc_log[-600:])

for t, w in WS.items():
    call("sandbox_delete", {"workspaceId": w}, t)
for _ in range(30):
    left = [x for x in kubectl("-n", RUNS, "get", "jobs,pods,configmaps,pvc", "-o", "name").split() if x != "configmap/kube-root-ca.crt"]
    if not left:
        break
    time.sleep(1)
check("nothing-left-behind", left == [], left)

print(f"{'FAILED: ' + ', '.join(failed) if failed else 'ALL PASS'}  ({time.time() - t0:.0f}s)")
sys.exit(1 if failed else 0)
