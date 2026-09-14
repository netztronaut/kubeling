# Kubeling

A Helm chart for [Kubeling](../../README.md), a
minimal cloud-controller-manager that lets kubelets run with
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

## Policy configuration

The controller reads `externalIPs`/`nodeSelector` policies from a ConfigMap
via the Kubernetes API (get/list/watch — never mounted as a volume). The
chart can manage that ConfigMap for you via `config`, or point the
controller at one you manage yourself via `configMap`.

### Chart-managed ConfigMap (`config`)

Set `config` to the policy document itself; it's rendered with `toYaml`
straight into the `config.yaml` key of a `<release>-config` ConfigMap the
chart creates, and the controller is pointed at it automatically:

```yaml
# values.yaml
config:
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

```sh
helm install kubeling ./charts/kubeling \
  --namespace kube-system \
  -f values.yaml
```

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

### Behavior

Every policy whose `nodeSelector` matches a Node gets its `externalIPs`
merged into that Node's `status.addresses` as `ExternalIP` entries
(existing addresses, including ones from other matching policies, are kept;
nothing is ever removed automatically). IPv6 addresses always sort before
IPv4 ones within that set. `labels`/`annotations` are
authoritative — a policy's value overwrites whatever is already on the
Node, even from another controller — with one safety valve: if two
matching policies disagree on the value for the same key, that key is
skipped (and an error logged) on both sides rather than fought over. When
neither `config` nor `configMap` is set, the `nodes/status` and
`configmaps` RBAC rules are still created, but nothing reads or writes
through them.

## Values

| Key | Default | Description |
| --- | --- | --- |
| `replicaCount` | `2` | Number of replicas. Only one is active at a time; see `leaderElection`. |
| `updateStrategy` | `RollingUpdate`, `maxSurge: 0`, `maxUnavailable: 1` | Deployment rollout strategy. Terminates a Pod before scheduling its replacement so rollouts don't deadlock on clusters where `nodeSelector` matches only as many nodes as there are replicas. |
| `image.repository` | `ghcr.io/steigr/kubeling` | Container image repository. |
| `image.tag` | `""` (chart `appVersion`) | Container image tag. |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `imagePullSecrets` | `[]` | Image pull secrets. |
| `providerID` | `custom` | Scheme used for the ProviderID stamped onto Nodes (`providerID = <providerID>://<node-name>`). |
| `workers` | `2` | Number of concurrent Node reconcile workers. |
| `resyncPeriod` | `10m` | Node informer resync period. |
| `leaderElection.enabled` | `true` | Enable leader election across replicas. |
| `leaderElection.namespace` | `kube-system` | Namespace holding the leader-election Lease; the Role/RoleBinding are created here. |
| `leaderElection.leaseName` | `kubeling` | Name of the leader-election Lease. |
| `extraArgs` | `[]` | Extra command-line arguments appended to the container. |
| `config` | `{}` | Policy document, rendered with `toYaml` into a chart-managed ConfigMap's `config.yaml` key. See [Policy configuration](#policy-configuration). Leave empty to not create a ConfigMap. |
| `configMap` | `""` | Reference to an existing ConfigMap with policy configuration, as `"(namespace/)name"`; namespace defaults to the controller's own namespace. Ignored when `config` is non-empty. |
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
