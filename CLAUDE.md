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
   is matched by `nodeSelector` and/or a regex against `providerID`.
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
go build ./...                 # build
go vet ./...                   # vet
go test ./...                  # all tests
go test ./pkg/controller/ -run TestMergeExternalIPs -v   # single test
go build -o bin/kubeling ./cmd/kubeling
```

Docker image (multi-arch, pushes):
```sh
docker buildx build --platform=linux/amd64,linux/arm64 -t <repo>:<tag> --push .
```

Helm deploy (see Makefile for the `git.example.com` default repo/values used there):
```sh
helm upgrade --install kubeling charts/kubeling [--values=<file>] --debug
```

There is no lint config beyond `go vet`; no CI config in-repo.

## Architecture

Single binary, `cmd/kubeling/main.go`, wiring up to four independent
controllers on top of one shared `informers.SharedInformerFactory` (Node
informer). All controllers are plain client-go workqueue-based controllers,
keyed per-Node (`AddEventHandler` → enqueue Node name → worker loop →
`reconcile(ctx, nodeName)`), not controller-runtime. There is deliberately
only one reconcile shape in this codebase now — every controller's matching
logic is purely a function of that one Node's own state, so there's no
correctness reason for a controller to work off a global sync key instead.

- **`pkg/controller.NodeController`** (`node_controller.go`) — always
  active. Per-Node reconcile: set `providerID` if empty, strip the
  `uninitialized` taint if present. Conflict errors are swallowed (the
  informer will observe the newer version and requeue).

- **`pkg/config.Watcher`** (`config/watcher.go`) — a single-ConfigMap
  informer (field-selected by name) that parses the `config.yaml` key into a
  `config.Config` and stores it in an `atomic.Pointer`. Rejects (and keeps
  the previous configuration on) both a YAML parse failure and a
  `Config.Validate` failure — every rule's `providerIDPattern`, across all
  three rule maps, must compile as a regex. `OnChange` is a caller-supplied
  hook fired on every successful load/clear. Only instantiated when
  `--configmap`/`CONFIGMAP` is set; the three rule controllers below and
  this watcher are all nil/absent otherwise.

- **`pkg/controller.MetadataController[T]`** (`metadata_controller.go`) — a
  single generic implementation (`T` = `config.LabelRule` or
  `config.AnnotationRule`) backing both `NewLabelController` (reads
  `Config.Labels`, writes `node.Labels`, maintains `LabeledConditionType`)
  and `NewAnnotationController` (same shape, `Config.Annotations` /
  `node.Annotations` / `AnnotatedConditionType`). What differs between the
  two domains is captured entirely in a `metadataDomain[T]` struct of
  closures passed to `newMetadataController`; the reconcile logic, workqueue
  plumbing, and condition management are written once. `reconcile` applies
  **at most one write per call** — clear a stale condition, set `Pending`,
  write the metadata, or set `Applied` — relying on the informer's
  `UpdateFunc` to re-enqueue the Node after each write so the next step runs
  against a fresh object (this can take up to 3 passes to converge from
  cold: Pending → metadata write → Applied). `resolveValues` merges a
  domain's values across matching rules per key; a same-key conflict is
  logged and the key is left out entirely (never fought over, never picked
  arbitrarily). `applyOverwrite` computes the resulting map and whether
  anything changed. Values already applied are never removed automatically,
  even if the rule is later changed/removed or the Node stops matching —
  but the domain's condition *is* removed once no rule matches anymore
  (`ensureConditionAbsent`), since the condition tracks live applicability
  while the values themselves are permanent.

- **`pkg/controller.ExternalIPController`** (`externalip_controller.go`) —
  same per-Node reconcile shape as `MetadataController`, but not built on
  it: `externalIPs` are a union (not an authoritative overwrite) written to
  `status.addresses` via `UpdateStatus`, not `node.Labels`/`Annotations` via
  `Update`, and matching rules don't need `resolveValues`'s conflict
  handling since their IP lists are simply concatenated. `mergeExternalIPs`
  unions existing + rule-supplied ExternalIP addresses, dedupes, and
  reorders the full ExternalIP block (IPv6 before IPv4, stable within
  family) — every touch renormalizes ordering, not just newly-added
  addresses. Maintains `ExternalIPsAppliedConditionType` with the same
  Pending/Applied/absent semantics as `MetadataController`.

- **`pkg/controller/match.go`** — `matches(node, config.Match)` is the one
  place nodeSelector/providerIDPattern matching happens, shared by all three
  rule controllers; `matchingIDs[T]` (generic) returns the sorted list of
  rule IDs matching a Node from any of the three rule maps.

- **`pkg/controller/conditions.go`** — condition helpers shared by
  `MetadataController` and `ExternalIPController`: the three
  `kubeling.io/...ConditionType` constants, `pendingCondition`/
  `appliedCondition` builders, `conditionUpToDate`/`setCondition`
  (preserves `LastTransitionTime` when status hasn't changed) and
  `removeCondition`/`hasCondition`.

- **`pkg/config/ref.go`** — `ParseRef` splits a `"(namespace/)name"` flag
  value; `OwnNamespace` reads the projected service-account namespace file,
  falling back to `"default"` outside a cluster.

`main.go` wires `watcher.OnChange` to call `EnqueueAll()` on all three rule
controllers (re-evaluating every Node) whenever the ConfigMap changes.
Leader election (`main.go`) wraps a `run(ctx)` closure that starts the
shared informer factory and all controllers' `Run` loops; with
`--leader-elect=false` it's called directly instead of via
`leaderelection.RunOrDie`. `OnStoppedLeading` calls `os.Exit(0)` rather than
just canceling a context, since there's nothing to gracefully drain.

## Deployment layout

- `charts/kubeling/` — the Helm chart (see its own
  README for the values reference).
- `deploy/` — equivalent plain manifests (`kubectl apply -f deploy/`).

Both schedule the Deployment on control-plane nodes with `hostNetwork: true`
and tolerations for the control-plane and `uninitialized` taints, to avoid
the chicken-and-egg problem of the controller itself needing an initialized
Node to be scheduled. Read README.md's Deploying section before changing
scheduling — the reasoning behind those defaults, and what to adjust for
managed control planes, is documented there rather than in code.
