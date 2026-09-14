# Kubeling

A Helm chart for [Kubeling](../../README.md), a minimal
cloud-controller-manager that lets kubelets run with
`--cloud-provider=external` without a real cloud backing the cluster.

## Installing

```sh
helm install kubeling ./charts/kubeling \
  --namespace kube-system
```

By default the chart schedules the controller on control-plane nodes
(`nodeSelector: node-role.kubernetes.io/control-plane`) with tolerations for
the control-plane and `node.cloudprovider.kubernetes.io/uninitialized`
taints, and `hostNetwork: true`. This avoids the chicken-and-egg problem
where the controller manager itself is a pod that needs a Node to be
initialized before it can be scheduled. If your control-plane nodes aren't
schedulable, or you run a managed control plane, override
`nodeSelector`/`tolerations` to target a fixed set of nodes you've
initialized out of band:

```sh
helm install kubeling ./charts/kubeling \
  --namespace kube-system \
  --set nodeSelector=null \
  --set-json 'tolerations=[]'
```

## Rule configuration

The controller can apply `externalIPs`, labels and annotations to Nodes
from rules it reads from a ConfigMap via the Kubernetes API (get/list/watch
— never mounted as a volume, so edits apply within seconds). The chart can
manage that ConfigMap for you via `config`, or point the controller at one
you manage yourself via `configMap`. With neither set, only providerID and
taint handling run. The rule semantics — matching, conflict handling and
the `kubeling.io/*` Node conditions — are described in the
[main README](../../README.md#rule-configuration).

### Chart-managed ConfigMap (`config`)

Set `config` to the rule document itself; it's rendered with `toYaml`
straight into the `config.yaml` key of a `<release>-config` ConfigMap the
chart creates, and the controller is pointed at it automatically:

```yaml
# values.yaml
config:
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

```sh
helm install kubeling ./charts/kubeling \
  --namespace kube-system \
  --values values.yaml
```

The controller rejects documents with unknown keys (and keeps its previous
configuration, logging an error), so a typo never silently disables a rule.
[`ci/rules-values.yaml`](ci/rules-values.yaml) is a complete example that
the repository's tests validate against the controller's schema.

### Externally-managed ConfigMap (`configMap`)

If you'd rather manage the ConfigMap yourself (e.g. via a separate
GitOps-synced manifest), leave `config` empty and set `configMap` to a
`"(namespace/)name"` reference instead; the namespace defaults to the
controller's own namespace when omitted. `configMap` is ignored whenever
`config` is non-empty, since the chart then manages its own ConfigMap.

```sh
helm install kubeling ./charts/kubeling \
  --namespace kube-system \
  --set configMap=kubeling-config
```

The RBAC rules for `nodes/status` and `configmaps` are created either way,
but nothing reads or writes through them while rule processing is off.

## Values

| Key | Default | Description |
| --- | --- | --- |
| `replicaCount` | `1` | Number of replicas. Only one is active at a time; see `leaderElection`. |
| `updateStrategy` | `RollingUpdate`, `maxSurge: 0`, `maxUnavailable: 1` | Deployment rollout strategy. Terminates a Pod before scheduling its replacement so rollouts don't deadlock on clusters where `nodeSelector` matches only as many nodes as there are replicas. |
| `image.repository` | `git.example.com/platform/kubeling` | Container image repository. |
| `image.tag` | `""` (chart `appVersion`) | Container image tag. |
| `image.pullPolicy` | `""` (Kubernetes default) | Image pull policy. Unset, Kubernetes pulls `latest` tags on every start and other tags only when missing. |
| `imagePullSecrets` | `[]` | Image pull secrets. |
| `providerID` | `custom` | Scheme used for the ProviderID stamped onto Nodes (`providerID = <providerID>://<node-name>`). |
| `workers` | `2` | Number of concurrent Node reconcile workers per controller. |
| `resyncPeriod` | `10m` | Node and ConfigMap informer resync period. |
| `leaderElection.enabled` | `true` | Enable leader election across replicas. |
| `leaderElection.namespace` | `kube-system` | Namespace holding the leader-election Lease; the Role/RoleBinding are created here. |
| `leaderElection.leaseName` | `kubeling` | Name of the leader-election Lease. |
| `extraArgs` | `[]` | Extra command-line arguments appended to the container. |
| `config` | `{}` | Rule document, rendered with `toYaml` into a chart-managed ConfigMap's `config.yaml` key. See [Rule configuration](#rule-configuration). Leave empty to not create a ConfigMap. |
| `configMap` | `""` | Reference to an existing ConfigMap with rule configuration, as `"(namespace/)name"`; namespace defaults to the controller's own namespace. Ignored when `config` is non-empty. |
| `healthz.port` | `10258` | Port serving `/healthz`, used by the liveness probe. |
| `rbac.create` | `true` | Create the ClusterRole/ClusterRoleBinding and Role/RoleBinding this chart needs. |
| `serviceAccount.create` | `true` | Create a ServiceAccount. |
| `serviceAccount.name` | `""` | ServiceAccount name; generated from the fullname template if unset. |
| `serviceAccount.annotations` | `{}` | Annotations for the ServiceAccount. |
| `serviceAccount.automount` | `true` | Automount the ServiceAccount token. |
| `podAnnotations` / `podLabels` | `{}` | Extra pod annotations/labels. |
| `podSecurityContext` | `{}` | Pod-level `securityContext`. |
| `securityContext` | non-root, no privilege escalation, read-only rootfs | Container-level `securityContext`. |
| `resources` | `50m`/`32Mi` requests, `128Mi` memory limit | Container resource requests/limits. |
| `hostNetwork` | `true` | Run the pod on the host network. |
| `priorityClassName` | `system-cluster-critical` | Pod priority class. |
| `nodeSelector` | control-plane nodes | Node selector for the Deployment. |
| `tolerations` | control-plane / uninitialized / not-ready | Tolerations for the Deployment. |
| `affinity` | `{}` | Affinity rules for the Deployment. |
| `nameOverride` / `fullnameOverride` | `""` | Override the generated chart/release name. |

## Testing

```sh
make helm-lint helm-test
```

`helm-test` runs [`hack/test-chart.sh`](../../hack/test-chart.sh), which
renders the chart with representative values and asserts on the output.
