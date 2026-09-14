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
2. Optionally applies `externalIPs`/labels/annotations to Nodes based on
   policies read live from a ConfigMap (never mounted as a volume — read via
   the Kubernetes API so changes apply within seconds, no pod restart).
3. Optionally, from the same ConfigMap, one-time labels a Node whose
   `providerID` matches a configured regex and reflects the outcome in a
   `kubeling.io/Onboarded` NodeCondition (`Pending`/`Onboarded`).

There is intentionally no instance metadata, zone/region, or load balancer
support — see README.md for the full behavioral spec (policy semantics,
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

Single binary, `cmd/kubeling/main.go`, wiring up to three
independent controllers on top of one shared `informers.SharedInformerFactory`
(Node informer). All controllers are plain client-go
workqueue-based controllers (`AddEventHandler` → enqueue → worker loop →
`reconcile`), not controller-runtime.

- **`pkg/controller.NodeController`** (`node_controller.go`) — keyed by Node
  name. Per-Node reconcile: set `providerID` if empty, strip the
  `uninitialized` taint if present. Conflict errors are swallowed (the
  informer will observe the newer version and requeue).

- **`pkg/config.Watcher`** (`config/watcher.go`) — a single-ConfigMap
  informer (field-selected by name) that parses the `config.yaml` key into a
  `config.Config` and stores it in an `atomic.Pointer`. Rejects (and keeps
  the previous configuration on) both a YAML parse failure and a
  `Config.Validate` failure — currently just "every `onboarding` rule's
  `providerIDPattern` compiles as a regex". `OnChange` is a caller-supplied
  hook fired on every successful load/clear. Only instantiated when
  `--configmap`/`CONFIGMAP` is set; the policy and kubeling controllers and
  this watcher are all nil/absent otherwise.

- **`pkg/controller.PolicyController`** (`policy_controller.go`) — *not*
  keyed per-Node. It has a single workqueue item (`syncKey`) because policy
  application depends on the whole Node set and whole policy set together.
  Triggered by Node add/update/delete *and* by `watcher.OnChange` (wired in
  `main.go`: `watcher.OnChange = pc.Enqueue`). Each reconcile pass
  (`applyOnePass`) walks Nodes in sorted-name order, finds matching policies
  by `nodeSelector`, and applies **at most one field-group change per pass**
  (labels+annotations together, else externalIPs) before returning `changed
  = true` and restarting the whole pass from a fresh Node listing. This
  keeps each individual Update/UpdateStatus call working against a
  known-fresh object. `resolveKeyValues` merges labels/annotations across
  matching policies per key; on a same-key value conflict between policies
  it logs and leaves that key untouched on both sides (never fought over,
  never picked arbitrarily). `mergeExternalIPs` unions existing +
  policy-supplied ExternalIP addresses, dedupes, and reorders the full
  ExternalIP block (IPv6 before IPv4, stable within family) — every touch
  renormalizes ordering, not just newly-added addresses. Nothing is ever
  removed automatically when a policy is deleted or a Node stops matching.

- **`pkg/controller.KubelingController`** (`kubeling_controller.go`) —
  keyed by Node name, like `NodeController`. Per-Node reconcile against the
  same `watcher.Current().Onboarding` rules (regex on `providerID` → label
  key/value). At most one write per reconcile call — the label `Update`, or
  the `kubeling.io/Onboarded` condition `UpdateStatus` — relying on the
  informer's own `UpdateFunc` to re-enqueue the Node after the label write
  so the condition write follows on a fresh object, the same trick
  `PolicyController` uses across a whole pass but applied per-Node here
  since this controller is keyed per-Node rather than by a single sync key.
  `EnqueueAll` (wired into the same `watcher.OnChange` as
  `PolicyController.Enqueue`) re-evaluates every Node when the ConfigMap
  changes. Onboarding is a one-time stamp, never re-applied or removed once
  a Node reaches `Onboarded` — same "nothing removed automatically"
  convention as `PolicyController`'s externalIPs/labels.

- **`pkg/config/ref.go`** — `ParseRef` splits a `"(namespace/)name"` flag
  value; `OwnNamespace` reads the projected service-account namespace file,
  falling back to `"default"` outside a cluster.

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
