# Helm troubleshooting with helm-debug

This page answers a bank of about 620 Helm debugging questions, collected from the Helm docs, Helm and
Flux helm-controller GitHub issues and Stack Overflow, and shows for each one which `helm-debug`
command answers it. When `helm-debug` cannot answer a question yet, the row links the issue where
the answer is planned.

`helm-debug` is read-only: it reads release records (`sh.helm.release.v1.*` Secrets or ConfigMaps)
and live objects, and prints fixes instead of running them. It does not render charts, talk to chart
repositories or run Helm operations, so questions about those are answered with the Helm CLI.

| Mark | Meaning |
|---|---|
| ✅ | Answered by `helm-debug` |
| 🟡 | Partly answered; the rest is noted, or planned in the linked issue |
| 🔧 #N | Not answered yet; planned in issue #N |
| ➖ | Out of scope: a Helm/Kubernetes concept or a client-side operation (rendering, repositories, running Helm). The pointer says where to look. |

Commands below omit `-n NAMESPACE` for brevity. `REL` is the release name, `N` a revision.

## Triage path

A good investigation follows the state, not a guess: release state → history → stored manifest →
desired vs live → API errors and events → hooks → workload readiness → ownership, CRDs, admission →
recovery. With `helm-debug`:

| Step | Command |
|---|---|
| Which releases are in trouble | `helm-debug stuck -A`, `helm-debug list -A --all` |
| What happened to the release | `helm-debug history REL` (status and stored failure reason per revision) |
| What changed in the failed revision | `helm-debug diff REL N-1 N` |
| What Helm thinks is installed | `helm-debug manifests REL [--hooks]`, `helm-debug values REL [--all]` |
| Do the declared objects exist and does Helm own them | `helm-debug resources REL` |
| Did someone change them | `helm-debug drift REL` |
| Are the workloads healthy, and why not | `helm-debug health REL --logs 50` |
| Did a hook block the operation | `helm-debug hooks REL` |
| Will the next Kubernetes version break it | `helm-debug deprecated REL --target-version 1.32` |
| Who owns this object | `helm-debug find KIND/NAME -A` |
| What is left of deleted releases | `helm-debug lost-found -A`, `helm-debug audit -A` |
| GitOps layer | `helm-debug flux status HR`, `helm-debug argo resources APP -n argocd` |

Deleting the latest `sh.helm.release.v1.*` Secret is an emergency measure, not a first step:
check `stuck` (it suggests a rollback to the last good revision) and back up the records first.

## 1. First look at a release

| # | Question | Answer with helm-debug |
|---|---|---|
| 1 | Does the release exist at all? | ✅ `list -A --all` or `history REL`; exit code 1 means no records |
| 2 | How to find a release when the namespace is unknown? | ✅ `list -A --all`; `find KIND/NAME -A` from one of its objects |
| 3 | Why can `helm list` miss a release whose Secret exists? | 🟡 `list --all` shows pending/uninstalled ones; records in another namespace or driver: #24, #45 |
| 4 | `helm list` vs `helm status` vs `helm history`? | ✅ `list` = latest revision per release, `history` = every revision with failure reason, `health` = live state |
| 5 | Which release states exist? | ➖ deployed, failed, pending-install/upgrade/rollback, superseded, uninstalling, uninstalled, unknown; all shown by `history` |
| 6–14 | What do `deployed`, `failed`, `pending-*`, `uninstalling`, `superseded`, `uninstalled`, `unknown` mean? | ➖ Helm concept; `history` shows them, `stuck` flags pending ones, `health` exits 2 on problem states |
| 15 | Where does Helm store release state? | ✅ Secrets (or ConfigMaps, `--storage`) in the release namespace; `audit` lists them with sizes |
| 16 | How to find the Secrets of one release? | ✅ `history REL`, `audit`; raw: `kubectl get secret -l owner=helm,name=REL` |
| 17 | What is `sh.helm.release.v1.foo.v17`? | ➖ the record of revision 17 of release `foo`; decoded by `history`, `manifests`, `values` |
| 18 | How to find the current revision? | ✅ `list`, `history` (last row) |
| 19 | Which revision was the last successful one? | ✅ `history` (latest `deployed`/`superseded`); `stuck` names it in its rollback fix |
| 20 | How to get the description of the last error? | ✅ `history REL` (DESCRIPTION column) |
| 21 | How to get the installed manifest? | ✅ `manifests REL [--revision N]` |
| 22 | How to get the installed values? | ✅ `values REL` |
| 23 | `helm get values` vs the original values.yaml? | ✅ `values` = user-supplied values only; `values --all` = merged with chart defaults |
| 24 | Values together with chart defaults? | ✅ `values REL --all` |
| 25 | Hooks of a release? | ✅ `hooks REL`, `manifests REL --hooks` |
| 26 | Release notes? | 🔧 #59 (`helm get notes` meanwhile) |
| 27 | Installed chart version? | ✅ `list`, `history` (CHART column) |
| 28 | appVersion? | ✅ `list`, `history` (APP VERSION column) |
| 29 | Which Helm client version created the release? | ➖ Helm does not store it in the record |
| 30 | Which Kubernetes object blocks a Helm operation? | 🟡 `resources` (absent, unknown kind, ownership), `health`, `hooks`; what `--wait` waits for: #46; finalizers: #49 |

## 2. Debugging approach

| # | Question | Answer with helm-debug |
|---|---|---|
| 31 | Where to start with `UPGRADE FAILED`? | ✅ `history REL` (reason), then `diff REL N-1 N`, `resources`, `health` |
| 32 | Which commands first? | ✅ see [Triage path](#triage-path) |
| 33–34 | When is `helm lint` useful, what does it check? | ➖ chart structure and rendering, not the cluster; template error context: #43 |
| 35 | When to use `helm template --debug`? | ➖ rendering; rendered YAML around an error: #43 |
| 36–37 | `helm template` vs `--dry-run`, client vs server dry-run? | ➖ Helm CLI; helm-debug does not render charts |
| 38–39 | Why does a chart pass `template`/YAML checks but fail on install? | 🟡 API validation, CRDs, admission, ownership; `resources` shows unknown kinds and ownership, `deprecated` removed APIs, webhooks: #57 |
| 40 | Check a chart against the real API server without installing? | ➖ `helm install --dry-run=server`; preview against the release: #17 |
| 41–45 | Helm vs Kubernetes, rendering vs API, Helm vs application, chart vs values, Helm vs webhook? | 🟡 `history` holds the API error text; `health` shows app failures; webhook detail: #57; template errors: #43 |
| 46 | First Kubernetes object that really broke? | 🟡 `resources`, `health`; what `--wait` was waiting for: #46 |
| 47 | Minimal reproduction of a broken chart? | 🟡 `values REL --output-file v.yaml` + `manifests REL --output-dir dir` give the exact inputs and output |
| 48 | Expected vs installed manifest? | ✅ `drift REL` (stored vs live), `diff REL A B` (between revisions) |
| 49 | Did someone change objects after the deployment? | ✅ `drift REL` |
| 50 | Order: release, events, controller logs, pod logs? | ✅ `history` → `resources`/`drift` → `health --logs` (events and logs together) |

## 3. helm install

| # | Question | Answer with helm-debug |
|---|---|---|
| 51 | Why can install fail before any object is created? | 🟡 rendering/validation errors (#43), RBAC (#58), webhooks (#57); `history` keeps the reason if a record was written |
| 52–53 | Why is a release stuck in `pending-install`, what to do? | ✅ `stuck -A` (with age and fix: `helm uninstall`) |
| 54 | `cannot re-use a name that is still in use`? | ✅ `list --all` / `history` show the existing record; `stuck` if it is pending |
| 55–57 | Were objects created before the failure? Partial installs? | ✅ `resources REL` (present/absent per object) |
| 58–61 | What does `--atomic` do, how does it relate to `--wait`, when does it hide the cause? | ➖ Helm behavior; keep evidence with `history` (the failed revision stays) and #55 (snapshot) |
| 62–63 | Debugging an install timeout; what is Helm waiting for? | 🟡 `health REL --logs 50` for Deployments/StatefulSets; all `--wait` kinds and a "waiting for" line: #46 |
| 64 | Can a Job block install? | 🟡 hook Jobs: `hooks`; logs and running state: #47; plain Jobs with `--wait-for-jobs`: #46 |
| 65–66 | Can a PVC or LoadBalancer Service block `--wait`? | 🔧 #46 |
| 67–69 | 0/1 Ready Deployment, no CPU/memory, ImagePullBackOff with `--wait`? | ✅ `health REL` (readiness, problem pods, `FailedScheduling` events) |
| 70–71 | Without `--wait`, a pod does not start; can Helm report success while the app is down? | ✅ yes; `health REL` shows it, exit code 2 |
| 72–73 | `--wait-for-jobs`? | ➖ Helm flag; Job readiness check: #46 |
| 74–75 | How to change the timeout; why is a longer timeout often wrong? | ➖ `--timeout`; find the real blocker with `health` / #46 first |

## 4. helm upgrade

| # | Question | Answer with helm-debug |
|---|---|---|
| 76–77 | What will `helm upgrade` change; diff before upgrade? | 🔧 #17 (preview against a new chart or values); after the fact: `diff REL N-1 N` |
| 78–81 | Why does an upgrade change nothing, or run without a chart version change? | ✅ `diff REL N-1 N` shows whether manifest or values changed |
| 82–83 | How does Helm merge manifests; what is the three-way merge? | ➖ Helm concept (old manifest, new manifest, live object) |
| 84–86 | Why do `kubectl edit` changes disappear or survive; drift before upgrade? | ✅ `drift REL` shows the edits that the next upgrade may revert |
| 87–91 | `--reuse-values`, `--reset-values`, stale values from an old chart? | ✅ `values REL --revision N` and `diff REL A B` (Values section) show what was carried over |
| 92 | Debugging `UPGRADE FAILED`? | ✅ `history`, `diff`, `resources`, `health` |
| 93 | Last successful upgrade? | ✅ `history` |
| 94–96 | Compare two revisions, manifest of the previous one, values N vs N-1? | ✅ `diff REL A B`, `manifests REL --revision N`, `values REL --revision N` |
| 97 | Rollback or fix forward? | 🟡 `diff REL CURRENT TARGET` shows what a rollback would revert; immutable fields: #50 |
| 98–99 | What happens to a failed revision; why a failed one between good ones? | ✅ `history` |
| 100–101 | Can a failed revision change part of the objects; which ones? | 🟡 `drift REL` compares the latest (failed) manifest with live objects; per-object apply state is not recorded by Helm |
| 102–104 | StatefulSet risk, immutable-field errors, changes that need a recreate? | 🔧 #50 |
| 105–110 | `--force`/replacement, Service ClusterIP, PVC, PDB on recreate? | ➖ Kubernetes behavior; #50 prints the recreate step for immutable fields |
| 111–114 | Upgrade broken after a Kubernetes upgrade; removed APIs in stored manifests? | ✅ `deprecated REL --target-version X.Y`; fix: `helm-mapkubeapis` |

## 5. pending-upgrade and "another operation is in progress"

| # | Question | Answer with helm-debug |
|---|---|---|
| 115–116 | Why stuck in `pending-upgrade`; what does "another operation is in progress" mean? | ✅ `stuck -A` |
| 117 | Is another Helm process really still running? | 🟡 Helm records no lock owner; `stuck --older-than` skips recent ones as possibly running |
| 118–122 | Ctrl+C, dead CI runner, API timeout, TLS timeout, API server restart? | ➖ all can leave a pending record; `stuck` finds it either way |
| 123 | Which revision holds the release in pending? | ✅ `stuck` (REVISION), `history` |
| 124 | Why does retrying `helm upgrade` not help? | ➖ the pending record blocks it until rolled back or removed |
| 125–126 | When is a rollback safe; which revision? | ✅ `stuck` suggests the last deployed/superseded revision; `diff` shows what it reverts |
| 127 | Rollback also ends in `pending-rollback`? | ✅ `stuck` reports `pending-rollback` too |
| 128–133 | When may the latest release Secret be deleted; consequences; restoring history? | 🟡 `history` shows what remains; back up the records first: #55; records Helm does not see: #45 |
| 134–135 | What is inside a release Secret; can it be decoded? | ✅ decoded by `history`, `values --all`, `manifests --hooks`; raw backup: #55 |
| 136 | Current revision corrupted, previous one fine? | 🟡 `history` reports undecodable records as errors; storage checks: #45 |

## 6. Rollback

| # | Question | Answer with helm-debug |
|---|---|---|
| 137–139 | How rollback works, what it restores, whether it creates a revision? | ➖ it re-applies the stored manifest and values of revision N as a new revision; visible in `history` |
| 140 | Why can a rollback fail? | 🟡 `history` (reason); immutable fields: #50; removed APIs: `deprecated --revision N` |
| 141 | Blocked by an immutable field? | 🔧 #50 |
| 142–143 | Rollback after a Kubernetes upgrade; old manifest with a removed API? | ✅ `deprecated REL --revision N` |
| 144 | Rollback brings back an old CRD? | 🔧 #48 (CRDs in `crds/` are not touched by rollback) |
| 145 | Debugging `pending-rollback`? | ✅ `stuck`, `history` |
| 146 | Rollback created part of the objects and failed? | ✅ `resources REL`, `drift REL` |
| 147 | `--cleanup-on-fail`? | ➖ Helm flag; `lost-found` finds objects left behind otherwise |
| 148–150 | StatefulSet rollback risk, app vs database, Helm vs application rollback? | ➖ application concern; `diff` shows what the rollback changes |

## 7. Values

| # | Question | Answer with helm-debug |
|---|---|---|
| 151 | Final value of one parameter? | ✅ `values REL --all` (computed values as Helm stored them) |
| 152–157 | Precedence of values.yaml, `-f`, `--set`; map and list merging? | ➖ Helm concept; the stored result is in `values --all` |
| 158–163 | `--set` vs `--set-string`, `0123`, `"false"` vs `false`, commas, dots, arrays? | ➖ Helm CLI; check the stored type with `values -o json` |
| 164–166 | Wrong YAML type, `cannot overwrite table with non table`, `<nil>`? | ➖ rendering errors; context around the error: #43 |
| 167–172 | `default`, nil pointers, safe nested access, `required`? | ➖ template authoring |
| 173–176 | `values.schema.json`, `--skip-schema-validation`? | ➖ Helm CLI |
| 177–179 | Typos in values keys, unknown keys, unused values? | 🔧 #51 |
| 180 | A value works with `helm template` but not through Flux/Argo? | 🟡 `values REL` shows what Flux actually passed; Flux `valuesFrom` checks: #56 |

## 8. Go templates

| # | Question | Answer with helm-debug |
|---|---|---|
| 181–184 | `executing ... at ...`, `can't evaluate field`, `wrong type`, `nil pointer`? | ➖ rendering errors; #43 shows the rendered context |
| 185–188, 192–204 | Pipelines, `include` vs `template`, `indent`/`nindent`, `toYaml`, `quote`, `range`, `$`, `with`, helpers, `tpl`? | ➖ template authoring; Helm docs "Chart Template Guide" |
| 189–191 | Bad indentation, `did not find expected key`, rendered output of a broken section? | 🔧 #43 |
| 205–209 | `.Capabilities`, other Kubernetes versions, local vs in-cluster rendering? | 🟡 `deprecated --target-version` checks the stored result; rendering itself is ➖ |

## 9. lookup

| # | Question | Answer with helm-debug |
|---|---|---|
| 210–213 | What `lookup` does, why it is empty, why `helm template` is not enough, `--dry-run=server`? | ➖ rendering; `manifests REL` shows what `lookup` produced at install time |
| 214 | RBAC and `lookup`? | 🟡 #58 checks the permissions of the release objects (not of `lookup` calls) |
| 215–220 | Namespace, wrong apiVersion, missing API, non-determinism, CI testing? | ➖ chart authoring; `diff REL A B` shows non-deterministic output between revisions |

## 10. Kubernetes API validation

| # | Question | Answer with helm-debug |
|---|---|---|
| 221 | Why does Kubernetes reject a manifest Helm rendered? | 🟡 `history` holds the API error; `resources` shows unknown kinds |
| 222–224 | `no matches for kind`, `resource mapping not found`, `could not find the requested resource`? | ✅ `resources REL` (`unknown-kind` with the error), `deprecated REL` |
| 225–226 | Is an API resource / version available? | ✅ `deprecated REL` (`not-served` against the connected cluster); `kubectl api-resources` |
| 227 | Custom resource before its CRD exists? | ✅ `resources` reports `unknown-kind`; CRD checks: #48 |
| 228–231 | Admission rejection, which webhook, mutation? | 🔧 #57 |
| 232–233 | Why does the installed manifest differ from the live object; SSA/managedFields? | ✅ `drift` compares declared fields only and ignores managedFields and defaults |
| 234–237 | Gatekeeper/Kyverno, Pod Security Admission? | 🟡 `health` shows `FailedCreate` events from PSA; webhook list: #57 |
| 238 | Works in one cluster, fails in another? | 🔧 #26 (compare releases across contexts); `deprecated` per cluster |

## 11. Ownership

| # | Question | Answer with helm-debug |
|---|---|---|
| 239–240 | `resource already exists`; why Helm will not adopt an object? | ✅ `resources REL` (OWNERSHIP `missing`/`mismatch`) with the annotate/label fix |
| 241–245 | `meta.helm.sh/release-name`, `release-namespace`, `managed-by`, `invalid ownership metadata`? | ✅ `resources` checks all three; `find KIND/NAME` shows the claimed release |
| 246–248 | Importing an existing object, `--take-ownership` and its risks? | ✅ `resources` prints the adoption commands; `find` shows a current owner first |
| 249–250 | Object owned by another release; move between releases? | ✅ `find KIND/NAME -A` (live annotation and declaring releases), `resources` (`mismatch`) |
| 251–253 | Two releases managing one ClusterRole or CRD; why cluster-scoped objects conflict? | ✅ `find clusterrole/NAME --all-revisions` lists every declaring release |
| 254–255 | Existing Namespace; should Helm own it? | 🟡 `resources` reports its ownership if the chart declares it; ownership policy is ➖ |
| 256 | Object created by Terraform, now in a chart? | ✅ `resources` (adoption commands), `drift` (differences) |

## 12. Immutable fields

| # | Question | Answer with helm-debug |
|---|---|---|
| 257–263 | `field is immutable`; which fields; selector, serviceName, Service, PVC; finding it in a big upgrade? | 🔧 #50 |
| 264–268 | When to recreate, when it is unsafe, automation, `--force`, replace vs patch? | ➖ Kubernetes/Helm behavior; #50 prints the recreate step |

## 13. Hooks

| # | Question | Answer with helm-debug |
|---|---|---|
| 269–277 | What hooks are, when each event runs, test hooks? | ➖ Helm concept; `hooks REL` shows the event of each hook |
| 278 | Hooks of an installed release? | ✅ `hooks REL`, `manifests REL --hooks` |
| 279–280 | Which hook hangs; why can a hook Job block Helm? | 🟡 `hooks` (phase, last run); running state and events: #47 |
| 281 | How does the timeout apply to hooks? | ➖ each hook is waited for up to `--timeout` |
| 282–284 | pre-/post-install hook failed; release state after a failed hook? | ✅ `history` (failed, reason), `hooks` (phase) |
| 285 | Logs of a hook Job? | 🔧 #47 |
| 286 | Why is the hook Job already gone? | 🟡 `hooks` shows the delete policy; explanation per policy: #47 |
| 287–288 | `hook-delete-policy`, `before-hook-creation` vs `hook-succeeded` vs `hook-failed`? | ✅ `hooks` (DELETE-POLICY) |
| 289 | Old hook object blocks the next upgrade? | ✅ `hooks` (`orphan` status for leftovers) |
| 290–292 | Hook weight, order, several hooks of one type? | ✅ `hooks` (WEIGHT, EVENT) |
| 293–295 | pre-delete hook blocks uninstall; `--no-hooks` and its consequences? | 🟡 `hooks` shows pre-delete hooks; logs: #47; uninstalling releases: #54 |
| 296 | Why are hooks not managed like other objects? | ➖ Helm does not track hooks as release objects; `resources` lists them separately |

## 14. CRDs

| # | Question | Answer with helm-debug |
|---|---|---|
| 297–301 | How Helm installs CRDs, `crds/` vs templates, order, why upgrades do not update them? | ➖ Helm concept; see #48 |
| 302–304 | Is the cluster CRD outdated; controller expects missing fields; compare chart vs cluster CRD? | 🔧 #48 |
| 305–306 | Who manages the CRD lifecycle; separate chart? | ➖ design decision |
| 307–311 | `--skip-crds`, uninstall and CRDs, what happens to custom resources? | ➖ Helm never deletes CRDs from `crds/`; `lost-found` finds leftovers |
| 312–314 | CRD migration, conversion webhook, storage version migration? | 🔧 #48 (stored versions, conversion webhook) |
| 315 | `no matches for kind` although the CRD is in the chart? | ✅ `resources` (`unknown-kind`); version mismatch: #48 |
| 316–317 | Can a hook fix CRD ordering; `crd-install` hook? | ➖ Helm 2 history |

## 15. Dependencies and subcharts

| # | Question | Answer with helm-debug |
|---|---|---|
| 318–326 | `dependency build/update`, `Chart.lock`, missing subchart, `--dependency-update`? | ➖ client-side chart tooling |
| 327–333 | Values for subcharts, `global`, condition/tags, alias? | 🟡 `values REL --all` shows the computed values per subchart |
| 334–335 | Two instances of a subchart; same object names from different subcharts? | ✅ `manifests REL --output-dir` (one file per object), `resources` |
| 336 | Dependency conflict visible only on install? | ✅ `resources` / `find` show which release owns the conflicting object |

## 16. Chart repositories

| # | Question | Answer with helm-debug |
|---|---|---|
| 337–350 | `chart not found`, repos, `repo update`, `index.yaml`, semver, prereleases, 401/403, TLS, proxies, CA, CI config? | ➖ client-side Helm; helm-debug does not talk to repositories (planned only for #17) |

## 17. OCI charts

| # | Question | Answer with helm-debug |
|---|---|---|
| 351–364 | OCI vs classic repos, login, `manifest unknown`, tags vs versions, digests, rate limits, credential helpers, ECR, token expiry? | ➖ client-side Helm |
| 365 | Flux and `OCIRepository`? | 🔧 #56 (source readiness in `flux status`) |

## 18. Namespaces

| # | Question | Answer with helm-debug |
|---|---|---|
| 366–368 | Missing namespace, `--create-namespace`, namespace on upgrade? | ➖ Helm CLI |
| 369 | Why is a release not found without `-n`? | ✅ `list -A --all` |
| 370 | Where is release metadata stored relative to the target namespace? | 🟡 in the namespace of the release; differing storage/release/object namespaces: #24 |
| 371–374 | Objects in other namespaces, cluster-scoped objects, same name in two namespaces? | ✅ `resources` (NAMESPACE column), `find -A` |
| 375–376 | Hardcoded namespace in a template; `.Release.Namespace`? | 🟡 `resources` shows objects outside the release namespace; full handling: #24 |

## 19. RBAC

| # | Question | Answer with helm-debug |
|---|---|---|
| 377–381 | Permissions Helm needs; can I create it; `auth can-i`; `Forbidden` on install; Deployment yes, ClusterRole no? | 🔧 #58 |
| 382–384 | Hook ServiceAccount; permission denied inside a hook; client vs pod permissions? | 🟡 hook pod logs: #47; client permissions: #58 |
| 385 | Does `lookup` need extra permissions? | ➖ yes, `get`/`list` on what it reads |
| 386 | GitOps controller ServiceAccount vs local user? | 🟡 #58 with `--as system:serviceaccount:flux-system:helm-controller` |

The permissions helm-debug itself needs are listed in [README → RBAC](../README.md#rbac).

## 20. Admission webhooks

| # | Question | Answer with helm-debug |
|---|---|---|
| 387–392 | `failed calling webhook`, `context deadline exceeded`, `failurePolicy: Fail`, a chart that blocks its own upgrade? | 🔧 #57 |
| 393 | Why do ingress-nginx admission Jobs show up among hooks? | ✅ `hooks REL` (they are pre-install/upgrade hooks that create the webhook certificate) |
| 394–398 | Webhook Service endpoints, CA bundle, certificate expiry, `x509: unknown authority`, deleting a webhook configuration? | 🔧 #57 |

## 21. Kubernetes events and workloads

| # | Question | Answer with helm-debug |
|---|---|---|
| 399–400 | Which events after a failure; events of one Deployment? | ✅ `health REL` (warning events of workloads, ReplicaSets and pods, last hour) |
| 401–407 | `FailedScheduling`, `FailedMount`, `FailedAttachVolume`, `ErrImagePull`, `ImagePullBackOff`, `CrashLoopBackOff`, readiness probe failures? | ✅ `health REL --logs 50` |
| 408–409 | Readiness/liveness probes and `--wait`? | ✅ `health` (not-ready replicas, restarts) |
| 410–413 | PDB blocks rollout; capacity; nodeSelector/affinity; taints? | ✅ `health` (events such as `FailedScheduling`) |
| 414–415 | PVC `Pending`, LoadBalancer provisioning? | 🔧 #46 |
| 416 | Rollout status apart from Helm? | ✅ `health REL --wait` |

## 22. `--wait`, timeouts and readiness

| # | Question | Answer with helm-debug |
|---|---|---|
| 417 | What does Helm consider Ready? | 🟡 `health` uses the same rules for Deployments/StatefulSets; all kinds: #46 |
| 418–421 | Default timeout, scope of the timeout, when to use `--wait`? | ➖ Helm CLI (5m, per operation) |
| 422–423 | `--wait-for-jobs`; Job finishes after Helm timed out? | 🔧 #46 |
| 424 | Which object is Helm waiting for right now? | 🔧 #46 |
| 425–427 | Helm timeout vs `progressDeadlineSeconds`; StatefulSet rollouts longer than 5m? | ✅ `health` reports `ProgressDeadlineExceeded` and StatefulSet progress |
| 428–430 | Just set 30m; picking timeouts; `context deadline exceeded`? | ➖ measure the rollout with `health --wait` first |

## 23. Uninstall

| # | Question | Answer with helm-debug |
|---|---|---|
| 431–434 | What uninstall does and deletes; history; `--keep-history`? | ➖ Helm concept; `history` shows `uninstalled` records |
| 435–436 | `upgrade --install` after `--keep-history`; `has no deployed releases`? | 🔧 #54 |
| 437–438 | `uninstalling`; why does uninstall hang? | 🔧 #54 (stuck uninstalling), #49 (finalizers) |
| 439–442 | Finalizers, `deletionTimestamp`, when to remove a finalizer? | 🔧 #49 |
| 443–446 | pre/post-delete hooks, `--no-hooks`? | 🟡 `hooks REL`; logs: #47 |
| 447–448 | `--wait` on uninstall; Namespace termination? | 🔧 #49 |
| 449 | CRD deleted but the manifest holds custom resources? | ✅ `resources` (`unknown-kind`) |
| 450–451 | Orphans after a failed uninstall; which objects belonged to the release? | ✅ `lost-found -A`, `manifests REL` |
| 452 | `helm.sh/resource-policy: keep`? | ✅ `lost-found` finds kept objects whose release is gone |
| 453 | Leftover release Secret after deleting the objects by hand? | ✅ `audit -A` (`orphaned`) |

## 24. Finalizers

| # | Question | Answer with helm-debug |
|---|---|---|
| 454–461 | What finalizers are, which controller owns one, risks of patching, dead operator, CRD finalizers, recovery order? | 🔧 #49 |

## 25. `helm.sh/resource-policy: keep`

| # | Question | Answer with helm-debug |
|---|---|---|
| 462–464 | What the annotation means, when it is used, uninstall? | ➖ Helm concept |
| 465–466 | Reinstall with the same object; ownership conflict? | ✅ `lost-found` (kept objects), `resources` (ownership on reinstall) |
| 467 | `keep` vs StatefulSet PVC retention? | ➖ Kubernetes concept |
| 468 | Kept Secret/ConfigMap with old data? | ✅ `drift REL` after reinstall shows the old payload |

## 26. Secrets and ConfigMaps

| # | Question | Answer with helm-debug |
|---|---|---|
| 469–472 | ConfigMap change without pod restart; checksum annotations; non-deterministic checksums? | 🟡 `diff REL A B` shows whether the checksum changed; pods not rolled out: #53 |
| 473–475 | Secrets in `--dry-run --debug`; hiding secrets while debugging? | ✅ `drift`/`diff` mask Secret data unless `--show-secrets`; `values` warns that values may hold secrets |
| 476–477 | Immutable Secrets? | 🔧 #50 |
| 478 | Secret rotation and Deployments? | 🔧 #53 (pods older than the latest template change) |

## 27. Release storage

| # | Question | Answer with helm-debug |
|---|---|---|
| 479–481 | What is in a release Secret; one Secret per revision; growth? | ✅ `audit` (revision counts and sizes) |
| 482 | `--history-max`? | ✅ `audit --max-revisions` flags releases that need it |
| 483–484 | Deleting old revision Secrets; restoring history? | 🟡 `history` shows what remains; backup: #55 |
| 485 | Backup before an emergency change? | 🔧 #55 |
| 486 | Storage other than Secrets? | ✅ `--storage configmap` (SQL driver is not supported) |
| 487 | How does corrupted metadata show up? | 🟡 decode errors in `history`/`list`; label/key mismatches: #45 |

## 28. `has no deployed releases`

| # | Question | Answer with helm-debug |
|---|---|---|
| 488–492 | Why `has no deployed releases`; all revisions failed; after `--keep-history`; recovery? | 🔧 #54; meanwhile `history REL` shows the statuses |
| 493 | Rollback to a superseded revision? | ✅ yes; `history` lists them, `stuck` suggests them |
| 494 | When to delete the metadata and reinstall? | 🟡 #54 prints the trade-off; back up with #55 |

## 29. Chart version vs appVersion

| # | Question | Answer with helm-debug |
|---|---|---|
| 495–496 | `version` vs `appVersion`; which one Helm uses? | ✅ `history` shows both; Helm selects by `version` |
| 497–500 | Image changed without a chart bump; mutable tags; which image really runs; appVersion is not the image? | 🔧 #53; `diff REL A B` shows image changes between revisions |

## 30. Drift

| # | Question | Answer with helm-debug |
|---|---|---|
| 501–504 | What drift is, how it appears, `kubectl edit`, detecting it? | ✅ `drift REL` (exit code 2 on drift) |
| 505 | Source of truth: manifest or live object? | ➖ Helm treats the stored manifest as desired state |
| 506–507 | Admission mutations and defaulted fields as diff noise? | ✅ `drift` ignores live-only fields and defaults |
| 508 | HPA-managed replicas? | 🔧 #52 |
| 509–511 | GitOps drift detection; Flux correction; when auto-correction is dangerous? | ➖ controller behavior; `drift` shows the edits a correction would revert |
| 512 | Exclude fields from drift detection? | 🔧 #52 |

## 31. Flux HelmRelease

| # | Question | Answer with helm-debug |
|---|---|---|
| 513 | Problem in the HelmRelease or the chart? | ✅ `flux status HR` (conditions next to Helm history) |
| 514–520 | `.status.conditions`, `Reconciling`, `Ready=False`, `InstallFailed`, `UpgradeFailed`, `TestFailed`, `Stalled`? | ✅ `flux status HR` |
| 521–522 | HelmRelease events; helm-controller logs? | ➖ `kubectl events --for helmrelease/HR`, `flux logs` |
| 523–526 | `lastAttemptedRevision`, `lastAttemptedReleaseAction`, failure counters, reset? | 🔧 #56 |
| 527–529 | `flux reconcile`, force, reset remediation? | ➖ Flux CLI |
| 530–533 | Install/upgrade remediation, rollback vs uninstall, `RetryOnFailure`? | 🔧 #56 (shows the configured remediation and what happens next) |
| 534–539 | `valuesFrom` missing ConfigMap/Secret; HelmRepository/HelmChart/OCIRepository not Ready? | 🔧 #56 |
| 540–541 | `dependsOn` blocking; dependency cycles? | 🔧 #56 |
| 542–543 | Failed uninstall of a HelmRelease; disabling hooks for uninstall? | 🟡 `flux status`, `hooks`; finalizers: #49 |
| 544 | CRD `Create`/`CreateReplace` policy? | 🔧 #48 shows the resulting CRD state |
| 545–546 | Manual `helm upgrade` of a Flux release; next reconcile? | ✅ `history` shows the manual revision; `flux status` the reconciliation |

## 32. Argo CD and Helm

| # | Question | Answer with helm-debug |
|---|---|---|
| 547–550 | Does Argo use Helm as release manager; where is history; why `helm list` is empty; who manages lifecycle? | ✅ Argo renders with `helm template` and keeps no Helm records (ADR-0004); use `argo resources APP` |
| 551–553 | How Argo passes values, precedence, different rendering? | ➖ Argo Application spec (`spec.source.helm`) |
| 554 | Argo sync hooks vs Helm hooks? | ✅ `argo resources` lists hooks separately |
| 555–557 | Manual change reverted by Argo; `OutOfSync` after a good render? | ✅ `argo resources APP` (SYNC, HEALTH, TRACKING per object) |
| 558 | `ignoreDifferences`? | ➖ Argo Application spec |

## 33. Helm tests

| # | Question | Answer with helm-debug |
|---|---|---|
| 559–561 | What `helm test` does; how a chart defines tests; where results are? | ✅ `hooks REL` (event `test`, phase, last run) |
| 562 | Logs of a test pod? | 🔧 #47 |
| 563 | What happens to test objects? | ✅ `hooks` (delete policy, `orphan`) |
| 564–567 | Does a failed test fail the release; GitOps and tests; smoke tests? | 🟡 `flux status` shows `TestFailed`; testing strategy is ➖ |

## 34. Post-renderers

| # | Question | Answer with helm-debug |
|---|---|---|
| 568–569 | What a post-renderer is, when it runs? | ➖ Helm concept |
| 570 | Why the manifest after a post-renderer differs from `helm template`? | ✅ `manifests REL` shows the stored, post-rendered manifest |
| 571–575 | Debugging a post-renderer, exit 1, Kustomize, immutable fields, same post-renderer in CI? | 🟡 `diff REL A B` shows its effect between revisions; immutable fields: #50; the rest is ➖ |

## 35. Security and policy

| # | Question | Answer with helm-debug |
|---|---|---|
| 576–578 | Kyverno/Gatekeeper blocking install; policy violation in Helm output? | 🔧 #57 |
| 579–580 | Privileged chart works in dev, not prod; Pod Security Standards? | 🟡 `health` events (`FailedCreate` with the PSA message) |
| 581–582 | Injected ServiceAccount; mutation and diffs? | ✅ `drift` ignores live-only fields, so injections do not show as drift |
| 583–584 | Image signature policy; registry allowlist after a successful upgrade? | ✅ `health` (`FailedCreate`, `ErrImagePull` events) |

## 36. Networking

| # | Question | Answer with helm-debug |
|---|---|---|
| 585–590 | DNS, lost API connection, TCP reset, API timeout, proxies, intermittent failures during Helm? | 🟡 any of them can leave a pending release: `stuck`; `-v` logs the API requests of helm-debug |
| 591–593 | Install succeeded but the Service is unreachable; Helm vs Ingress/LB? | 🟡 `health` (pods), `resources` (objects present); LoadBalancer address: #46 |

## 37. Production scenarios

| # | Question | Answer with helm-debug |
|---|---|---|
| 594 | Upgrade aborted after half of the objects? | ✅ `stuck`, `drift REL` (what was applied), then the suggested rollback |
| 595 | `pending-upgrade` but pods are running? | ✅ `stuck` (fix), `health` (workloads are fine), `drift` |
| 596 | `deployed` but pods CrashLoop: did Helm succeed? | ✅ `health REL --logs 50` (exit code 2) |
| 597 | Upgrade timed out, a minute later the Deployment is Ready? | ✅ `history` shows `failed`; `health` shows it healthy now |
| 598 | CI died during an upgrade: safe next step? | ✅ `stuck` (revision, age, fix), `diff REL N-1 N` |
| 599 | Rollback returns an immutable-field error? | 🔧 #50 |
| 600 | CRD stayed old after upgrade: how to prove it? | 🔧 #48 |
| 601 | Uninstall hangs for 30 minutes: first checks? | 🟡 `stuck` (#54 for `uninstalling`), `hooks`, `resources` (#49 for finalizers) |
| 602 | pre-delete Job fails because a Secret is gone? | 🟡 `hooks REL`; logs: #47 |
| 603 | Record shows a pending operation but no Helm process exists? | ✅ `stuck` |
| 604 | Old API removed after a Kubernetes upgrade? | ✅ `deprecated REL`; fix with `helm-mapkubeapis` |
| 605 | Object carries annotations of another release: safe migration? | ✅ `find KIND/NAME`, `resources` (adoption commands) |
| 606 | Installs locally, fails through Flux? | 🟡 `flux status`, `values REL`; sources and `valuesFrom`: #56 |
| 607 | Works with Helm CLI but not ArgoCD? | ✅ `argo resources APP`; rendering differences are ➖ |
| 608–609 | `template` passes, `--dry-run=server` fails; dry-run passes, install fails? | 🟡 API/admission/RBAC: `resources`, #57, #58 |
| 610 | Upgrade succeeded but the app uses the old ConfigMap? | 🟡 `drift REL` (ConfigMap content), pods not restarted: #53 |
| 611 | Values right, manifest right, live object different? | ✅ `drift REL` |
| 612–613 | Live object right, pod spec old; StatefulSet does not update pods? | ✅ `health` (StatefulSet partition, `OnDelete`); images per pod: #53 |
| 614 | New ReplicaSet never becomes Ready: is Helm to blame? | ✅ `health REL --logs 50` |
| 615 | Timeout only in prod: cluster differences? | 🔧 #26 (compare contexts), #46 |
| 616 | Chart stopped uninstalling after a Kubernetes upgrade? | 🟡 `deprecated REL`; finalizers: #49 |
| 617 | Install hangs only with `--wait`? | 🟡 the blocker is readiness, not the API: `health`, all kinds: #46 |
| 618 | `--atomic` removes the failed release: how to keep evidence? | 🔧 #55 |
| 619 | Post-mortem without CI logs? | ✅ `history` (reasons), `diff`, `drift`; a timeline of all releases: #44 |
| 620 | What to save automatically on a failed deployment? | 🔧 #55 |
