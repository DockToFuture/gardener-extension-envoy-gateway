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

When `true`, the extension reconciles an ingress `NetworkPolicy` named
`envoy-gateway-proxies` into every shoot namespace that holds a `Gateway`. The
policy selects the data-plane proxy pods
(`app.kubernetes.io/managed-by: envoy-gateway`, `app.kubernetes.io/name: envoy`)
and allows ingress **from anywhere** on the data-plane ports (`10080`, `19003`),
so external traffic reaching the `Gateway`'s load balancer can be delivered to
the proxies on a cluster that enforces a default-deny `NetworkPolicy` posture.
Defaults to `false`.

The extension writes these policies directly to the shoot on every reconcile
(they cannot travel through the shoot `ManagedResource`, because
gardener-resource-manager only reconciles a fixed set of namespaces and cannot
reach arbitrary `Gateway` namespaces). It owns their full lifecycle: a policy is
created when a namespace gains its first `Gateway` and removed once the namespace
no longer holds one, or when the feature is disabled.

Only enable this on shoots whose network plugin enforces `NetworkPolicy` objects
and where the default-deny posture would otherwise block `Gateway` traffic.

> **Scope — this covers only the *ingress* hop to the proxies.** A request to a
> `Gateway` traverses three hops, and on a default-deny namespace each must be
> allowed independently:
>
> | Hop | Allowed by |
> |-----|------------|
> | client → Envoy proxy (proxy ingress) | `manageDataPlaneNetworkPolicies` — **this feature** |
> | Envoy proxy → your backend (proxy egress) | **you** |
> | your backend ← Envoy proxy (backend ingress) | **you** |
>
> The extension deliberately does not manage the proxy→backend hops: it cannot
> know which pods are your legitimate upstreams, and opening egress from the
> proxies to arbitrary workloads would be an overreach. If you enable this
> feature on a default-deny namespace and see `upstream connect error or
> disconnect/reset before headers` from Envoy, external traffic *is* reaching the
> proxies — the missing piece is a `NetworkPolicy` pair for your own backends,
> for example:
>
> ```yaml
> # 1. Allow the proxies to egress to your backends (and DNS).
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
>     - {} # narrow to your backend + kube-dns in production
> ---
> # 2. Allow your backend pods to accept ingress from the proxies.
> apiVersion: networking.k8s.io/v1
> kind: NetworkPolicy
> metadata:
>   name: allow-backend-ingress
>   namespace: <gateway-namespace>
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
writes them directly into the `Gateway` namespaces (see that field above).

The Envoy Gateway control plane spawns one Envoy data-plane Deployment +
LoadBalancer Service per user-created `Gateway`, in `kube-system`. The
cloud-provider load-balancer controller provisions the underlying LB for each
Service.

## Validation

The admission webhook validates the following on every Shoot create/update:

1. `spec.purpose` must be `evaluation` when the extension is enabled.
2. `providerConfig`, if supplied, must decode strictly as an
   `EnvoyGatewayConfig` (unknown fields are rejected).
3. `controlPlane.logLevel` and `dataPlane.logLevel`, if set, must be one of
   `debug`, `info`, `warn`, `error`.
4. `channel`, if set, must be one of `standard`, `experimental`.
