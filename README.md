# Kubeling

A companion to the [cloud-controller-manager](https://kubernetes.io/docs/concepts/architecture/cloud-controller/)
that manages the parts of a Node outside a cloud-controller-manager's
responsibility. A cloud-controller-manager initializes Nodes from what the
cloud knows about them; Kubeling applies what the cluster operator knows:
`externalIPs`, labels and annotations, declared as rules in a ConfigMap and
kept applied as Nodes come and go.

## What it does

### Node rules

Kubeling reads rules from a ConfigMap (via the Kubernetes API, so edits
apply within seconds without a restart) and applies them to every Node they
match:

- **`externalIPs`** are added to `status.addresses`.
- **Labels** are set on the Node.
- **Annotations** are set on the Node.

Each rule is matched by any combination of `nodeSelector`,
node-affinity-style `selectorTerms` and a `providerID` regex, and each
domain reports its progress through its own `kubeling.io/` NodeCondition.
See [Rule configuration](#rule-configuration).

### Node initialization

Kubeling also covers the minimal cloud-controller-manager contract for
clusters with no cloud behind them — bare-metal, on-prem or dev clusters
where kubelets must run with `--cloud-provider=external` (required on modern
Kubernetes, where in-tree cloud providers are gone) but there's no cloud API
to talk to.

When a kubelet starts with `--cloud-provider=external` it:

1. Registers its Node with the taint `node.cloudprovider.kubernetes.io/uninitialized:NoSchedule`.
2. Leaves `Node.spec.providerID` empty.

A cloud-controller-manager is expected to initialize the Node and remove
that taint so normal pods can be scheduled. Kubeling:

- Sets `Node.spec.providerID` to `custom://<node-name>` if it isn't already set.
- Removes the `node.cloudprovider.kubernetes.io/uninitialized` taint.

The provider ID scheme is `custom` (configurable via `--provider-id`). There
is no instance metadata, zone/region, or load balancer support — that is the
cloud-controller-manager's domain.

Node initialization is currently always active. Next to a real
cloud-controller-manager, Kubeling may therefore stamp a `custom://`
providerID or remove the taint before the cloud-controller-manager has
initialized the Node.

## Images and charts

The source lives at
[github.com/netztronaut/kubeling](https://github.com/netztronaut/kubeling).
The [Release workflow](.github/workflows/release.yml) publishes a
multi-arch (`linux/amd64`, `linux/arm64`) image to
`ghcr.io/netztronaut/kubeling` and the Helm chart to
`oci://ghcr.io/netztronaut/charts/kubeling`:

| Trigger | Image tags | Chart version |
| --- | --- | --- |
| Push to `main` | `latest`, `main`, `sha-<short-sha>` | `<Chart.yaml version>-main.<run>`, pinned to `sha-<short-sha>` |
| Tag `vX.Y.Z` | `X.Y.Z`, `X.Y` | `X.Y.Z`, pinned to image `X.Y.Z` |
| Tag `vX.Y.Z-rc.N` | `X.Y.Z-rc.N` | `X.Y.Z-rc.N`, pinned to image `X.Y.Z-rc.N` |

A release tag's `X.Y.Z` must match `version` in
[`charts/kubeling/Chart.yaml`](charts/kubeling/Chart.yaml), so bump it
before tagging. Once the image and chart are published, a tag also becomes a
[GitHub release](https://github.com/netztronaut/kubeling/releases), created
by [GoReleaser](.goreleaser.yaml) with `linux` and `darwin` binaries
(`amd64`, `arm64`), checksums, a changelog and the packaged chart; `-rc.N`
tags are marked as prereleases. Pull requests build everything, including a
GoReleaser snapshot, without pushing or releasing.

To build and push the image yourself instead:

```sh
make image                                  # ghcr.io/netztronaut/kubeling:latest
make image IMAGE_REPOSITORY=registry.example.com/kubeling IMAGE_TAG=v0.2.0
```

Or build the binary locally with `make build` (writes `bin/kubeling`).

## Deploying

### Helm

The chart is published to `oci://ghcr.io/netztronaut/charts/kubeling`
(see [Images and charts](#images-and-charts)); its source is
[`charts/kubeling`](charts/kubeling):

Install the latest release (no registry login needed — the chart and image
are public):

```sh
helm install kubeling oci://ghcr.io/netztronaut/charts/kubeling \
  --namespace kube-system
```

Pin a release with `--version`, which also pins the image, since every
released chart deploys the image tag matching its own version:

```sh
helm install kubeling oci://ghcr.io/netztronaut/charts/kubeling \
  --namespace kube-system \
  --version 0.2.3
```

Upgrade an existing installation to the latest release (or to a pinned one
with `--version`), keeping the values you set before while picking up the
new chart's defaults:

```sh
helm upgrade kubeling oci://ghcr.io/netztronaut/charts/kubeling \
  --namespace kube-system \
  --reset-then-reuse-values
```

To try unreleased changes, add `--devel` to pick up the newest
`-main.<run>` build of `main`, or install from a checkout with
`./charts/kubeling`.

`make deploy` wraps `helm upgrade --install` for the current kube context
and accepts `VALUES=<file>` and `NAMESPACE=<namespace>`.

See the [chart README](charts/kubeling/README.md) for the full values reference.

### Plain manifests

Manifests are in [`deploy/`](deploy/):

- `service-account.yaml` — the `kubeling` ServiceAccount in `kube-system`.
- `clusterrole.yaml` — cluster-wide permission to read/update Nodes, Node status, ConfigMaps, and emit Events.
- `role.yaml` — namespaced permission to manage the leader-election `Lease` in `kube-system`.
- `deployment.yaml` — a 2-replica Deployment with leader election enabled.
- `examples/configmap.yaml` — an example rule ConfigMap. It lives in a
  subdirectory so `kubectl apply -f deploy/` doesn't pick it up; adapt it
  and uncomment the `CONFIGMAP` env var in `deployment.yaml` to use it.

```sh
kubectl apply -f deploy/
```

Both the chart and the plain manifests prefer control-plane nodes (a soft
node affinity on `node-role.kubernetes.io/control-plane`), tolerate the
control-plane, `node.cloudprovider.kubernetes.io/uninitialized` and
`not-ready` taints, and run with `hostNetwork: true`. This avoids the
chicken-and-egg problem where the controller manager itself is a pod that
needs a Node to be initialized before it can be scheduled. Because the
affinity is only a preference, the controller still schedules on clusters
without schedulable control-plane nodes (e.g. managed control planes); set a
`nodeSelector` or required node affinity if it must run on specific nodes.
With `hostNetwork`, the health port (`10258`) has to be free on that node.

### Running kubelets with the external cloud provider

Once the controller manager is deployed, start kubelets with:

```
--cloud-provider=external
```

New Nodes will show the `node.cloudprovider.kubernetes.io/uninitialized`
taint until this controller processes them, which normally happens within
seconds of the Node object appearing.

## Rule configuration

The controller can optionally read rules from a ConfigMap and apply them to
Nodes. Point it at one with `--configmap=(namespace/)name` or the
`CONFIGMAP` environment variable (the flag wins if both are set); the
namespace defaults to the controller's own namespace when omitted. The
ConfigMap is read via the Kubernetes API (get/list/watch) — it is never
mounted as a volume — so changes take effect within seconds, without a pod
restart. Leave it unset to disable rule processing entirely.

The ConfigMap must have a `config.yaml` key holding a YAML document with up
to three independent rule maps at the root — `externalIPs`, `labels`,
`annotations` — each keyed by an arbitrary rule ID and reconciled by its
own controller:

```yaml
externalIPs:
  edge:
    nodeSelector:
      topology.kubernetes.io/zone: eu-central-1a
    externalIPs:
      - 203.0.113.10
labels:
  edge:
    providerIDPattern: '^custom://edge-'
    selectorTerms:
      - matchExpressions:
          - key: topology.kubernetes.io/zone
            operator: In
            values:
              - eu-central-1a
              - eu-central-1b
    labels:
      environment: production
annotations:
  edge:
    nodeSelector:
      topology.kubernetes.io/zone: eu-central-1a
    annotations:
      example.com/rack: r42
```

Every rule, in every one of the three maps, is matched the same way:

- `nodeSelector` — labels a Node must have for this rule to apply.
- `selectorTerms` — a list of node selector terms, with exactly the shape
  and semantics of a Pod's
  `affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms`:
  the terms are ORed, and the requirements within one term are ANDed. Each
  term holds `matchExpressions` on Node labels (operators `In`, `NotIn`,
  `Exists`, `DoesNotExist`, `Gt`, `Lt`) and/or `matchFields` on
  `metadata.name` (operators `In`, `NotIn`, with exactly one value).
- `providerIDPattern` — a Go regular expression matched against a Node's
  `spec.providerID`. A Node with no `providerID` yet never matches a rule
  that sets this.

All three are optional; an unset one imposes no constraint. When a rule sets
several, a Node must satisfy every one of them to match it — e.g. a rule with
both `nodeSelector` and `selectorTerms` matches only Nodes that have the
`nodeSelector` labels *and* satisfy at least one term.

Selecting on a label that a `labels` rule itself applies is self-reinforcing:
applied labels are never removed automatically, so such a Node keeps
matching even after the original reason is gone.

The document is decoded strictly: unknown keys (a typo, or the retired
`policies` schema), `providerIDPattern`s that don't compile, and malformed
`selectorTerms` (an empty term, an unknown operator, `In` without values, a
non-integer `Gt`/`Lt` value, a `matchFields` key other than `metadata.name`)
make the
controller log an error and keep its previous configuration, rather than
silently applying nothing.

### externalIPs

Every `externalIPs` rule matching a Node contributes its `externalIPs` to
that Node's `status.addresses` as `ExternalIP` entries (existing addresses,
including ones added by other matching rules, are preserved — nothing is
ever removed automatically; if a rule is deleted or a Node stops matching,
previously applied `externalIPs` stay until removed by hand). Whenever the
controller touches a Node's `externalIPs`, it also normalizes their order so
IPv6 addresses always come before IPv4 ones (stable within each family) —
this applies to the full set on the Node, not just newly added addresses, so
a pre-existing IPv4-before-IPv6 ordering gets corrected too.

### labels / annotations

`labels` and `annotations` rules are authoritative: a rule's value is
written onto the Node even if the key already exists with a different value
(from another controller, or from `kubectl label`/`kubectl annotate`) — same
"nothing removed automatically" rule applies if a rule is later deleted or a
Node stops matching. If two matching rules (within the same map) disagree on
the value for the same key, neither value is applied and an error is
logged; fix the conflicting rules to resolve it. Because `labels`/
`annotations` can overwrite anything, including labels other controllers or
the scheduler rely on, avoid targeting reserved prefixes (`kubernetes.io/`,
`node-role.kubernetes.io/`, etc.) unless you mean to.

The three rule maps are reconciled by independent controllers that don't
coordinate with each other, so this conflict detection only applies
*within* a single map (two `labels` rules, or two `annotations` rules) —
not across maps.

### Conditions

Each of the three controllers maintains its own NodeCondition, present on a
Node **only while at least one rule for that domain matches it** — a Node no
`annotations` rule ever matches carries no `kubeling.io/Annotated` condition
at all:

| Domain | Condition type |
| --- | --- |
| `externalIPs` | `kubeling.io/ExternalIPsApplied` |
| `labels` | `kubeling.io/Labeled` |
| `annotations` | `kubeling.io/Annotated` |

While a rule matches, the condition reflects live progress:

- `Status: "False", Reason: "Pending"` — a rule matches but its values
  haven't been fully applied to the Node yet.
- `Status: "True", Reason: "Applied"` — a rule matches and its values are
  present on the Node.

If a Node stops matching any rule for a domain (the rule is edited, removed,
the whole map or ConfigMap is removed, or the Node's labels/providerID
change), that domain's condition is removed
— even though, per the "nothing removed automatically" rule above, any
labels/annotations/externalIPs it already applied are left in place. The
condition tracks current applicability; the values it caused are permanent.

Editing the ConfigMap re-evaluates every Node against the current rules, so
a newly added or widened rule can still match previously-unmatched Nodes.

## Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--kubeconfig` | in-cluster config | Path to a kubeconfig; omit when running inside the cluster. |
| `--provider-id` | `custom` | Scheme used for `providerID = <provider-id>://<node-name>`. |
| `--leader-elect` | `true` | Enable leader election across replicas. |
| `--leader-elect-namespace` | `kube-system` | Namespace holding the leader-election Lease. |
| `--leader-elect-lease-name` | `kubeling` | Name of the leader-election Lease. |
| `--resync-period` | `10m` | Node and ConfigMap informer resync period. |
| `--workers` | `2` | Number of concurrent Node reconcile workers per controller. |
| `--health-addr` | `:10258` | Address serving `/healthz`. |
| `--configmap` | `""` (or `CONFIGMAP` env var) | Rule ConfigMap reference, `(namespace/)name`. Empty disables rule processing. |

## Development

Requires Go (the version in `go.mod`), Helm and, for images, Docker with
buildx.

```sh
make help     # list targets
make check    # everything CI runs: vet, lint, test, helm-lint, helm-test
make test     # go test -race -cover ./...
make lint     # golangci-lint, installed at a pinned version into bin/
```

CI runs `make check` on every push to `main` and on pull requests via
GitHub Actions ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) and
Forgejo Actions ([`.forgejo/workflows/ci.yml`](.forgejo/workflows/ci.yml)).
