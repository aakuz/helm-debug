# Helm Resources Tools

A collection of CLI utilities for working with Helm releases directly via Kubernetes Secrets, without requiring the Helm CLI.

## Tools

### 1. **helm-resources-checker.py** – Release Resource Checker
Inspects a specific Helm release and lists all resources rendered by that release revision, highlighting whether each resource currently exists in the cluster.

```
✓ exists  – green
✗ absent  – red
```

### 2. **helm-resource-finder.py** – Resource Finder (NEW)
Searches for a specific Kubernetes resource across **all** Helm releases in the cluster to determine which release(s) manage it.

---

## Features

### helm-resources-checker.py
* **Revision aware** – query the latest revision or any specific `--revision` you need.
* **Cluster‑scoped & namespaced** resources are both detected.
* **Robust decoding** – handles the single‑ or double‑base64 + gzip formats used by different Helm versions/plugins.
* **No cluster‑wide list calls** – works only with the release Secret and targeted GETs, so it's friendly to restricted RBAC environments.
* **Colour output** – clearly see which objects are missing.

### helm-resource-finder.py
* **Cross-release search** – scans all Helm releases across all namespaces to find where a resource is defined.
* **Flexible filtering** – search by kind, name, and optionally namespace.
* **Revision history** – shows all revisions where the resource appears.
* **Detailed output** – displays release status, timestamps, labels, and annotations.
* **Robust decoding** – handles multiple Helm encoding formats including double base64, gzip, zlib, and bz2.
* **Statistics** – reports on successful/failed decodes to help diagnose issues.

---

## Installation

```bash
python -m venv venv && source venv/bin/activate
pip install -r requirements.txt
```

Both scripts rely on your default kube‑config _or_ in‑cluster service account credentials, exactly like `kubectl`.

---

## Usage

### helm-resources-checker.py

Check which resources from a Helm release currently exist in the cluster:

```bash
python helm-resources-checker.py \
  --name       <release>     \
  --namespace  <namespace>   \
  [--revision <N>]
```

Examples:

* Inspect **latest** revision of release `backend` in namespace `prod`:

  ```bash
  python helm-resources-checker.py --name backend --namespace prod
  ```

* Inspect **revision 12** of release `flux-ui` in namespace `flux-system`:

  ```bash
  python helm-resources-checker.py --name flux-ui --namespace flux-system --revision 12
  ```

_Output sample:_

```
Resources in Helm release 'flux-ui' revision 12 (namespace flux-system):
-----------------------------------------------------------------------
Deployment                flux-system/flux-ui            -> ✓ exists
ClusterRole               flux-ui                        -> ✓ exists
ServiceAccount            flux-system/flux-ui            -> ✗ absent
...
```

---

### helm-resource-finder.py

Find which Helm release(s) manage a specific Kubernetes resource:

```bash
python helm-resource-finder.py \
  --kind      <kind>      \
  --name      <name>      \
  [--namespace <ns>]
```

**Command-line options:**
* `-k, --kind` – Kubernetes resource kind (e.g., Deployment, Service, ConfigMap) [required]
* `-n, --name` – Name of the resource to search for [required]
* `-ns, --namespace` – Namespace to filter the search (optional, searches all if not specified)
* `-v, --verbose` – Enable verbose output

Examples:

* Find which release manages a Deployment named `nginx-deployment`:

  ```bash
  python helm-resource-finder.py --kind Deployment --name nginx-deployment
  ```

* Find a Service named `api-service` in the `production` namespace:

  ```bash
  python helm-resource-finder.py --kind Service --name api-service --namespace production
  ```

* Short form options:

  ```bash
  python helm-resource-finder.py -k ConfigMap -n redis-config -ns storage
  ```

_Output sample:_

```
🔍 Searching for deployment/nginx-deployment across all namespaces
------------------------------------------------------------
Fetching Helm release secrets from cluster...
Found 45 Helm release secret(s)

Processing 15 unique Helm release(s)...
Searching in release 'backend'...
  ✓ Found in backend revision 8 (status: deployed)

============================================================
SEARCH RESULTS
============================================================

✅ Resource deployment/nginx-deployment FOUND in 1 location(s):

📦 Release: backend (namespace: production)
   └─ 🟢 Revision 8 (status: deployed)
      • Updated: 2025-11-10 14:32:15
      • Resource namespace: production
      • Labels: app=nginx, env=prod

============================================================
STATISTICS
------------------------------------------------------------
Total Helm secrets found: 45
Successfully decoded: 45
============================================================
```

---

## Exit codes

| Code | Meaning                                  |
|------|-------------------------------------------|
| 0    | Success                                   |
| 1    | Secret or revision not found / other error|

---

## Notes

* Both scripts use **ANSI escape codes** and emojis for visual output; if you pipe the output, colours might disappear (depending on your terminal). For CI environments without TTY support you can temporarily remove the colour codes.
* **RBAC requirements:**
  * **helm-resources-checker.py**: needs permission to **get** `secrets` in the release namespace _and_ **get** each listed resource to check its existence.
  * **helm-resource-finder.py**: requires permission to **list** `secrets` across all namespaces with label `owner=helm`.

---

## Use Cases

### helm-resources-checker.py
Use this when you:
- Want to verify which resources from a specific Helm release are still present in the cluster
- Need to troubleshoot a release that may have partially failed or been partially deleted
- Want to audit the current state vs. declared state of a specific release

### helm-resource-finder.py
Use this when you:
- Found a resource in the cluster and want to know which Helm release manages it
- Need to track down the origin of a specific Deployment, Service, ConfigMap, etc.
- Want to see all revisions of a release that included a particular resource
- Are investigating resource ownership and management

---

## License

MIT © 2025 Anton Kuznetsov
