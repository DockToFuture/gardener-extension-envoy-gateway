// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

// Package envoygateway provides resources for deploying the Envoy Gateway
// control plane and the Gateway API CRDs to shoot clusters.
package envoygateway

const (
	// Namespace is the namespace where Envoy Gateway is deployed in the shoot.
	// Must be one of the namespaces watched by the shoot's gardener-resource-manager
	// (kube-system, kubernetes-dashboard, kube-node-lease) — otherwise applying the
	// ManagedResource fails with "unknown namespace for the cache".
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
	// managed-policy label value used to select these policies for pruning.
	DataPlaneNetworkPolicyName = "envoy-gateway-proxies"

	// DataPlaneHTTPPort is the shifted-up HTTP listener port envoy-gateway
	// configures for non-root data-plane proxies (Service :80 → targetPort).
	DataPlaneHTTPPort = 10080
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

	// apiGroupGatewayAPI is the Gateway API resource group.
	apiGroupGatewayAPI = "gateway.networking.k8s.io"
	// apiGroupEnvoyGateway is the Envoy Gateway resource group.
	apiGroupEnvoyGateway = "gateway.envoyproxy.io"

	// labelValueAllowed is the value Gardener's networking NetworkPolicies gate on.
	labelValueAllowed = "allowed"

	// EnvoyProxyManagedByValue and EnvoyProxyNameValue are the canonical labels
	// envoy-gateway stamps on the data-plane Envoy proxy pods; the extension's
	// NetworkPolicies (both the control-plane one here and the in-shoot
	// data-plane controller) select on them.
	EnvoyProxyManagedByValue = "envoy-gateway"
	EnvoyProxyNameValue      = "envoy"

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
