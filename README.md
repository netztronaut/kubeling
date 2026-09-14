# Kubeling

A minimal [cloud-controller-manager](https://kubernetes.io/docs/concepts/architecture/cloud-controller/)
for clusters that have no real cloud backing them. It exists so kubelets can
be run with `--cloud-provider=external` (required on modern Kubernetes,
where in-tree cloud providers are gone) without needing a real cloud API to
talk to.

## What it does

When a kubelet starts with `--cloud-provider=external` it:

1. Registers its Node with the taint `node.cloudprovider.kubernetes.io/uninitialized:NoSchedule`.
2. Leaves `Node.spec.providerID` empty.

A cloud-controller-manager is expected to initialize the Node and remove
that taint so normal pods can be scheduled. This controller does exactly
that and nothing else:

- Watches Nodes.
- Sets `Node.spec.providerID` to `custom://<node-name>` if it isn't already set.
- Removes the `node.cloudprovider.kubernetes.io/uninitialized` taint.

The provider ID scheme is `custom` (configurable via `--provider-id`). There
is no real instance metadata, zone/region, or load balancer support — this
is intentionally the smallest thing that satisfies the external
cloud-provider contract. It's meant for bare-metal, on-prem, or dev clusters
where `--cloud-provider=external` is required by the kubelet/control plane
but there's no cloud to integrate with.

Optionally, it can also apply `externalIPs`, labels and annotations to Nodes
based on rules read from a ConfigMap, each matched by `nodeSelector` and/or
`providerID` regex — see [Rule configuration](#rule-configuration).

## Building

The source lives at
[git.example.com/platform/kubeling](https://git.example.com/platform/kubeling).
Build and push the multi-arch (`linux/amd64`, `linux/arm64`) image:

```sh
make image                                  # git.example.com/platform/kubeling:latest
make image IMAGE_REPOSITORY=registry.example.com/kubeling IMAGE_TAG=v0.2.0
```

Or build the binary locally with `make build` (writes `bin/kubeling`).

## Deploying

### Helm

A chart is available at [`charts/kubeling`](charts/kubeling):

```sh
helm install kubeling ./charts/kubeling \
  --namespace kube-system
```

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

Both the chart and the plain manifests run the Deployment on control-plane
nodes by default (`nodeSelector: node-role.kubernetes.io/control-plane`)
with tolerations for the control-plane and
`node.cloudprovider.kubernetes.io/uninitialized` taints, plus
`hostNetwork: true`. This avoids the chicken-and-egg problem where the
controller manager itself is a pod that needs a Node to be initialized
before it can be scheduled. If your control-plane nodes aren't schedulable,
or you run a managed control plane where you don't control those nodes,
adjust `nodeSelector`/`tolerations` to fit your setup — for example
scheduling it on a fixed set of worker nodes that you have initialized out
of band.

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
- `providerIDPattern` — a Go regular expression matched against a Node's
  `spec.providerID`. A Node with no `providerID` yet never matches a rule
  that sets this.

Both are optional; an unset one imposes no constraint. When a rule sets
both, a Node must satisfy both to match it.

The document is decoded strictly: unknown keys (a typo, or the retired
`policies` schema) and `providerIDPattern`s that don't compile make the
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
Forgejo Actions ([`.forgejo/workflows/ci.yml`](.forgejo/workflows/ci.yml)).
