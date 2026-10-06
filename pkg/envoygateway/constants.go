// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

// Package envoygateway provides resources for deploying the Envoy Gateway
// control plane and the Gateway API CRDs to shoot clusters.
package envoygateway

const (
	// Namespace is the namespace where the Envoy Gateway control plane is
	// deployed in the shoot. It must be a namespace watched by the shoot's
	// gardener-resource-manager so the ManagedResource can be applied.
	Namespace = "kube-system"

	// DeploymentName is the name of the Envoy Gateway control-plane Deployment.
	DeploymentName = "envoy-gateway"

	// ServiceAccountName is the name of the Envoy Gateway control-plane ServiceAccount.
	ServiceAccountName = "envoy-gateway"

	// ConfigMapName is the name of the ConfigMap holding the EnvoyGateway server config.
	ConfigMapName = "envoy-gateway-config"
	// ConfigFileName is the file key within the ConfigMap that envoy-gateway reads.
	ConfigFileName = "envoy-gateway.yaml"
	// ConfigMountPath is the directory where the config ConfigMap is mounted in the pod.
	ConfigMountPath = "/config"

	// ManagedResourceName is the name of the shoot-class ManagedResource that
	// delivers all Envoy Gateway resources into the shoot cluster.
	ManagedResourceName = "extension-envoy-gateway"

	// ImageName is the key for the Envoy Gateway controller image in imagevector.
	ImageName = "envoy-gateway"

	// ExtensionImageName is the image-vector key for this extension's own image.
	// The netpol-controller subcommand lives in this image (not in the upstream
	// Envoy Gateway image keyed by ImageName), so the seed-side netpol-controller
	// Deployment must run it. The chart overwrites this entry with the extension's
	// actual image at deploy time.
	ExtensionImageName = "gardener-extension-envoy-gateway"

	// GatewayClassName is the GatewayClass installed by this extension. Users
	// reference it from their Gateway resources via spec.gatewayClassName.
	GatewayClassName = "gardener-envoy-gateway"

	// EnvoyProxyDefaultsName is the cluster-default EnvoyProxy CR that our
	// GatewayClass references via parametersRef. It carries the pod labels
	// the data-plane Envoy pods need to satisfy Gardener's kube-system
	// NetworkPolicies.
	EnvoyProxyDefaultsName = "envoy-gateway-defaults"

	// GatewayClassControllerName is the controller name that Envoy Gateway
	// reconciles. It is configured on the GatewayClass and matches the
	// controller name Envoy Gateway advertises.
	GatewayClassControllerName = "gateway.envoyproxy.io/gatewayclass-controller"

	// DeployModeGatewayNamespace is the Envoy Gateway provider.kubernetes.deploy.type
	// value that places each Gateway's data-plane proxy Deployment, ServiceAccount,
	// and supporting resources in the Gateway's own namespace instead of the
	// controller namespace. Hardcoded as a security control to prevent a
	// confused-deputy escalation into kube-system.
	DeployModeGatewayNamespace = "GatewayNamespace"

	// EnvoyProxyGuardPolicyName is the name of the ValidatingAdmissionPolicy (and
	// its binding) that rejects dangerous EnvoyProxy fields (patch, initContainers,
	// arbitrary volumes/volumeMounts/securityContext) at admission.
	EnvoyProxyGuardPolicyName = "envoyproxy-deploy-guard.gardener.cloud"

	// DataPlaneNetworkPolicyName is the name of the per-Gateway-namespace
	// data-plane ingress NetworkPolicy the extension reconciles directly into the
	// shoot when ManageDataPlaneNetworkPolicies is enabled. It doubles as the
	// managed-policy label value used to select these policies for pruning. This
	// is the hop-1 (client → proxy ingress) policy.
	DataPlaneNetworkPolicyName = "envoy-gateway-proxies"

	// ProxyEgressNetworkPolicyName is the name of the per-Gateway-namespace
	// hop-2 (proxy → backend egress) NetworkPolicy. It selects the data-plane
	// proxy pods and allows egress to DNS plus every resolved route backend.
	ProxyEgressNetworkPolicyName = "envoy-gateway-proxies-egress"

	// BackendIngressNetworkPolicyPrefix is the name prefix of the per-backend
	// hop-3 (backend ← proxy ingress) NetworkPolicies written into backend
	// namespaces. The full name appends a short hash of the Gateway namespace so
	// multiple Gateways targeting one backend namespace do not collide.
	BackendIngressNetworkPolicyPrefix = "envoy-gateway-backend"

	// LabelHop is the label key that records which of the three data-plane
	// network hops a managed NetworkPolicy belongs to. It scopes the prune List
	// so each hop's reconciliation only removes its own stale policies.
	LabelHop = "envoy-gateway.extensions.gardener.cloud/hop"
	// HopProxyIngress, HopProxyEgress, and HopBackendIngress are the [LabelHop]
	// values for the three managed data-plane hops.
	HopProxyIngress   = "proxy-ingress"
	HopProxyEgress    = "proxy-egress"
	HopBackendIngress = "backend-ingress"

	// DataPlaneDNSPort is the DNS port the proxy egress policy opens (UDP+TCP)
	// so the proxies can resolve backend Service names.
	DataPlaneDNSPort = 53

	// XDSPort is the gRPC xDS port the Envoy Gateway control-plane pod serves on.
	// The data-plane proxies pull their listener/cluster config from it, so the
	// proxy-egress NetworkPolicy must allow egress to the control plane here and
	// the control-plane ingress NetworkPolicy must allow it in.
	XDSPort = 18000

	// MetadataNameLabel is the well-known namespace label kube-apiserver stamps
	// with each namespace's name; used to scope cross-namespace NetworkPolicy
	// peers to a single namespace.
	MetadataNameLabel = "kubernetes.io/metadata.name"

	// DataPlaneHTTPPort is the shifted-up HTTP listener port envoy-gateway
	// configures for non-root data-plane proxies (Service :80 → targetPort).
	DataPlaneHTTPPort = 10080
	// DataPlaneHTTPSPort is the shifted-up HTTPS listener port envoy-gateway
	// configures for non-root data-plane proxies (Service :443 → targetPort).
	DataPlaneHTTPSPort = 10443
	// DataPlaneReadyPort is the readiness-probe port envoy-gateway exposes on
	// the data-plane proxy pods for the load-balancer health check.
	DataPlaneReadyPort = 19003

	// ClusterRoleName is the name of the ClusterRole for the Envoy Gateway controller.
	ClusterRoleName = "envoy-gateway"

	// LeaderElectionRoleName is the name of the Role used for leader election
	// by the Envoy Gateway controller (namespace-scoped).
	LeaderElectionRoleName = "envoy-gateway-leader-election"

	// LogLevelInfo is the default log level used when none is configured.
	LogLevelInfo = "info"

	// LabelName is the "app.kubernetes.io/name" label key.
	LabelName = "app.kubernetes.io/name"
	// LabelInstance is the "app.kubernetes.io/instance" label key.
	LabelInstance = "app.kubernetes.io/instance"
	// LabelComponent is the "app.kubernetes.io/component" label key.
	LabelComponent = "app.kubernetes.io/component"
	// LabelManagedBy is the "app.kubernetes.io/managed-by" label key.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelComponentValue is the value for the gateway-controller component label.
	LabelComponentValue = "gateway-controller"
	// LabelManagedByValue is the value for the gardener managed-by label.
	LabelManagedByValue = "gardener"

	// kindServiceAccount is the Kubernetes Kind name for ServiceAccount.
	kindServiceAccount = "ServiceAccount"
	// kindSecret is the Kubernetes Kind name for Secret.
	kindSecret = "Secret"
	// apiVersionRBAC is the RBAC API version used by the deployer.
	apiVersionRBAC = "rbac.authorization.k8s.io/v1"

	// APIGroupGatewayAPI is the Gateway API resource group.
	APIGroupGatewayAPI = "gateway.networking.k8s.io"
	// apiGroupEnvoyGateway is the Envoy Gateway resource group.
	apiGroupEnvoyGateway = "gateway.envoyproxy.io"

	// OwningGatewayNameLabel is the label envoy-gateway stamps on every
	// per-Gateway data-plane infra object (Deployment, Service, ServiceAccount,
	// ConfigMap, PDB, HPA) it provisions, naming the Gateway that owns it. The
	// control-plane resources this extension deploys into kube-system carry no
	// owning-gateway-* label, so the presence of this label cleanly distinguishes
	// envoy-gateway's per-Gateway data-plane objects from our control plane.
	OwningGatewayNameLabel = "gateway.envoyproxy.io/owning-gateway-name"
	// OwningGatewayNamespaceLabel is the companion to [OwningGatewayNameLabel]
	// naming the namespace of the owning Gateway. Value is fixed by envoy-gateway.
	OwningGatewayNamespaceLabel = "gateway.envoyproxy.io/owning-gateway-namespace"

	// labelValueAllowed is the value Gardener's networking NetworkPolicies gate on.
	labelValueAllowed = "allowed"

	// NetpolControllerName is the base name of the per-shoot data-plane
	// NetworkPolicy controller: it names the seed Deployment, the shoot
	// ServiceAccount it authenticates as, and the in-shoot RBAC objects.
	NetpolControllerName = "envoy-gateway-netpol-controller"
	// NetpolControllerShootAccessSecretName is the (unprefixed) name of the
	// shoot-access secret the token-requestor fills for the controller. The
	// gardener helper prefixes it with "shoot-access-".
	NetpolControllerShootAccessSecretName = "envoy-gateway-netpol"
	// NetpolControllerServiceAccountName is the ServiceAccount the controller
	// authenticates as inside the shoot (lives in kube-system).
	NetpolControllerServiceAccountName = "envoy-gateway-netpol-controller"
	// NetpolControllerClusterRoleName is the in-shoot ClusterRole bound to the
	// controller's ServiceAccount.
	NetpolControllerClusterRoleName = "envoy-gateway-netpol-controller"

	// LabelToAllShootsKubeAPIServer is the pod label that lets a seed
	// control-plane pod reach any shoot's kube-apiserver (TCP 443) past the
	// seed's NetworkPolicies. The netpol controller watches the shoot API, so its
	// pod carries it.
	LabelToAllShootsKubeAPIServer = "networking.resources.gardener.cloud/to-all-shoots-kube-apiserver-tcp-443"
	// LabelToDNS lets a seed pod reach cluster DNS.
	LabelToDNS = "networking.gardener.cloud/to-dns"
	// LabelToRuntimeAPIServer lets a seed pod reach the seed (runtime) apiserver,
	// which hosts the controller's leader-election lease.
	LabelToRuntimeAPIServer = "networking.gardener.cloud/to-runtime-apiserver"

	// EnvoyProxyManagedByValue and EnvoyProxyNameValue are the canonical labels
	// envoy-gateway stamps on the data-plane Envoy proxy pods; the extension's
	// NetworkPolicies (both the control-plane one here and the in-shoot
	// data-plane controller) select on them.
	EnvoyProxyManagedByValue = "envoy-gateway"
	// EnvoyProxyNameValue is the app-name label value on the data-plane Envoy proxy pods.
	EnvoyProxyNameValue = "envoy"

	// verbGet, verbList, verbWatch, verbCreate, verbUpdate, verbPatch, verbDelete
	// are RBAC PolicyRule verbs used by the deployer.
	verbGet              = "get"
	verbList             = "list"
	verbWatch            = "watch"
	verbCreate           = "create"
	verbUpdate           = "update"
	verbPatch            = "patch"
	verbDelete           = "delete"
	verbDeleteCollection = "deletecollection"
)

// ValidLogLevels enumerates the log levels accepted by EnvoyGatewayConfig.
var ValidLogLevels = map[string]struct{}{
	"debug":      {},
	LogLevelInfo: {},
	"warn":       {},
	"error":      {},
}

// ValidChannels enumerates the Gateway API CRD channels accepted by
// EnvoyGatewayConfig.
var ValidChannels = map[string]struct{}{
	"standard":     {},
	"experimental": {},
}
