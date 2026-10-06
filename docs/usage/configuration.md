# Configuration

The `envoy-gateway` extension is enabled per shoot by adding an
`Extension` entry to `spec.extensions`. The extension is restricted to shoots
with `purpose: evaluation` during the GEP-68 incubation phase.

> The cluster-admin side of the deployment (registering the extension on the
> landscape so shoot owners can enable it) is documented separately in
> [deployment.md](deployment.md).


```yaml
apiVersion: core.gardener.cloud/v1beta1
kind: Shoot
spec:
  purpose: evaluation
  extensions:
    - type: envoy-gateway
      providerConfig:
        apiVersion: envoy-gateway.extensions.gardener.cloud/v1alpha1
        kind: EnvoyGatewayConfig
        controlPlane:
          replicas: 2
          logLevel: info
        dataPlane:
          replicas: 2
          logLevel: info
        channel: standard
        manageCRDs: true
        manageDataPlaneNetworkPolicies: false
        envoyProxyDefaults:
          accessLogging: true
          resources:
            requests:
              cpu: 100m
              memory: 256Mi
```

The `EnvoyGatewayConfig` has no `spec`/`status` wrapper — the configuration
fields live at the top level, following the convention of other Gardener
extension `providerConfig` APIs.

## Fields

### `controlPlane`

Envoy Gateway control-plane settings.

- `replicas` (int) — number of control-plane replicas. Default: `2`.
- `logLevel` (string) — control-plane log level. One of `debug`, `info`,
  `warn`, `error`. Default: `info`.

### `dataPlane`

Envoy data-plane (proxy) settings, applied per `Gateway`.

- `replicas` (int) — number of proxy replicas per `Gateway`. Default: `2`.
- `logLevel` (string) — data-plane log level. One of `debug`, `info`, `warn`,
  `error`. Default: `info`.

### `channel`

The Gateway API CRD channel to install. One of `standard` or `experimental`.
Default: `standard`.

Selecting `experimental` additionally installs the experimental-channel
Gateway API CRDs (`TCPRoute`, `TLSRoute`, `UDPRoute`, `BackendTLSPolicy`).
Experimental APIs may change in backwards-incompatible ways across Gateway
API releases. Operators that enable this option should track the upstream
release notes carefully.

### `manageCRDs`

When `true` (default), the extension installs and updates the Gateway API and
Envoy Gateway CRDs. Set to `false` if the CRDs are owned externally (for
example, managed by another controller or shipped via static manifests), in
which case the extension leaves them alone.

### `envoyProxyDefaults`

Opinionated defaults applied to every `Gateway` via an `EnvoyProxy` template
reference. Currently exposes:

- `accessLogging` (bool) — enables access-log emission on the data-plane
  Envoy proxies.
- `resources` (`corev1.ResourceRequirements`) — compute resources applied to
  every data-plane Envoy proxy container.

### `manageDataPlaneNetworkPolicies`

When `true`, the extension manages the `NetworkPolicy` objects for **all three
hops** a request takes through a `Gateway`, so that `Gateway` traffic flows on a
shoot that enforces a default-deny `NetworkPolicy` posture. Defaults to `false`.

The policies are reconciled by a small per-shoot controller the extension runs
in the shoot's control-plane namespace **on the seed** (not inside the shoot).
It watches the shoot's `Gateway` and route objects through the shoot-access
token and reconciles the policies **on change** rather than on the extension's
periodic poll, so a newly created `Gateway` gets its policies promptly. Running
the controller on the seed keeps the "write `NetworkPolicy` into any namespace"
privilege out of reach of shoot nodes and tenant workloads.

Three ports are opened because they serve different callers on the ingress hop:

- `10080` / `10443` — the HTTP and HTTPS listeners that carry user traffic.
  Envoy Gateway shifts the Service's `:80`/`:443` down to these target ports
  because the proxies run non-root and cannot bind privileged ports.
- `19003` — the proxy's readiness endpoint (`/readyz`), probed by the kubelet
  and, in IP-target LB modes (e.g. GKE NEG, AWS NLB `target-type: ip`), by the
  load balancer directly. Opening it keeps the proxies visible as ready
  endpoints regardless of how the cloud provider wires the LB health check.

The extension writes these policies directly to the shoot and owns their full
lifecycle: the proxy ingress policy is created when a namespace gains its first
`Gateway` and removed once the namespace no longer holds one, or when the
feature is disabled. The proxy-egress and backend-ingress policies (see below)
are created and pruned as routes and their backends change.

Enable this only on shoots whose default-deny `NetworkPolicy` posture would
otherwise block `Gateway` traffic.

The ingress rule allows traffic from anywhere (`from` is left empty) rather than
being scoped to the load balancer. The extension reconciles on the `Extension`
resource and does not know a `Gateway`'s load-balancer scope — whether it is
internet-facing or internal is a user-set Service annotation that varies per
cloud provider and can change after the policy is written. A proxy that is only
reachable through an internal load balancer is already unreachable from the
internet at the network layer, so the wide `from` grants no additional exposure;
narrowing it would add provider-specific Service bookkeeping without closing a
reachable path.

> **Scope — all three hops.** A request to a `Gateway` traverses three hops, and
> on a default-deny namespace each must be allowed independently. With
> `manageDataPlaneNetworkPolicies: true` the extension manages all three:
>
> | Hop | Policy | How it is derived |
> |-----|--------|-------------------|
> | client → Envoy proxy (proxy ingress) | ingress policy in the `Gateway` namespace | selects the proxy pods; allows ingress from anywhere on the data-plane ports |
> | Envoy proxy → your backend (proxy egress) | egress policy in the `Gateway` namespace | selects the proxy pods; allows egress to DNS and to each route's backend pods |
> | your backend ← Envoy proxy (backend ingress) | ingress policy in the backend namespace | selects the backend pods; allows ingress from the proxy pods |
>
> The proxy-egress and backend-ingress hops are derived from the shoot's route
> objects (`HTTPRoute`, `GRPCRoute`, and the experimental `TCPRoute`/`TLSRoute`/
> `UDPRoute` when that channel is enabled). For each route `backendRef`, the
> controller resolves the referenced `Service` and copies its `spec.selector`
> into the policy's `podSelector`, so the egress and ingress rules target exactly
> the pods behind that `Service`. Cross-namespace `backendRef`s are honored only
> when a Gateway API `ReferenceGrant` in the backend namespace permits the
> reference — without the grant, no backend policy is written for that reference.
>
> **Limitation — not every backend can be derived automatically.** The controller
> can only build the proxy-egress and backend-ingress policies for backends it can
> resolve to a concrete set of pods via a `Service` selector. It **skips (and
> logs)**:
>
> - `Service`s **without a `spec.selector`** — headless `Service`s with manually
>   managed `Endpoints`/`EndpointSlice`s, and any other selector-less `Service`.
> - **`ExternalName`** `Service`s (they have no pods in the cluster).
> - `backendRef`s that point at something **other than a `Service`** (e.g. a
>   custom backend resource).
>
> For those backends you must write the proxy-egress and backend-ingress
> `NetworkPolicy` objects yourself. The proxy-egress policy must reproduce the
> three egress rules the managed policy would have written — **DNS**, the **xDS
> control plane**, and your **backend** — because a hand-written policy replaces,
> rather than adds to, the proxy's egress allowance. Omitting the DNS or xDS
> rules will cut the proxy off from name resolution or its control plane (the
> latter manifests as the proxy never becoming `Ready`). For example:
>
> ```yaml
> # 1. Allow the proxies to egress to DNS, the xDS control plane, and your backend.
> apiVersion: networking.k8s.io/v1
> kind: NetworkPolicy
> metadata:
>   name: allow-proxy-egress
>   namespace: <gateway-namespace>
> spec:
>   podSelector:
>     matchLabels:
>       app.kubernetes.io/managed-by: envoy-gateway
>       app.kubernetes.io/name: envoy
>   policyTypes: [Egress]
>   egress:
>     # DNS resolution (kube-dns / node-local DNS).
>     - to:
>         - namespaceSelector: {}
>       ports:
>         - { protocol: UDP, port: 53 }
>         - { protocol: TCP, port: 53 }
>     # xDS: the proxy pulls its config from the envoy-gateway control plane in
>     # kube-system. Without this the proxy never programs a listener and stays
>     # NotReady. Do not omit this rule.
>     - to:
>         - namespaceSelector:
>             matchLabels:
>               kubernetes.io/metadata.name: kube-system
>           podSelector:
>             matchLabels:
>               app.kubernetes.io/name: envoy-gateway
>               app.kubernetes.io/instance: envoy-gateway
>       ports:
>         - { protocol: TCP, port: 18000 }
>     # Your backend pods. Add a namespaceSelector too if the backend is in a
>     # different namespace than the Gateway.
>     - to:
>         - podSelector:
>             matchLabels:
>               app: <your-backend>
> ---
> # 2. Allow your backend pods to accept ingress from the proxies.
> apiVersion: networking.k8s.io/v1
> kind: NetworkPolicy
> metadata:
>   name: allow-backend-ingress
>   namespace: <backend-namespace>
> spec:
>   podSelector:
>     matchLabels:
>       app: <your-backend>
>   policyTypes: [Ingress]
>   ingress:
>     - from:
>         - podSelector:
>             matchLabels:
>               app.kubernetes.io/managed-by: envoy-gateway
>               app.kubernetes.io/name: envoy
> ```

## How the data-plane NetworkPolicy controller works

> This section explains the controller behind
> [`manageDataPlaneNetworkPolicies`](#managedataplanenetworkpolicies). It is
> background for operators; nothing here is configurable.

**Where it runs.** The controller is a single-replica `Deployment` the extension
creates **per shoot, on the seed**, in that shoot's control-plane namespace
(`shoot--<project>--<name>`). It is not deployed into the shoot. It runs the
extension's own image under the `netpol-controller` subcommand — the same image
as the extension pod, not the upstream Envoy Gateway image.

**Two cluster connections.** The controller's manager holds two connections:

- the **seed** (its own in-cluster config), used only for its leader-election
  lease; and
- the **shoot** API server, built from the generic kubeconfig the token-requestor
  projects into the pod. That kubeconfig uses a `tokenFile` whose token is
  rotated and reloaded without a restart, so the controller reads `Gateway`s and
  routes and writes `NetworkPolicy` objects into the shoot through a scoped
  shoot-access token.

Running on the seed keeps the "write a `NetworkPolicy` into any shoot namespace"
privilege out of reach of shoot nodes and tenant workloads — a node compromise
cannot reach the controller's credentials.

**Event-driven, keyed by Gateway namespace.** Rather than polling, the controller
watches the shoot and reconciles **on change**. Each watch maps the changed
object back to the affected `Gateway` namespace(s), and a reconcile recomputes
the full policy set for that one namespace. It watches:

| Watched object | Why |
|----------------|-----|
| `Gateway` | the unit of reconciliation; its namespace is the key |
| `HTTPRoute`, `GRPCRoute` (and `TCPRoute`/`TLSRoute`/`UDPRoute` when the experimental channel is on) | their `backendRef`s determine hops 2 and 3 |
| `Service` | its `spec.selector` is copied into the backend `podSelector`; a selector change must re-derive the policy |
| `ReferenceGrant` | gates whether a cross-namespace backendRef is honored |
| `NetworkPolicy` (the ones it manages) | **self-heal** — if someone edits or deletes a managed policy, the change re-enqueues the namespace and the controller restores it |

**What a reconcile writes.** For a Gateway namespace, the controller derives the
three hops described [above](#managedataplanenetworkpolicies) from the routes it
finds, then creates/updates and prunes:

- **hop 1** — proxy-ingress policy in the Gateway namespace;
- **hop 2** — proxy-egress policy in the Gateway namespace, always carrying the
  DNS and xDS rules plus one rule per resolvable backend;
- **hop 3** — one backend-ingress policy per backend namespace.

Backends it cannot resolve to pods are skipped and logged (see
[the limitation above](#managedataplanenetworkpolicies) and the
[troubleshooting entry](./troubleshooting.md#backend-unreachable-on-a-default-deny-shoot-proxy-is-ready-traffic-times-out)).
Policies for backends a route no longer references are pruned on the next
reconcile, so the set stays precise even when many Gateway namespaces share a
backend namespace.

**Lifecycle.** The extension manages the controller's `Deployment` alongside the
rest of the shoot's envoy-gateway resources: it is created when the feature is
enabled, **scaled to zero** while the shoot is hibernated (the shoot-access
secret is kept so it resumes on wake-up), and torn down when the feature is
disabled or the shoot is deleted.

## What gets installed in the shoot

When the extension reconciles a shoot, it creates a `ManagedResource` in the
shoot's control-plane namespace on the seed. The contents of the
`ManagedResource` are applied into the shoot cluster:

1. ServiceAccount, ClusterRole, ClusterRoleBinding, leader-election Role and
   RoleBinding for the Envoy Gateway controller (in `kube-system`).
2. The Envoy Gateway control-plane `Deployment` and `Service` (in
   `kube-system`).
3. A `PodDisruptionBudget` and `NetworkPolicy` objects for the control plane.
4. A single `GatewayClass` named `gardener-envoy-gateway` bound to the
   controller `gateway.envoyproxy.io/gatewayclass-controller`, plus a default
   `EnvoyProxy` template it references.
5. Standard-channel Gateway API CRDs (and the experimental channel when
   `channel: experimental`), unless `manageCRDs: false`.
6. Envoy Gateway's own CRDs (`EnvoyProxy`, `BackendTrafficPolicy`,
   `ClientTrafficPolicy`, `SecurityPolicy`, etc.) which power
   implementation-specific configuration, unless `manageCRDs: false`.

The data-plane ingress `NetworkPolicy` objects are **not** part of the
`ManagedResource`; when `manageDataPlaneNetworkPolicies` is enabled the extension
writes them directly into the `Gateway` namespaces (see
[`manageDataPlaneNetworkPolicies`](#managedataplanenetworkpolicies) above).

The Envoy Gateway control plane spawns one Envoy data-plane Deployment +
LoadBalancer Service per user-created `Gateway`, **in the `Gateway`'s own
namespace**. The cloud-provider load-balancer controller provisions the
underlying LB for each Service.

> [!NOTE]
> The data plane runs in the `Gateway`'s namespace because the extension pins
> Envoy Gateway to the `GatewayNamespace` deploy mode
> (`provider.kubernetes.deploy.type`). Earlier extension versions used the
> upstream default (`ControllerNamespace`), which placed every data-plane proxy
> and its LoadBalancer Service in `kube-system` using the control plane's
> privileged ServiceAccount — a confused-deputy escalation. See
> [Migration: data plane moved out of `kube-system`](#migration-data-plane-moved-out-of-kube-system)
> for what this means when upgrading an existing cluster.

## Migration: data plane moved out of `kube-system`

Extensions upgraded across the `GatewayNamespace` switch move each `Gateway`'s
data-plane proxy `Deployment` and its LoadBalancer `Service` from `kube-system`
into the `Gateway`'s own namespace. Envoy Gateway **recreates** the bundle in the
new namespace but does **not** garbage-collect the old copies it left in
`kube-system` — they were created directly by the controller (not via the
extension's `ManagedResource`), so nothing owns them anymore. Left alone this
would leave, per `Gateway`:

- an idle proxy `Deployment` (dead pods), and
- an orphaned `Service` of type `LoadBalancer` — which keeps a **cloud load
  balancer provisioned and billing** even though no traffic reaches it.

**The extension cleans this up automatically.** On every reconcile, right after
deploying the control plane, it sweeps `kube-system` for objects that carry
Envoy Gateway's `app.kubernetes.io/managed-by=envoy-gateway` label **and** a
`gateway.envoyproxy.io/owning-gateway-name` label (i.e. per-`Gateway` data-plane
infra: `Service`, `Deployment`, `ServiceAccount`, `ConfigMap`, and any
`PodDisruptionBudget`/`HorizontalPodAutoscaler`) and deletes them. The
control-plane objects the extension installs in `kube-system` carry **no**
`owning-gateway-*` label, so the sweep never touches them. Deleting the orphaned
`LoadBalancer` Service triggers the cloud-controller-manager to deprovision its
LB, reclaiming the cost.

The sweep is idempotent and best-effort: once the orphans are gone it is a no-op,
it runs on the next reconcile after the new-namespace bundle exists, and a
transient failure is logged and retried on the following reconcile rather than
blocking reconciliation. No operator action is required — the cleanup happens the
next time each shoot reconciles under the new version. To confirm it has run, see
[Duplicate LoadBalancer / orphaned proxies in `kube-system` after upgrade](./troubleshooting.md#duplicate-loadbalancer--orphaned-proxies-in-kube-system-after-upgrade)
in the troubleshooting guide.

## Validation

The admission webhook validates the following on every Shoot create/update:

1. `spec.purpose` must be `evaluation` when the extension is enabled.
2. `providerConfig`, if supplied, must decode strictly as an
   `EnvoyGatewayConfig` (unknown fields are rejected).
3. `controlPlane.logLevel` and `dataPlane.logLevel`, if set, must be one of
   `debug`, `info`, `warn`, `error`.
4. `channel`, if set, must be one of `standard`, `experimental`.
