"""The Kubernetes backend on a real cluster (tests/kind/smoke.sh): MCP calls through a port-forward to the sandbox
service, with the caller the gateway would forward, plus kubectl checks of what the cluster actually did."""
import json
import os
import subprocess
import sys
import threading
import time
import urllib.request

URL = "http://127.0.0.1:8570/mcp"
RUNS = "idp-sandbox-runs"
META = {"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": {}}
failed = []


def check(name, ok, detail=""):
    print(("PASS " if ok else "FAIL ") + name + ("" if ok else f"  {detail}"), flush=True)
    if not ok:
        failed.append(name)


def call(tool, args, subject="alice"):
    meta = dict(META)
    if subject:
        meta["io.idp.telos/caller"] = {"subject": subject, "tenant": "acme", "roles": ["operator"]}
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": tool, "arguments": args, "_meta": meta}}).encode()
    req = urllib.request.Request(URL, body, {"Content-Type": "application/json", "Accept": "application/json", "MCP-Protocol-Version": "2026-07-28",
                                              "Mcp-Method": "tools/call", "Mcp-Name": tool})
    with urllib.request.urlopen(req, timeout=180) as r:
        return json.load(r)["result"]


def structured(r):
    return r.get("structuredContent") or {}


def text(r):
    return r["content"][0]["text"]


def kubectl(*args):
    return subprocess.run(["kubectl", *args], capture_output=True, text=True, timeout=60).stdout


def run(ws, code, **kw):
    return call("sandbox_run", {"workspaceId": ws, "language": "python", "code": code, **kw})


t0 = time.time()
ws = structured(call("sandbox_create", {}))["workspaceId"]
check("create", len(ws) == 32)
check("write", call("sandbox_write_file", {"workspaceId": ws, "path": "data/in.csv", "content": "a,b\n1,2\n3,4\n"}).get("isError") is not True)
r = run(ws, "import csv\nrows=list(csv.DictReader(open('data/in.csv')))\ns=sum(int(x['a'])+int(x['b']) for x in rows)\nopen('out.txt','w').write(str(s))\nprint('sum', s)\nimport sys; print('to stderr', file=sys.stderr)")
check("run-stdout-and-exit", structured(r).get("exitCode") == 0 and "sum 10" in structured(r).get("stdout", ""), r)
check("stderr-is-separate", "to stderr" in structured(r).get("stderr", "") and "to stderr" not in structured(r).get("stdout", ""), r)
check("read-back", text(call("sandbox_read_file", {"workspaceId": ws, "path": "out.txt"})) == "10")
files = structured(call("sandbox_list_files", {"workspaceId": ws})).get("files", [])
check("list", sorted(f["path"] for f in files) == ["data/in.csv", "out.txt"], files)
check("nonzero-exit", structured(run(ws, "import sys; sys.exit(3)")).get("exitCode") == 3)

# what the code can and cannot do
r = run(ws, """
import os, socket
def tcp(h, p):
    s = socket.socket(); s.settimeout(3)
    try:
        s.connect((h, p)); return 'CONNECTED'
    except OSError:
        return 'blocked'
print('uid', os.getuid())
print('caps', [l for l in open('/proc/self/status') if l.startswith('CapEff')][0].strip())
print('noprivs', [l for l in open('/proc/self/status') if l.startswith('NoNewPrivs')][0].strip())
try:
    open('/etc/pwned', 'w'); print('root writable')
except OSError:
    print('root read-only')
print('internet', tcp('1.1.1.1', 443))
print('api', tcp('%s', 443))
print('sandbox-service', tcp('%s', 8570))
print('sa token', os.path.exists('/var/run/secrets/kubernetes.io/serviceaccount/token'))
""" % (os.environ["API_IP"], os.environ["SANDBOX_IP"]))
out = structured(r).get("stdout", "")
check("unprivileged", "uid 65534" in out and "CapEff:\t0000000000000000" in out and "NoNewPrivs:\t1" in out and "root read-only" in out, out)
check("no-network", "internet blocked" in out and "api blocked" in out and "sandbox-service blocked" in out and "CONNECTED" not in out, out)
check("no-service-account-token", "sa token False" in out, out)

# limits
r = run(ws, "x=[]\nwhile True: x.append(bytearray(32<<20))")
check("memory-bomb-killed", structured(r).get("outOfMemory") is True or structured(r).get("exitCode") in (137, -9), r)
r = run(ws, "while True: pass", timeoutSeconds=6)
check("loop-times-out", structured(r).get("timedOut") is True, r)
r = run(ws, "import sys\nwhile True: sys.stdout.write('A'*65536)", timeoutSeconds=6)
check("output-capped", len(structured(r).get("stdout", "")) <= 64 * 1024 + 16, len(structured(r).get("stdout", "")))
r = run(ws, "import errno\ntry:\n    with open('/tmp/fill','wb') as f:\n        while True: f.write(b'x'*(1<<20)); f.flush()\nexcept OSError as e:\n    print('tmp full', errno.errorcode.get(e.errno))")
check("tmp-quota", "tmp full ENOSPC" in structured(r).get("stdout", ""), r)

# the cluster's view of a running pod
th = threading.Thread(target=lambda: run(ws, "import time; time.sleep(25)"))
th.start()
pod = None
for _ in range(60):
    items = json.loads(kubectl("-n", RUNS, "get", "pods", "-l", "idp.sandbox/role=run", "-o", "json") or '{"items":[]}')["items"]
    if items and items[0]["status"].get("phase") == "Running":
        pod = items[0]
        break
    time.sleep(1)
check("run-pod-seen", pod is not None)
if pod:
    sp, c = pod["spec"], pod["spec"]["containers"][0]
    sc, psc = c["securityContext"], sp.get("securityContext", {})
    check("pod-hardening", sc.get("allowPrivilegeEscalation") is False and sc.get("readOnlyRootFilesystem") is True and sc["capabilities"]["drop"] == ["ALL"]
          and psc.get("runAsNonRoot") is True and psc.get("runAsUser") == 65534 and psc.get("seccompProfile", {}).get("type") == "RuntimeDefault"
          and sp.get("automountServiceAccountToken") is False and not sp.get("hostNetwork") and not sp.get("hostPID") and not sp.get("hostIPC"), json.dumps(sp)[:600])
    check("pod-limits", "memory" in c["resources"]["limits"] and "cpu" in c["resources"]["limits"], c["resources"])
th.join()

# isolation between callers and clean-up
r = call("sandbox_read_file", {"workspaceId": ws, "path": "out.txt"}, subject="bob")
check("other-caller-gets-not-found", r.get("isError") is True and "not found" in text(r), r)
check("anonymous-refused", call("sandbox_create", {}, subject="").get("isError") is True)
check("delete", call("sandbox_delete", {"workspaceId": ws}).get("isError") is not True)
time.sleep(3)
left = [x for x in kubectl("-n", RUNS, "get", "jobs,pods,configmaps,pvc", "-o", "name").split() if x != "configmap/kube-root-ca.crt"]
check("nothing-left-behind", left == [], left)
print(f"kubernetes backend: {len(failed)} failed in {time.time() - t0:.0f}s {failed}")
sys.exit(1 if failed else 0)
