// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package envoygateway

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gardener/gardener/pkg/utils/imagevector"
	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/apis/config"
)

func newTestImageVector(t *testing.T) imagevector.ImageVector {
	t.Helper()
	iv, err := imagevector.Read([]byte(`---
images:
- name: envoy-gateway
  repository: docker.io/envoyproxy/gateway
  tag: "v1.8.3"
`))
	if err != nil {
		t.Fatalf("failed to construct test image vector: %v", err)
	}

	return iv
}

// supportedConfig returns a DefaultConfig pinned to a Kubernetes version that
// serves the admissionregistration.k8s.io/v1 ValidatingAdmissionPolicy API, so
// the guard is emitted. Tests that assert the VAP is present use this.
func supportedConfig() Config {
	cfg := DefaultConfig()
	cfg.ShootKubernetesVersion = "1.30.0"

	return cfg
}

func TestGenerateResources_DefaultConfig(t *testing.T) {
	d := NewDeployer(nil, logr.Discard(), supportedConfig(), newTestImageVector(t))
	resources, err := d.GenerateResources()
	if err != nil {
		t.Fatalf("GenerateResources returned error: %v", err)
	}

	expected := []string{
		"serviceaccount.yaml",
		"configmap.yaml",
		"clusterrole.yaml",
		"clusterrolebinding.yaml",
		"role.yaml",
		"rolebinding.yaml",
		"deployment.yaml",
		"service.yaml",
		"poddisruptionbudget.yaml",
		"gatewayclass.yaml",
		"validatingadmissionpolicy.yaml",
	}
	for _, name := range expected {
		if _, ok := resources[name]; !ok {
			t.Errorf("expected resource %q not found in generated resources", name)
		}
	}

	// The dead kube-system data-plane policy must no longer be shipped — it is
	// replaced by the opt-in, seed-side per-shoot NetworkPolicy controller.
	if _, ok := resources["networkpolicy-proxies.yaml"]; ok {
		t.Error("did not expect networkpolicy-proxies.yaml to be generated")
	}

	// The netpol-controller in-shoot RBAC is gated on
	// ManageDataPlaneNetworkPolicies, which DefaultConfig leaves false.
	for _, name := range []string{
		"netpol-controller-serviceaccount.yaml",
		"netpol-controller-clusterrole.yaml",
		"netpol-controller-clusterrolebinding.yaml",
	} {
		if _, ok := resources[name]; ok {
			t.Errorf("did not expect %q when ManageDataPlaneNetworkPolicies is false", name)
		}
	}
}

func TestGenerateResources_ShipsNetpolControllerRBACWhenEnabled(t *testing.T) {
	cfg := supportedConfig()
	cfg.ManageDataPlaneNetworkPolicies = true
	d := NewDeployer(nil, logr.Discard(), cfg, newTestImageVector(t))

	resources, err := d.GenerateResources()
	if err != nil {
		t.Fatalf("GenerateResources returned error: %v", err)
	}

	for _, name := range []string{
		"netpol-controller-serviceaccount.yaml",
		"netpol-controller-clusterrole.yaml",
		"netpol-controller-clusterrolebinding.yaml",
	} {
		if _, ok := resources[name]; !ok {
			t.Errorf("expected %q to be generated when ManageDataPlaneNetworkPolicies is true", name)
		}
	}
}

func TestConfigMap_HardcodesGatewayNamespaceDeployMode(t *testing.T) {
	d := NewDeployer(nil, logr.Discard(), DefaultConfig(), newTestImageVector(t))
	cm := d.configMap()
	body := cm.Data[ConfigFileName]

	for _, want := range []string{"provider:", "type: Kubernetes", "type: GatewayNamespace"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected configMap body to contain %q\n---\n%s", want, body)
		}
	}
}

func TestGenerateResources_ShipsEnvoyProxyGuardVAP(t *testing.T) {
	d := NewDeployer(nil, logr.Discard(), supportedConfig(), newTestImageVector(t))
	resources, err := d.GenerateResources()
	if err != nil {
		t.Fatalf("GenerateResources returned error: %v", err)
	}

	vap, ok := resources["validatingadmissionpolicy.yaml"]
	if !ok {
		t.Fatal("expected validatingadmissionpolicy.yaml in generated resources")
	}
	body := string(vap)
	for _, want := range []string{
		"ValidatingAdmissionPolicy",
		"ValidatingAdmissionPolicyBinding",
		"envoyDeployment",
		"envoyDaemonSet",
		"patch",
		"initContainers",
		"validationActions",
		"kube-system",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected VAP YAML to contain %q\n---\n%s", want, body)
		}
	}
}

func TestGenerateResources_SkipsEnvoyProxyGuardVAPOnOldKubernetes(t *testing.T) {
	for _, version := range []string{"1.29.9", ""} {
		cfg := DefaultConfig()
		cfg.ShootKubernetesVersion = version
		d := NewDeployer(nil, logr.Discard(), cfg, newTestImageVector(t))
		resources, err := d.GenerateResources()
		if err != nil {
			t.Fatalf("GenerateResources(version=%q) returned error: %v", version, err)
		}
		if _, ok := resources["validatingadmissionpolicy.yaml"]; ok {
			t.Errorf("expected VAP to be skipped for Kubernetes version %q, but it was generated", version)
		}
	}
}

func TestNetworkPolicy_AllowsCrossNamespaceProxies(t *testing.T) {
	d := NewDeployer(nil, logr.Discard(), DefaultConfig(), newTestImageVector(t))
	np := d.networkPolicy()

	if len(np.Spec.Ingress) == 0 || len(np.Spec.Ingress[0].From) == 0 {
		t.Fatal("expected networkPolicy to have an ingress peer")
	}
	peer := np.Spec.Ingress[0].From[0]
	if peer.NamespaceSelector == nil {
		t.Error("expected an all-namespaces NamespaceSelector on the ingress peer")
	}
	if peer.PodSelector == nil {
		t.Fatal("expected a PodSelector on the ingress peer")
	}
	if got := peer.PodSelector.MatchLabels[LabelManagedBy]; got != EnvoyProxyManagedByValue {
		t.Errorf("expected podSelector managed-by=%q, got %q", EnvoyProxyManagedByValue, got)
	}
	if got := peer.PodSelector.MatchLabels[LabelName]; got != EnvoyProxyNameValue {
		t.Errorf("expected podSelector name=%q, got %q", EnvoyProxyNameValue, got)
	}
}

func TestGenerateResources_ExperimentalFeaturesFlag(t *testing.T) {
	// Without experimental features → no experimental CRDs even if the asset is non-empty.
	cfg := DefaultConfig()
	cfg.ExperimentalFeatures = false
	d := NewDeployer(nil, logr.Discard(), cfg, newTestImageVector(t))
	_, err := d.GenerateResources()
	if err != nil {
		t.Fatalf("GenerateResources returned error: %v", err)
	}
}

func TestBuildResources_EmitsTLSSecretWhenBundleProvided(t *testing.T) {
	d := NewDeployer(nil, logr.Discard(), DefaultConfig(), newTestImageVector(t))
	tls := TLSBundle{CertPEM: []byte("c"), KeyPEM: []byte("k"), CAPEM: []byte("a")}
	resources, err := d.buildResources(tls)
	if err != nil {
		t.Fatalf("buildResources returned error: %v", err)
	}
	if _, ok := resources["secret-tls.yaml"]; !ok {
		t.Fatal("expected secret-tls.yaml in resources when TLS bundle is supplied")
	}
}

func TestBuildResources_OmitsTLSSecretWhenBundleEmpty(t *testing.T) {
	d := NewDeployer(nil, logr.Discard(), DefaultConfig(), newTestImageVector(t))
	resources, err := d.buildResources(TLSBundle{})
	if err != nil {
		t.Fatalf("buildResources returned error: %v", err)
	}
	if _, ok := resources["secret-tls.yaml"]; ok {
		t.Fatal("did not expect secret-tls.yaml in resources without TLS bundle")
	}
}

func TestBuildResources_EmitsClientAndHMACSecretsWhenPopulated(t *testing.T) {
	d := NewDeployer(nil, logr.Discard(), DefaultConfig(), newTestImageVector(t))
	tls := TLSBundle{
		CertPEM:       []byte("c"),
		KeyPEM:        []byte("k"),
		CAPEM:         []byte("a"),
		ClientCertPEM: []byte("cc"),
		ClientKeyPEM:  []byte("ck"),
		HMACBytes:     []byte("hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh"),
	}
	resources, err := d.buildResources(tls)
	if err != nil {
		t.Fatalf("buildResources returned error: %v", err)
	}
	for _, name := range []string{"secret-tls.yaml", "secret-envoy.yaml", "secret-hmac.yaml"} {
		if _, ok := resources[name]; !ok {
			t.Errorf("expected %q in resources when full TLS bundle is supplied", name)
		}
	}
}

func TestHMACSecretConfig_GeneratesRandom32ByteKey(t *testing.T) {
	cfg := &HMACSecretConfig{Name: HMACSecretName}

	a, err := cfg.Generate()
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	b, err := cfg.Generate()
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}

	keyA := a.SecretData()[HMACSecretKey]
	keyB := b.SecretData()[HMACSecretKey]

	if len(keyA) != 32 {
		t.Fatalf("expected 32-byte HMAC key, got %d bytes", len(keyA))
	}

	if string(keyA) == string(keyB) {
		t.Fatal("HMACSecretConfig.Generate returned the same key twice; key must be random")
	}
}

func TestBuildResources_ManageCRDsFalseOmitsCRDs(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ManageCRDs = false
	d := NewDeployer(nil, logr.Discard(), cfg, newTestImageVector(t))
	resources, err := d.GenerateResources()
	if err != nil {
		t.Fatalf("GenerateResources returned error: %v", err)
	}
	for name := range resources {
		if strings.HasPrefix(name, "crd-") {
			t.Errorf("did not expect any CRD resource with manageCRDs=false, found %q", name)
		}
	}
	// The GatewayClass and workload must still be delivered.
	if _, ok := resources["gatewayclass.yaml"]; !ok {
		t.Error("expected gatewayclass.yaml even with manageCRDs=false")
	}
}

func TestBuildResources_ManageCRDsTrueEmitsCRDs(t *testing.T) {
	d := NewDeployer(nil, logr.Discard(), DefaultConfig(), newTestImageVector(t))
	resources, err := d.GenerateResources()
	if err != nil {
		t.Fatalf("GenerateResources returned error: %v", err)
	}
	var found bool
	for name := range resources {
		if strings.HasPrefix(name, "crd-") {
			found = true

			break
		}
	}
	if !found {
		t.Error("expected at least one CRD resource with the default manageCRDs=true")
	}
}

func TestDeployment_HonorsControlPlaneReplicas(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ControlPlaneReplicas = ptr.To[int32](3)
	d := NewDeployer(nil, logr.Discard(), cfg, newTestImageVector(t))
	deploy, err := d.deployment()
	if err != nil {
		t.Fatalf("deployment returned error: %v", err)
	}
	if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 3 {
		t.Fatalf("expected 3 control-plane replicas, got %v", deploy.Spec.Replicas)
	}
}

func TestEnvoyProxyGuard_ProducesPolicyAndBinding(t *testing.T) {
	policy, binding := envoyProxyGuard()
	if policy == nil || binding == nil {
		t.Fatal("expected both a policy and a binding")
	}
	if policy.Kind != "ValidatingAdmissionPolicy" {
		t.Errorf("expected policy kind ValidatingAdmissionPolicy, got %q", policy.Kind)
	}
	if binding.Kind != "ValidatingAdmissionPolicyBinding" {
		t.Errorf("expected binding kind ValidatingAdmissionPolicyBinding, got %q", binding.Kind)
	}
	// Six guarded fields on two workloads → twelve validations.
	if got := len(policy.Spec.Validations); got != 12 {
		t.Errorf("expected 12 validations, got %d", got)
	}
	// FailurePolicy must be Fail: a Fail-open policy would silently admit a
	// malicious EnvoyProxy whenever the CEL evaluation errors.
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Errorf("expected FailurePolicy Fail, got %v", policy.Spec.FailurePolicy)
	}
	// The binding must Deny: a binding that only Warns/Audits leaves the guard
	// toothless and still admits the resource.
	if want := []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}; !reflect.DeepEqual(binding.Spec.ValidationActions, want) {
		t.Errorf("expected validationActions %v, got %v", want, binding.Spec.ValidationActions)
	}
	if binding.Spec.PolicyName != policy.Name {
		t.Errorf("binding policyName %q does not match policy name %q", binding.Spec.PolicyName, policy.Name)
	}
	// The binding must exempt kube-system via a NotIn namespaceSelector: that is
	// where our own control plane lives and where the policy would otherwise
	// reject the EnvoyProxy we deploy ourselves. A string-contains test cannot
	// tell NotIn from In, so assert the operator and values structurally.
	if binding.Spec.MatchResources == nil || binding.Spec.MatchResources.NamespaceSelector == nil {
		t.Fatal("expected binding to carry a namespaceSelector")
	}
	exprs := binding.Spec.MatchResources.NamespaceSelector.MatchExpressions
	if len(exprs) != 1 {
		t.Fatalf("expected exactly one namespaceSelector expression, got %d", len(exprs))
	}
	if exprs[0].Key != "kubernetes.io/metadata.name" {
		t.Errorf("expected namespaceSelector key kubernetes.io/metadata.name, got %q", exprs[0].Key)
	}
	if exprs[0].Operator != metav1.LabelSelectorOpNotIn {
		t.Errorf("expected namespaceSelector operator NotIn, got %q", exprs[0].Operator)
	}
	if !reflect.DeepEqual(exprs[0].Values, []string{Namespace}) {
		t.Errorf("expected namespaceSelector values %v, got %v", []string{Namespace}, exprs[0].Values)
	}
}

func TestFieldAbsentExpression(t *testing.T) {
	// Order matters: the null-guard on the workload variable must come first so
	// the later !has(...) terms never dereference a null, and each nested path
	// segment must be guarded before the next is accessed.
	for _, tc := range []struct {
		workload string
		field    string
		want     string
	}{
		{
			workload: "deploy",
			field:    "patch",
			want:     "variables.deploy == null || !has(variables.deploy.patch)",
		},
		{
			workload: "daemonset",
			field:    "pod.volumes",
			want:     "variables.daemonset == null || !has(variables.daemonset.pod) || !has(variables.daemonset.pod.volumes)",
		},
		{
			workload: "deploy",
			field:    "container.securityContext",
			want:     "variables.deploy == null || !has(variables.deploy.container) || !has(variables.deploy.container.securityContext)",
		},
	} {
		if got := fieldAbsentExpression(tc.workload, tc.field); got != tc.want {
			t.Errorf("fieldAbsentExpression(%q, %q) =\n  %q\nwant\n  %q", tc.workload, tc.field, got, tc.want)
		}
	}
}

func TestEnvoyProxyDefaultsYAML_RendersDataPlaneKnobs(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DataPlaneReplicas = ptr.To[int32](4)
	cfg.DataPlaneLogLevel = "debug"
	cfg.EnvoyProxyDefaults = &config.EnvoyProxyDefaults{
		AccessLogging: true,
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}
	d := NewDeployer(nil, logr.Discard(), cfg, newTestImageVector(t))
	out := d.envoyProxyDefaultsYAML()

	for _, want := range []string{
		"replicas: 4",
		"default: debug",
		"accessLog:",
		"container:",
		"cpu: 100m",
		"networking.gardener.cloud/to-dns: allowed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected EnvoyProxy defaults YAML to contain %q\n---\n%s", want, out)
		}
	}
}
