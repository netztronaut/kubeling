# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A minimal out-of-tree [cloud-controller-manager](https://kubernetes.io/docs/concepts/architecture/cloud-controller/)
for clusters with no real cloud backing them. It exists so kubelets can run
with `--cloud-provider=external` without a real cloud API to talk to. It does
three things:

1. Stamps `Node.spec.providerID = <provider-id>://<node-name>` (default
   scheme `custom`) and removes the `node.cloudprovider.kubernetes.io/uninitialized`
   taint the kubelet sets in external mode.
2. Optionally applies `externalIPs`, labels, and annotations to Nodes,
   each from its own independently-reconciled rule map read live from a
   ConfigMap (never mounted as a volume — read via the Kubernetes API so
   changes apply within seconds, no pod restart). Every rule, in every map,
   is matched by any combination (ANDed) of `nodeSelector`, node-affinity-style
   `selectorTerms`, and a regex against `providerID`.
3. Each of those three domains maintains its own `kubeling.io/` NodeCondition
   (`Labeled`, `Annotated`, `ExternalIPsApplied`) — present on a Node only
   while at least one rule for that domain currently matches it, tracking
   live applicability (`Pending`/`Applied`) separately from the
   labels/annotations/addresses it already wrote, which are never removed
   automatically.

There is intentionally no instance metadata, zone/region, or load balancer
support — see README.md for the full behavioral spec (rule semantics,
conflict resolution, flags).

## Commands

```sh
make check                     # everything CI runs: vet, lint, test, helm-lint, helm-test
make test                      # go test -race -cover ./...
make lint                      # golangci-lint (pinned, installed into bin/)
go test ./pkg/controller/ -run TestMergeExternalIPs -v   # single test
make build                     # bin/kubeling
make image                     # multi-arch image, pushed to ghcr.io/netztronaut/kubeling
make deploy VALUES=<file>      # helm upgrade --install in the current kube context
```

The repository is hosted on GitHub at `github.com/netztronaut/kubeling` and
mirrored to a private Forgejo instance. CI just runs `make check` on
`runs-on: ubuntu-latest`, in `.github/workflows/ci.yml` and, for the mirror,
`.forgejo/workflows/ci.yml`. Lint rules live in `.golangci.yml` (notably:
use `slices`, not `sort`). `go.mod` pins `toolchain go1.26.8` because the
auto-selected go1.26.0 toolchain breaks coverage builds.

## Releasing

`.github/workflows/release.yml` runs four jobs in sequence on pushes to
`main`, `v*` tags, pull requests and manual dispatch:

1. **`version`** derives everything else from the ref and
   `charts/kubeling/Chart.yaml`'s `version`: on `main`, chart
   `<version>-main.<run>` pinned to image `sha-<short>`; on a `vX.Y.Z[-*]`
   tag, chart and appVersion `X.Y.Z[-*]`, and the job **fails unless the
   tag's `X.Y.Z` equals `Chart.yaml`'s `version`**; anywhere else,
   `<version>-dev.<run>` with nothing pushed.
2. **`image`** builds `linux/amd64,linux/arm64` with docker/metadata-action
   tags (`latest`/`main`/`sha-<short>` on `main`; `X.Y.Z[-*]` on tags,
   plus `X.Y` for final releases) and pushes to `ghcr.io/netztronaut/kubeling`.
3. **`chart`** runs `helm package --version --app-version` (Chart.yaml's
   `appVersion: "latest"` is always overridden) and pushes to
   `oci://ghcr.io/netztronaut/charts/kubeling`; on tags it also uploads the
   `.tgz` as the `chart` artifact.
4. **`release`** runs GoReleaser (`.goreleaser.yaml`): on tags it creates
   the GitHub release with linux/darwin × amd64/arm64 archives,
   `checksums.txt`, the changelog and the chart `.tgz` as an extra file
   (`-*` tags become prereleases); everywhere else it builds a `--snapshot`.

To cut a release: bump `version` in `Chart.yaml` and the pinned
`--version` example in README.md, push `main` to both remotes, wait for
its CI and Release runs, then push an annotated `vX.Y.Z` tag to both.
Push named refs only (`main`, `vX.Y.Z`), never `--tags` or `--mirror`.

Gotchas learned the hard way:

- **Snapshot builds skip publishing**, so pull requests and `main` never
  exercise the release upload. Problems there (like `extra_files`) only
  show up in the `release` job of a tag run; always watch it.
- **GoReleaser's `extra_files` globs don't support absolute paths**
  (`stat static prefix .//home/...: invalid argument`). The chart artifact
  is therefore downloaded into the git-ignored `.release/chart` inside the
  checkout (`CHART_PACKAGE_DIR`); `dist/` won't do, since `--clean` wipes it.
- A failed `release` job leaves an **empty draft release**, which a re-run
  doesn't reuse (it looks releases up by tag, and drafts aren't attached to
  one). Delete the draft by hand.
- Re-running a tag's workflow uses the workflow file **at the tagged
  commit**, so a workflow fix needs a new tag or a moved tag. If a tag push
  doesn't start a run, `gh workflow run release.yml --ref vX.Y.Z` works;
  `version` still sees `refs/tags/...`.
- **New ghcr.io packages start private**, regardless of repo visibility
  (e.g. after the repository is recreated), and visibility can only be
  changed in the package settings UI, not via the API.
- To verify anonymous pulls, use the raw registry API (anonymous token from
  `https://ghcr.io/token?scope=repository:<repo>:pull&service=ghcr.io`,
  then the manifest). `docker pull`/`helm pull` can silently pick up stored
  credentials even with empty `DOCKER_CONFIG`/`HELM_REGISTRY_CONFIG`.

README.md's "Images and charts" documents the tag scheme for users.

## Architecture

Single binary, `cmd/kubeling/main.go`, wiring up to four independent
controllers on top of one shared `informers.SharedInformerFactory` (Node
informer). All controllers are plain client-go workqueue-based controllers,
keyed per-Node, not controller-runtime. Every controller's matching logic is
purely a function of that one Node's own state, so there is only one
reconcile shape: enqueue Node name → worker → `reconcile(ctx, nodeName)`.

- **`pkg/controller/controller.go`** — `nodeQueue`, the workqueue plumbing
  every controller embeds (event handler registration, `Run`, `EnqueueAll`,
  rate-limited retries around a `reconcile` func), and `ConfigSource`, the
  one-method interface (`Current() config.Config`) rule controllers read
  configuration through. `*config.Watcher` implements it; tests use a
  static implementation.

- **`pkg/controller.NodeController`** (`node_controller.go`) — always
  active. Per-Node reconcile: set `providerID` if empty, strip the
  `uninitialized` taint if present. Conflict errors are swallowed (the
  informer will observe the newer version and requeue).

- **`pkg/config.Watcher`** (`config/watcher.go`) — a single-ConfigMap
  informer (field-selected by name) that parses the `config.yaml` key with
  `config.Parse` and stores the result in an `atomic.Pointer`. `Parse`
  decodes strictly (`yaml.UnmarshalStrict` — unknown keys are errors) and
  runs `Config.Validate` (every `providerIDPattern` must compile, every
  `selectorTerms` must parse as scheduler node-affinity terms); on either
  failure the previous configuration is kept. A missing key or deleted
  ConfigMap clears the configuration. `OnChange` is a caller-supplied hook
  fired on every successful load/clear. Only instantiated when
  `--configmap`/`CONFIGMAP` is set; the three rule controllers below are
  nil/absent otherwise.

- **`pkg/controller.MetadataController[T]`** (`metadata_controller.go`) — a
  single generic implementation (`T` = `config.LabelRule` or
  `config.AnnotationRule`) backing both `NewLabelController` (reads
  `Config.Labels`, writes `node.Labels`, maintains `LabeledConditionType`)
  and `NewAnnotationController` (same shape, `Config.Annotations` /
  `node.Annotations` / `AnnotatedConditionType`). What differs between the
  two domains is captured entirely in a `metadataDomain[T]` struct of
  closures. `reconcile` applies **at most one write per call** — clear a
  stale condition, set `Pending`, write the metadata, or set `Applied` —
  relying on the informer's `UpdateFunc` to re-enqueue the Node after each
  write so the next step runs against a fresh object (up to 3 passes from
  cold: Pending → metadata write → Applied). It always evaluates the Node,
  even with an empty rule map, so a condition is cleared once no rule
  matches anymore. `resolveValues` merges a domain's values across matching
  rules per key; a same-key conflict is logged and the key is left out
  entirely. `applyOverwrite` computes the resulting map and whether
  anything changed. Values already applied are never removed automatically
  — the condition tracks live applicability while the values are permanent.

- **`pkg/controller.ExternalIPController`** (`externalip_controller.go`) —
  same per-Node reconcile shape as `MetadataController`, but not built on
  it: `externalIPs` are a union (not an authoritative overwrite) written to
  `status.addresses` via `UpdateStatus`, and matching rules' IP lists are
  simply concatenated. `mergeExternalIPs` unions existing + rule-supplied
  ExternalIP addresses, dedupes, and reorders the full ExternalIP block
  (IPv6 before IPv4, stable within family) — every touch renormalizes
  ordering. Maintains `ExternalIPsAppliedConditionType` with the same
  Pending/Applied/absent semantics as `MetadataController`.

- **`pkg/controller/match.go`** — `matches(node, config.Match)` is the one
  place nodeSelector/selectorTerms/providerIDPattern matching happens
  (`selectorTerms` are evaluated with the scheduler's own
  `k8s.io/component-helpers/.../nodeaffinity`; compiled patterns are cached); `matchingIDs[T]` returns the sorted rule IDs matching a Node
  from any of the three rule maps.

- **`pkg/controller/conditions.go`** — the three `kubeling.io/...`
  condition types, `pendingCondition`/`appliedCondition` builders,
  `conditionUpToDate`/`setCondition` (preserves `LastTransitionTime` when
  status hasn't changed), `removeCondition`/`hasCondition`, and
  `ensureCondition`/`ensureConditionAbsent`, which perform the status write
  (swallowing conflicts) for both rule controllers.

- **`pkg/config/ref.go`** — `ParseRef` splits a `"(namespace/)name"` flag
  value; `OwnNamespace` reads the projected service-account namespace file,
  falling back to `"default"` outside a cluster.

`main.go` wires `watcher.OnChange` to call `EnqueueAll()` on all three rule
controllers whenever the ConfigMap changes. Leader election wraps a
`run(ctx)` closure that starts the shared informer factory and all
controllers' `Run` loops; with `--leader-elect=false` it's called directly.
`OnStoppedLeading` calls `os.Exit(0)` rather than just canceling a context,
since there's nothing to gracefully drain.

## Tests

- Pure helpers have table tests next to them.
- Reconcile loops are tested against `k8s.io/client-go/kubernetes/fake`
  through the `harness` in `pkg/controller/controller_test.go`: its Node
  informer is never started, `sync` copies a Node from the fake API into the
  informer cache by hand, and `converge` calls `reconcile` until it stops
  writing, returning the number of writes.
- `pkg/config/examples_test.go` validates `charts/kubeling/ci/*.yaml` and
  `deploy/examples/configmap.yaml` with `config.Parse` — keep those examples
  covering every rule map when the schema changes.
- `hack/test-chart.sh` (`make helm-test`) renders the chart and greps the
  output.

## Deployment layout

- `charts/kubeling/` — the Helm chart (see its own README for the values
  reference); `ci/rules-values.yaml` is the complete rule example.
- `deploy/` — equivalent plain manifests (`kubectl apply -f deploy/`);
  `deploy/examples/` holds an example rule ConfigMap that the non-recursive
  apply deliberately skips.

Cluster-specific values files don't belong in this repository; they live in
the GitOps repository of the respective cluster.

Both the chart and the plain manifests run the Deployment with
`hostNetwork: true`, tolerations for the control-plane, `uninitialized` and
`not-ready` taints, and a *preferred* (not required) node affinity for
control-plane nodes, to avoid the chicken-and-egg problem of the controller
itself needing an initialized Node to be scheduled. Read README.md's Deploying section before changing scheduling —
the reasoning behind those defaults, and what to adjust for managed control
planes, is documented there rather than in code.
