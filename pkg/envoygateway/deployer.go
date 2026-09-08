// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package envoygateway

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/utils/imagevector"
	"github.com/gardener/gardener/pkg/utils/managedresources"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/apis/config"
)

const (
	// ManagedResourceDeletionTimeout is the maximum time to wait for a
	// ManagedResource to be deleted before timing out.
	ManagedResourceDeletionTimeout = 2 * time.Minute
)

//go:embed assets/gateway-api-standard.yaml
var gatewayAPIStandardYAML []byte

//go:embed assets/gateway-api-experimental.yaml
var gatewayAPIExperimentalYAML []byte

//go:embed assets/envoy-gateway-crds.yaml
var envoyGatewayCRDsYAML []byte

var (
	shootScheme *runtime.Scheme
	shootCodec  runtime.Codec
)

func init() {
	shootScheme = runtime.NewScheme()
	_ = corev1.AddToScheme(shootScheme)
	_ = appsv1.AddToScheme(shootScheme)
	_ = rbacv1.AddToScheme(shootScheme)
	_ = policyv1.AddToScheme(shootScheme)
	_ = networkingv1.AddToScheme(shootScheme)
	shootCodec = serializer.NewCodecFactory(shootScheme).LegacyCodec(
		corev1.SchemeGroupVersion,
		appsv1.SchemeGroupVersion,
		rbacv1.SchemeGroupVersion,
		policyv1.SchemeGroupVersion,
		networkingv1.SchemeGroupVersion,
	)
}

// Config holds the configuration for the Envoy Gateway deployment.
type Config struct {
	// ControlPlaneReplicas is the number of control-plane replicas. When nil,
	// the deployer falls back to the built-in default (2).
	ControlPlaneReplicas *int32
	// ControlPlaneLogLevel sets the log level on the control-plane Envoy Gateway.
	ControlPlaneLogLevel string
	// DataPlaneReplicas is the number of data-plane proxy replicas per Gateway.
	// When nil, the deployer falls back to the built-in default (2).
	DataPlaneReplicas *int32
	// DataPlaneLogLevel sets the log level on the data-plane Envoy proxies.
	DataPlaneLogLevel string
	// ExperimentalFeatures toggles the experimental-channel Gateway API CRDs.
	// Derived from EnvoyGatewayConfig.Channel == "experimental".
	ExperimentalFeatures bool
	// ManageCRDs controls whether the Gateway API / Envoy Gateway CRDs are
	// delivered by this extension. Defaults to true.
	ManageCRDs bool
	// EnvoyProxyDefaults are opinionated defaults applied per-Gateway.
	EnvoyProxyDefaults *config.EnvoyProxyDefaults
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		ControlPlaneLogLevel: LogLevelInfo,
		DataPlaneLogLevel:    LogLevelInfo,
		ManageCRDs:           true,
	}
}

// Deployer handles deploying Envoy Gateway resources to shoot clusters.
type Deployer struct {
	client      client.Client
	logger      logr.Logger
	config      Config
	imageVector imagevector.ImageVector
}

// NewDeployer creates a new Deployer.
func NewDeployer(c client.Client, logger logr.Logger, cfg Config, imageVector imagevector.ImageVector) *Deployer {
	return &Deployer{
		client:      c,
		logger:      logger.WithName("envoy-gateway-deployer"),
		config:      cfg,
		imageVector: imageVector,
	}
}

// TLSBundle carries the PEM-encoded materials shipped to the envoy-gateway
// pod at /certs, plus the sibling Secrets the controller reads from its own
// namespace. CertPEM/KeyPEM/CAPEM back the xDS server; ClientCertPEM and
// ClientKeyPEM back the data-plane envoy proxy client; HMACBytes backs the
// OIDC OAuth2 filter.
type TLSBundle struct {
	CertPEM       []byte
	KeyPEM        []byte
	CAPEM         []byte
	ClientCertPEM []byte
	ClientKeyPEM  []byte
	HMACBytes     []byte
}

// Deploy deploys Envoy Gateway to the shoot cluster via a ManagedResource.
func (d *Deployer) Deploy(ctx context.Context, namespace string, tls TLSBundle) error {
	d.logger.Info("deploying envoy-gateway to shoot cluster", "namespace", namespace)

	resources, err := d.buildResources(tls)
	if err != nil {
		return fmt.Errorf("failed to generate envoy-gateway resources: %w", err)
	}

	// The Gateway API and Envoy Gateway CRDs alone exceed the 1 MiB Kubernetes
	// Secret size limit. Hand the pre-serialized manifests to managedresources.
	// Registry, which sorts them, joins them with '---' separators, and
	// brotli-compresses the stream under the "data.yaml.br" key that
	// gardener-resource-manager transparently decompresses.
	registry := managedresources.NewRegistry(kubernetes.ShootScheme, kubernetes.ShootCodec, kubernetes.ShootSerializer)
	for name, data := range resources {
		registry.AddSerialized(name, data)
	}
	serialized, err := registry.SerializedObjects()
	if err != nil {
		return fmt.Errorf("failed to serialize envoy-gateway manifests: %w", err)
	}

	if err := managedresources.CreateForShoot(ctx, d.client, namespace, ManagedResourceName, ManagedResourceName, false, serialized); err != nil {
		return fmt.Errorf("failed to create or update managed resource: %w", err)
	}

	d.logger.Info("successfully deployed envoy-gateway", "namespace", namespace)

	return nil
}

// Delete removes Envoy Gateway from the shoot cluster.
func (d *Deployer) Delete(ctx context.Context, namespace string) error {
	return d.deleteManagedResource(ctx, namespace)
}

// DeleteKeepingObjects removes the ManagedResource without deleting the
// underlying shoot-cluster objects. Use this during force-delete or migrate.
func (d *Deployer) DeleteKeepingObjects(ctx context.Context, namespace string) error {
	if err := managedresources.SetKeepObjects(ctx, d.client, namespace, ManagedResourceName, true); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to set keepObjects on managed resource: %w", err)
	}

	return d.deleteManagedResource(ctx, namespace)
}

func (d *Deployer) deleteManagedResource(ctx context.Context, namespace string) error {
	d.logger.Info("deleting envoy-gateway from shoot cluster", "namespace", namespace)

	if err := managedresources.Delete(ctx, d.client, namespace, ManagedResourceName, true); err != nil {
		return fmt.Errorf("failed to delete managed resource: %w", err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, ManagedResourceDeletionTimeout)
	defer cancel()

	if err := managedresources.WaitUntilDeleted(timeoutCtx, d.client, namespace, ManagedResourceName); err != nil {
		return fmt.Errorf("timed out waiting for managed resource to be deleted: %w", err)
	}

	d.logger.Info("successfully deleted envoy-gateway", "namespace", namespace)

	return nil
}

// GenerateResources is exposed for unit testing.
func (d *Deployer) GenerateResources() (map[string][]byte, error) {
	return d.buildResources(TLSBundle{})
}

func (d *Deployer) buildResources(tls TLSBundle) (map[string][]byte, error) {
	resources := make(map[string][]byte)

	encode := func(name string, obj runtime.Object) error {
		data, err := runtime.Encode(shootCodec, obj)
		if err != nil {
			return fmt.Errorf("failed to encode %s: %w", name, err)
		}
		resources[name] = data

		return nil
	}

	deploy, err := d.deployment()
	if err != nil {
		return nil, err
	}

	// Fixed set of typed objects, encoded in a table to keep this function's
	// complexity low. Secrets are added conditionally below.
	objects := []struct {
		name string
		obj  runtime.Object
	}{
		{"serviceaccount.yaml", d.serviceAccount()},
		{"configmap.yaml", d.configMap()},
		{"clusterrole.yaml", d.clusterRole()},
		{"clusterrolebinding.yaml", d.clusterRoleBinding()},
		{"role.yaml", d.leaderElectionRole()},
		{"rolebinding.yaml", d.leaderElectionRoleBinding()},
		{"deployment.yaml", deploy},
		{"service.yaml", d.service()},
		{"poddisruptionbudget.yaml", d.podDisruptionBudget()},
		{"networkpolicy.yaml", d.networkPolicy()},
		{"networkpolicy-proxies.yaml", d.networkPolicyForEnvoyProxies()},
	}
	for _, o := range objects {
		if err := encode(o.name, o.obj); err != nil {
			return nil, err
		}
	}

	if len(tls.CertPEM) > 0 || len(tls.KeyPEM) > 0 || len(tls.CAPEM) > 0 {
		if err := encode("secret-tls.yaml", d.tlsSecret(tls)); err != nil {
			return nil, err
		}
	}
	if len(tls.ClientCertPEM) > 0 && len(tls.ClientKeyPEM) > 0 && len(tls.CAPEM) > 0 {
		if err := encode("secret-envoy.yaml", d.envoyClientSecret(tls)); err != nil {
			return nil, err
		}
	}
	if len(tls.HMACBytes) > 0 {
		if err := encode("secret-hmac.yaml", d.hmacSecret(tls)); err != nil {
			return nil, err
		}
	}

	// GatewayClass is delivered as raw YAML because we don't want to pull the
	// sigs.k8s.io/gateway-api types into our deployer scheme.
	resources["gatewayclass.yaml"] = []byte(d.gatewayClassYAML())
	// Default EnvoyProxy CR referenced by the GatewayClass. Ships pod labels
	// that let data-plane envoy pods pass Gardener's kube-system egress
	// NetworkPolicies.
	resources["envoyproxy-defaults.yaml"] = []byte(d.envoyProxyDefaultsYAML())

	if d.config.ManageCRDs {
		d.addCRDs(resources)
	}

	return resources, nil
}

// addCRDs adds the embedded Gateway API standard-channel CRDs, the Envoy
// Gateway CRDs, and (when enabled) the experimental-channel CRDs to resources.
// The envoy-gateway helm chart bundles Gateway API CRDs alongside its own —
// the envoy-gateway split is restricted to the gateway.envoyproxy.io group so
// the authoritative Gateway API release isn't overwritten by an older bundled
// copy.
func (d *Deployer) addCRDs(resources map[string][]byte) {
	if crds, err := splitCRDs(gatewayAPIStandardYAML, "gateway-api", nil); err == nil {
		maps.Copy(resources, crds)
	}
	if d.config.ExperimentalFeatures {
		if crds, err := splitCRDs(gatewayAPIExperimentalYAML, "gateway-api-experimental", nil); err == nil {
			maps.Copy(resources, crds)
		}
	}
	if crds, err := splitCRDs(envoyGatewayCRDsYAML, "envoy-gateway", []string{apiGroupEnvoyGateway}); err == nil {
		maps.Copy(resources, crds)
	}
}

// splitCRDs splits a multi-document YAML byte slice into one entry per CRD,
// keyed as "crd-<prefix>-<crdname>.yaml". CRDs that are missing a name or are
// not CustomResourceDefinitions are skipped. The first argument may be empty
// or contain only placeholder text; in that case the function returns an
// empty map without erroring out. If keepGroups is non-nil, only CRDs whose
// spec.group is in the set are kept.
func splitCRDs(raw []byte, prefix string, keepGroups []string) (map[string][]byte, error) {
	result := make(map[string][]byte)
	if len(bytes.TrimSpace(raw)) == 0 {
		return result, nil
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := decoder.Decode(&crd); err != nil {
			if err == io.EOF {
				break
			}
			// Skip documents that don't decode as a CRD (e.g. namespaces shipped
			// alongside the CRDs in some upstream installs).
			continue
		}
		if crd.Kind != "CustomResourceDefinition" || crd.Name == "" {
			continue
		}
		if keepGroups != nil && !slices.Contains(keepGroups, crd.Spec.Group) {
			continue
		}
		data, err := json.Marshal(&crd)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal CRD %s: %w", crd.Name, err)
		}
		result[fmt.Sprintf("crd-%s-%s.yaml", prefix, crd.Name)] = data
	}

	return result, nil
}

func commonLabels() map[string]string {
	return map[string]string{
		LabelName:      DeploymentName,
		LabelInstance:  DeploymentName,
		LabelComponent: LabelComponentValue,
		LabelManagedBy: LabelManagedByValue,
	}
}

// podNetworkLabels are the egress allow-labels that Gardener's NetworkPolicies
// in kube-system gate on. Without them the shoot pod cannot reach the shoot
// apiserver (which lives outside the cluster network) or kube-dns.
func podNetworkLabels() map[string]string {
	return map[string]string{
		"networking.gardener.cloud/to-apiserver":       labelValueAllowed,
		"networking.gardener.cloud/to-dns":             labelValueAllowed,
		"networking.gardener.cloud/to-public-networks": labelValueAllowed,
	}
}

func (d *Deployer) serviceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       kindServiceAccount,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceAccountName,
			Namespace: Namespace,
			Labels:    commonLabels(),
		},
	}
}

// configMap renders the EnvoyGateway server config that the control-plane
// container reads at startup via --config-path. The log level is the only
// knob we surface today; defaults match upstream where omitted.
func (d *Deployer) configMap() *corev1.ConfigMap {
	logLevel := d.config.ControlPlaneLogLevel
	if logLevel == "" {
		logLevel = LogLevelInfo
	}
	cfg := fmt.Sprintf(`apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyGateway
logging:
  level:
    default: %s
`, logLevel)

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: Namespace,
			Labels:    commonLabels(),
		},
		Data: map[string]string{
			ConfigFileName: cfg,
		},
	}
}

// tlsSecret carries the xDS server cert/key plus the signing CA bundle that
// envoy-gateway reads from /certs (tls.crt, tls.key, ca.crt). The cert is
// served to the data-plane Envoy proxies on the xDS gRPC connection; the CA
// is used to verify those proxies' client certs.
func (d *Deployer) tlsSecret(tls TLSBundle) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       kindSecret,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      TLSSecretName,
			Namespace: Namespace,
			Labels:    commonLabels(),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": tls.CertPEM,
			"tls.key": tls.KeyPEM,
			"ca.crt":  tls.CAPEM,
		},
	}
}

// envoyClientSecret is the "envoy" Secret that envoy-gateway's provider looks
// up on every reconcile (see processEnvoyTLSSecret upstream). It carries the
// data-plane envoy proxy's client cert; the same CA that signed the xDS
// server cert also signs this one, so the mTLS mesh trusts both directions.
func (d *Deployer) envoyClientSecret(tls TLSBundle) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       kindSecret,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      EnvoySecretName,
			Namespace: Namespace,
			Labels:    commonLabels(),
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": tls.ClientCertPEM,
			"tls.key": tls.ClientKeyPEM,
			"ca.crt":  tls.CAPEM,
		},
	}
}

// hmacSecret is the "envoy-oidc-hmac" Secret the SecurityPolicy OIDC filter
// consumes when signing OAuth2 state cookies. envoy-gateway logs an ERROR
// every reconcile if this is absent even when no SecurityPolicy uses OIDC —
// we always ship it to keep the logs clean.
func (d *Deployer) hmacSecret(tls TLSBundle) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       kindSecret,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      HMACSecretName,
			Namespace: Namespace,
			Labels:    commonLabels(),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			HMACSecretKey: tls.HMACBytes,
		},
	}
}

func (d *Deployer) clusterRole() *rbacv1.ClusterRole {
	rules := []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{"namespaces", "nodes"},
			Verbs:     []string{verbGet, verbList, verbWatch},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"endpoints", "secrets"},
			Verbs:     []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate, verbPatch, verbDelete},
		},
		{
			// serviceaccounts/services/configmaps: the "infra-manager"
			// half of envoy-gateway creates the per-Gateway data-plane
			// envoy Deployment plus its SA/Svc/CM, and cleans them up
			// on GatewayClass detach via deletecollection.
			APIGroups: []string{""},
			Resources: []string{"serviceaccounts", "services", "configmaps"},
			Verbs:     []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate, verbPatch, verbDelete, verbDeleteCollection},
		},
		{
			APIGroups: []string{"discovery.k8s.io"},
			Resources: []string{"endpointslices"},
			Verbs:     []string{verbGet, verbList, verbWatch},
		},
		{
			APIGroups: []string{"apps"},
			Resources: []string{"deployments", "daemonsets"},
			Verbs:     []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate, verbPatch, verbDelete, verbDeleteCollection},
		},
		{
			APIGroups: []string{"autoscaling"},
			Resources: []string{"horizontalpodautoscalers"},
			Verbs:     []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate, verbPatch, verbDelete, verbDeleteCollection},
		},
		{
			APIGroups: []string{"policy"},
			Resources: []string{"poddisruptionbudgets"},
			Verbs:     []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate, verbPatch, verbDelete, verbDeleteCollection},
		},
		{
			// tokenreviews: infra-manager validates ServiceAccount tokens
			// mounted into the data-plane envoy pods before letting them
			// pull config from the xDS server.
			APIGroups: []string{"authentication.k8s.io"},
			Resources: []string{"tokenreviews"},
			Verbs:     []string{verbCreate},
		},
		{
			// gatewayclasses is cluster-scoped; envoy-gateway also sets a
			// finalizer on the class it manages, so patch/update are needed
			// in addition to the read verbs.
			APIGroups: []string{apiGroupGatewayAPI},
			Resources: []string{"gatewayclasses"},
			Verbs:     []string{verbGet, verbList, verbWatch, verbPatch, verbUpdate},
		},
		{
			APIGroups: []string{apiGroupGatewayAPI},
			Resources: []string{"gateways", "httproutes", "grpcroutes", "tcproutes", "tlsroutes", "udproutes", "referencegrants", "backendtlspolicies", "listenersets"},
			Verbs:     []string{verbGet, verbList, verbWatch},
		},
		{
			APIGroups: []string{apiGroupGatewayAPI},
			Resources: []string{"gatewayclasses/status", "gateways/status", "httproutes/status", "grpcroutes/status", "tcproutes/status", "tlsroutes/status", "udproutes/status", "backendtlspolicies/status", "listenersets/status"},
			Verbs:     []string{verbUpdate, verbPatch},
		},
		{
			APIGroups: []string{apiGroupEnvoyGateway},
			Resources: []string{"envoyproxies", "envoypatchpolicies", "clienttrafficpolicies", "backendtrafficpolicies", "securitypolicies", "envoyextensionpolicies", "backends", "httproutefilters"},
			Verbs:     []string{verbGet, verbList, verbWatch},
		},
		{
			APIGroups: []string{apiGroupEnvoyGateway},
			Resources: []string{"envoyproxies/status", "envoypatchpolicies/status", "clienttrafficpolicies/status", "backendtrafficpolicies/status", "securitypolicies/status", "envoyextensionpolicies/status", "backends/status"},
			Verbs:     []string{verbUpdate, verbPatch},
		},
	}

	return &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiVersionRBAC,
			Kind:       "ClusterRole",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   ClusterRoleName,
			Labels: commonLabels(),
		},
		Rules: rules,
	}
}

func (d *Deployer) clusterRoleBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiVersionRBAC,
			Kind:       "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   ClusterRoleName,
			Labels: commonLabels(),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     ClusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      kindServiceAccount,
				Name:      ServiceAccountName,
				Namespace: Namespace,
			},
		},
	}
}

func (d *Deployer) leaderElectionRole() *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiVersionRBAC,
			Kind:       "Role",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      LeaderElectionRoleName,
			Namespace: Namespace,
			Labels:    commonLabels(),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"coordination.k8s.io"},
				Resources: []string{"leases"},
				Verbs:     []string{verbGet, verbList, verbWatch, verbCreate, verbUpdate, verbPatch, verbDelete},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{verbCreate, verbPatch},
			},
		},
	}
}

func (d *Deployer) leaderElectionRoleBinding() *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiVersionRBAC,
			Kind:       "RoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      LeaderElectionRoleName,
			Namespace: Namespace,
			Labels:    commonLabels(),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     LeaderElectionRoleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      kindServiceAccount,
				Name:      ServiceAccountName,
				Namespace: Namespace,
			},
		},
	}
}

func (d *Deployer) deployment() (*appsv1.Deployment, error) {
	img, err := d.imageVector.FindImage(ImageName)
	if err != nil {
		return nil, fmt.Errorf("failed to find envoy-gateway image in image vector: %w", err)
	}
	image := img.String()

	labels := commonLabels()
	podLabels := commonLabels()
	maps.Copy(podLabels, podNetworkLabels())

	replicas := d.config.ControlPlaneReplicas
	if replicas == nil {
		replicas = ptr.To[int32](2)
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DeploymentName,
			Namespace: Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelName:     DeploymentName,
					LabelInstance: DeploymentName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: ServiceAccountName,
					// The PDB (minAvailable=1) is meaningless if both replicas can
					// land on the same node, and the TopologySpreadConstraint is
					// meaningless if both can land in the same zone. Both are
					// preferred (not required) so single-node/single-zone shoots
					// still schedule.
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
								{
									Weight: 100,
									PodAffinityTerm: corev1.PodAffinityTerm{
										TopologyKey: corev1.LabelHostname,
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: map[string]string{
												LabelName:     DeploymentName,
												LabelInstance: DeploymentName,
											},
										},
									},
								},
							},
						},
					},
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
						{
							MaxSkew:           1,
							TopologyKey:       corev1.LabelTopologyZone,
							WhenUnsatisfiable: corev1.ScheduleAnyway,
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									LabelName:     DeploymentName,
									LabelInstance: DeploymentName,
								},
							},
						},
					},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true),
						RunAsUser:    ptr.To[int64](65532),
						RunAsGroup:   ptr.To[int64](65532),
						FSGroup:      ptr.To[int64](65532),
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: ConfigMapName},
								},
							},
						},
						{
							Name: "certs",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: TLSSecretName,
								},
							},
						},
						{
							// envoy-gateway uses /var/lib/eg as scratch for its wasm
							// module cache. The container root is read-only for
							// hardening, so we carve out a bounded emptyDir here —
							// per-pod, non-persistent, capped at 100Mi to prevent a
							// compromised process from exhausting node scratch space.
							Name: "wasm-cache",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{
									SizeLimit: new(resource.MustParse("100Mi")),
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:  DeploymentName,
							Image: image,
							Args: []string{
								"server",
								fmt.Sprintf("--config-path=%s/%s", ConfigMountPath, ConfigFileName),
							},
							Env: []corev1.EnvVar{
								// envoy-gateway reads ENVOY_GATEWAY_NAMESPACE as its
								// ControllerNamespace at startup. It backs the
								// leader-election lease namespace, the watch namespace
								// for owned objects, and the in-cluster API endpoint
								// for its components. We deploy into kube-system, so
								// this env var must reflect that — otherwise the pod
								// reaches into the upstream default "envoy-gateway-system"
								// where it has neither RBAC nor a Service.
								{
									Name: "ENVOY_GATEWAY_NAMESPACE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.namespace",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "config", MountPath: ConfigMountPath, ReadOnly: true},
								{Name: "certs", MountPath: "/certs", ReadOnly: true},
								{Name: "wasm-cache", MountPath: "/var/lib/eg"},
							},
							Ports: []corev1.ContainerPort{
								{Name: "grpc-xds", ContainerPort: 18000, Protocol: corev1.ProtocolTCP},
								{Name: "metrics", ContainerPort: 19001, Protocol: corev1.ProtocolTCP},
								{Name: "health", ContainerPort: 8081, Protocol: corev1.ProtocolTCP},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(8081),
									},
								},
								InitialDelaySeconds: 15,
								PeriodSeconds:       20,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/readyz",
										Port: intstr.FromInt(8081),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: new(false),
								ReadOnlyRootFilesystem:   new(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
						},
					},
				},
			},
		},
	}, nil
}

func (d *Deployer) service() *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DeploymentName,
			Namespace: Namespace,
			Labels:    commonLabels(),
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				LabelName:     DeploymentName,
				LabelInstance: DeploymentName,
			},
			Ports: []corev1.ServicePort{
				{Name: "grpc-xds", Port: 18000, TargetPort: intstr.FromString("grpc-xds"), Protocol: corev1.ProtocolTCP},
				{Name: "metrics", Port: 19001, TargetPort: intstr.FromString("metrics"), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func (d *Deployer) podDisruptionBudget() *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "policy/v1",
			Kind:       "PodDisruptionBudget",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DeploymentName,
			Namespace: Namespace,
			Labels:    commonLabels(),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelName:     DeploymentName,
					LabelInstance: DeploymentName,
				},
			},
			UnhealthyPodEvictionPolicy: ptr.To(policyv1.AlwaysAllow),
		},
	}
}

// networkPolicy allows the data-plane Envoy pods (managed by envoy-gateway,
// running in the same namespace) to reach the control-plane pod on:
//   - 18000/TCP — xDS server
//   - 19001/TCP — metrics endpoint (for scraping)
//
// Without this, Gardener's default-deny NetworkPolicy in kube-system blocks
// every inbound connection to our pod, and every user Gateway's Envoy stays
// stuck with "xds_cluster connection timeout".
func (d *Deployer) networkPolicy() *networkingv1.NetworkPolicy {
	xdsPort := intstr.FromInt(18000)
	metricsPort := intstr.FromInt(19001)
	tcp := corev1.ProtocolTCP

	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.k8s.io/v1",
			Kind:       "NetworkPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DeploymentName,
			Namespace: Namespace,
			Labels:    commonLabels(),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelName:     DeploymentName,
					LabelInstance: DeploymentName,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							LabelManagedBy: envoyProxyManagedByValue,
							LabelName:      envoyProxyNameValue,
						},
					},
				}},
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &xdsPort},
					{Protocol: &tcp, Port: &metricsPort},
				},
			}},
		},
	}
}

// networkPolicyForEnvoyProxies allows external traffic to reach the
// data-plane Envoy proxies that envoy-gateway spawns per user-created
// Gateway. Those proxies are exposed via a LoadBalancer Service; without
// this policy, Gardener's default-deny NetworkPolicy in kube-system blocks
// both the AWS ELB health checks (which hit the healthCheckNodePort) and the
// actual client traffic that lands on the proxy's container port.
//
// The policy selects any pod that envoy-gateway has stamped with its
// canonical labels (managed-by=envoy-gateway, name=envoy) and allows
// ingress from anywhere on the two well-known data-plane ports:
//   - 10080/TCP — the shifted-up HTTP listener envoy-gateway configures
//     for non-root pods to expose Service :80 as targetPort :10080
//   - 19003/TCP — the readiness probe port envoy-gateway itself exposes
//     on the envoy proxy pod for the LB health check to succeed
func (d *Deployer) networkPolicyForEnvoyProxies() *networkingv1.NetworkPolicy {
	dataPort := intstr.FromInt(10080)
	readyPort := intstr.FromInt(19003)
	tcp := corev1.ProtocolTCP

	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.k8s.io/v1",
			Kind:       "NetworkPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DeploymentName + "-proxies",
			Namespace: Namespace,
			Labels:    commonLabels(),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/managed-by": "envoy-gateway",
					"app.kubernetes.io/name":       "envoy",
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				// Empty From: allow from anywhere. This is a data-plane
				// ingress path — the whole point of a Gateway is to accept
				// external traffic, so the security model here is exactly
				// what the user opted into by creating a Gateway.
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &dataPort},
					{Protocol: &tcp, Port: &readyPort},
				},
			}},
		},
	}
}

// gatewayClassYAML returns the GatewayClass installed by this extension as
// raw YAML so we don't have to pull sigs.k8s.io/gateway-api into the scheme.
// The class points at a default EnvoyProxy CR (see envoyProxyDefaultsYAML)
// via parametersRef so every user Gateway inherits the pod labels the
// shoot's kube-system NetworkPolicies require for egress.
func (d *Deployer) gatewayClassYAML() string {
	return fmt.Sprintf(`apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: %s
  labels:
    %s: %s
    %s: %s
    %s: %s
spec:
  controllerName: %s
  parametersRef:
    group: gateway.envoyproxy.io
    kind: EnvoyProxy
    name: %s
    namespace: %s
`,
		GatewayClassName,
		LabelName, DeploymentName,
		LabelInstance, DeploymentName,
		LabelManagedBy, LabelManagedByValue,
		GatewayClassControllerName,
		EnvoyProxyDefaultsName,
		Namespace,
	)
}

// envoyProxyDefaultsYAML returns the cluster-default EnvoyProxy CR referenced
// by our GatewayClass. It tags every data-plane Envoy pod with the
// networking.gardener.cloud/to-*=allowed labels so Gardener's default-deny
// NetworkPolicies in kube-system let the pod reach:
//   - DNS (for envoy-gateway.kube-system.svc.cluster.local resolution)
//   - the envoy-gateway xDS server (control-plane pod in kube-system)
//   - the shoot apiserver (used by envoy-gateway's provisioning path)
//   - public networks (upstream traffic; user Services may live anywhere)
//
// Beyond the mandatory pod labels, it renders the operator-tunable knobs from
// EnvoyGatewayConfig: the per-Gateway data-plane replica count, the proxy log
// level, optional access logging, and optional container resources.
func (d *Deployer) envoyProxyDefaultsYAML() string {
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
    %s: %s
    %s: %s
spec:
`,
		EnvoyProxyDefaultsName,
		Namespace,
		LabelName, DeploymentName,
		LabelInstance, DeploymentName,
		LabelManagedBy, LabelManagedByValue,
	)

	if d.config.DataPlaneLogLevel != "" {
		fmt.Fprintf(&b, "  logging:\n    level:\n      default: %s\n", d.config.DataPlaneLogLevel)
	}

	// Access logging is opt-in. Setting spec.telemetry.accessLog with no
	// entries disables it; a single default text sink enables it.
	if d.config.EnvoyProxyDefaults != nil && d.config.EnvoyProxyDefaults.AccessLogging {
		fmt.Fprint(&b, `  telemetry:
    accessLog:
      settings:
      - format:
          type: Text
`)
	}

	fmt.Fprint(&b, "  provider:\n    type: Kubernetes\n    kubernetes:\n      envoyDeployment:\n")

	if replicas := d.config.DataPlaneReplicas; replicas != nil {
		fmt.Fprintf(&b, "        replicas: %d\n", *replicas)
	}

	if d.config.EnvoyProxyDefaults != nil && d.config.EnvoyProxyDefaults.Resources != nil {
		fmt.Fprint(&b, indentYAML(marshalResources(d.config.EnvoyProxyDefaults.Resources), 8, "container:\n"))
	}

	// The pod labels are mandatory: without them Gardener's kube-system
	// default-deny NetworkPolicies block the data-plane proxy's egress.
	fmt.Fprint(&b, `        pod:
          labels:
            networking.gardener.cloud/to-apiserver: allowed
            networking.gardener.cloud/to-dns: allowed
            networking.gardener.cloud/to-public-networks: allowed
`)

	return b.String()
}

// marshalResources renders a ResourceRequirements into YAML under a
// "resources:" key. Errors are swallowed into an empty string — the caller
// only reaches this path when Resources is non-nil, and a malformed
// ResourceList cannot occur from the typed API.
func marshalResources(r *corev1.ResourceRequirements) string {
	out, err := sigsyaml.Marshal(map[string]any{"resources": r})
	if err != nil {
		return ""
	}

	return string(out)
}

// indentYAML prefixes every non-empty line of s with n spaces and prepends the
// given header (already at the target indentation). Used to splice a
// marshalled sub-document into the hand-built EnvoyProxy YAML at the right
// depth.
func indentYAML(s string, n int, header string) string {
	pad := strings.Repeat(" ", n)
	var b strings.Builder
	fmt.Fprint(&b, pad)
	fmt.Fprint(&b, header)
	for line := range strings.SplitSeq(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			continue
		}
		fmt.Fprint(&b, pad)
		fmt.Fprint(&b, "  ")
		fmt.Fprint(&b, line)
		fmt.Fprint(&b, "\n")
	}

	return b.String()
}
