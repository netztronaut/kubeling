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

Optionally, it can also apply `externalIPs` to Nodes based on policies read
from a ConfigMap — see [Policy configuration](#policy-configuration) — and
label Nodes matching a `providerID` pattern, reflecting the outcome in a
custom NodeCondition — see [Node onboarding](#node-onboarding).

## Building the image

```sh
docker build -t ghcr.io/steigr/kubeling:latest .
docker push ghcr.io/steigr/kubeling:latest
```

Or build the binary locally:

```sh
go build -o bin/kubeling ./cmd/kubeling
```

## Deploying

### Helm

A chart is available at [`charts/kubeling`](charts/kubeling):

```sh
helm install kubeling ./charts/kubeling \
  --namespace kube-system
```

See the [chart README](charts/kubeling/README.md) for the full values reference.

### Plain manifests

Manifests are in [`deploy/`](deploy/):

- `service-account.yaml` — the `kubeling` ServiceAccount in `kube-system`.
- `clusterrole.yaml` — cluster-wide permission to read/update Nodes, Node status, ConfigMaps, and emit Events.
- `role.yaml` — namespaced permission to manage the leader-election `Lease` in `kube-system`.
- `deployment.yaml` — a 2-replica Deployment with leader election enabled.

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

## Policy configuration

The controller can optionally read policies from a ConfigMap and apply them
to Nodes. Point it at one with `--configmap=(namespace/)name` or the
`CONFIGMAP` environment variable (the flag wins if both are set); the
namespace defaults to the controller's own namespace when omitted. The
ConfigMap is read via the Kubernetes API (get/list/watch) — it is never
mounted as a volume — so changes take effect within seconds, without a pod
restart. Leave it unset to disable policy processing entirely.

The ConfigMap must have a `config.yaml` key holding a YAML document with a
`policies` map at the root, keyed by an arbitrary policy ID:

```yaml
policies:
  example:
    nodeSelector:
      topology.kubernetes.io/zone: eu-central-1a
    externalIPs:
      - 203.0.113.10
    labels:
      environment: production
    annotations:
      example.com/rack: r42
```

- `nodeSelector` — labels a Node must have for this policy to apply.
- `externalIPs` — optional list of IPs to ensure are present on matching
  Nodes' `status.addresses` as `ExternalIP` entries.
- `labels` / `annotations` — optional key/value pairs to set on matching
  Nodes' metadata.

Every policy whose `nodeSelector` matches a Node contributes its
`externalIPs` to that Node (existing addresses, including ones added by
other matching policies, are preserved; nothing is ever removed
automatically — if a policy is deleted or a Node stops matching, previously
applied `externalIPs` stay until removed by hand). Whenever the controller
touches a Node's `externalIPs`, it also normalizes their order so IPv6
addresses always come before IPv4 ones (stable within each family) — this
applies to the full set on the Node, not just newly added addresses, so a
pre-existing IPv4-before-IPv6 ordering gets corrected too.

`labels` and `annotations` are authoritative: a policy's value is written
onto the Node even if the key already exists with a different value (from
another controller, or from `kubectl label`/`kubectl annotate`) — same
"nothing removed automatically" rule applies if a policy is later deleted
or a Node stops matching. If two policies both match the same Node and
disagree on the value for the same key, neither value is applied and an
error is logged; fix the conflicting policies to resolve it. Because
`labels`/`annotations` can overwrite anything, including labels other
controllers or the scheduler rely on, avoid targeting reserved prefixes
(`kubernetes.io/`, `node-role.kubernetes.io/`, etc.) unless you mean to.

## Node onboarding

The same ConfigMap used for [policies](#policy-configuration) can also carry
an `onboarding` map, keyed by an arbitrary rule ID:

```yaml
onboarding:
  edge:
    providerIDPattern: '^custom://edge-'
    label: kubeling.io/onboarded
    labelValue: "true"
```

- `providerIDPattern` — a Go regular expression matched against a Node's
  `spec.providerID`. A Node with no `providerID` yet never matches.
- `label` / `labelValue` — the label applied, once, to a Node whose
  `providerID` matches.

Every Node the controller sees gets a `kubeling.io/Onboarded` NodeCondition
reflecting the outcome:

- `Status: "False", Reason: "Pending"` — no onboarding rule matches this
  Node's `providerID` yet.
- `Status: "True", Reason: "Onboarded"` — a rule matched and its label has
  been applied.

If multiple rules could match the same Node, the one with the
lexicographically first rule ID wins.

Onboarding is a **one-time stamp**, not a continuously-enforced policy: once
a Node is `Onboarded`, its label and condition are never re-evaluated,
changed, or removed — not even if the matching rule is later edited or
removed from the ConfigMap (same "nothing removed automatically" rule
[policies](#policy-configuration) follow for `externalIPs`/`labels`).
Editing the ConfigMap does cause every Node to be re-evaluated against the
current rules, so a newly added or widened rule can still onboard
previously-unmatched Nodes.

Choose a `label` key outside anything a policy in the same ConfigMap also
manages — the policy and onboarding mechanisms don't coordinate with each
other, so both writing the same key can fight over its value.

## Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--kubeconfig` | in-cluster config | Path to a kubeconfig; omit when running inside the cluster. |
| `--provider-id` | `custom` | Scheme used for `providerID = <provider-id>://<node-name>`. |
| `--leader-elect` | `true` | Enable leader election across replicas. |
| `--leader-elect-namespace` | `kube-system` | Namespace holding the leader-election Lease. |
| `--leader-elect-lease-name` | `kubeling` | Name of the leader-election Lease. |
| `--resync-period` | `10m` | Node and ConfigMap informer resync period. |
| `--workers` | `2` | Number of concurrent Node reconcile workers. |
| `--health-addr` | `:10258` | Address serving `/healthz`. |
| `--configmap` | `""` (or `CONFIGMAP` env var) | Policy ConfigMap reference, `(namespace/)name`. Empty disables policy processing. |

## Development

```sh
go build ./...
go vet ./...
```
