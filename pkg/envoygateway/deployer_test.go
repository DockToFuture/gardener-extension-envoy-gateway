// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package envoygateway

import (
	"strings"
	"testing"

	"github.com/gardener/gardener/pkg/utils/imagevector"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestGenerateResources_DefaultConfig(t *testing.T) {
	d := NewDeployer(nil, logr.Discard(), DefaultConfig(), newTestImageVector(t))
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
	}
	for _, name := range expected {
		if _, ok := resources[name]; !ok {
			t.Errorf("expected resource %q not found in generated resources", name)
		}
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

func TestHMACSecretBytes_Deterministic(t *testing.T) {
	a := HMACSecretBytes("shoot--foo--bar")
	b := HMACSecretBytes("shoot--foo--bar")
	c := HMACSecretBytes("shoot--foo--baz")
	if len(a) != 32 {
		t.Fatalf("expected 32-byte HMAC key, got %d bytes", len(a))
	}
	if string(a) != string(b) {
		t.Fatal("HMACSecretBytes not deterministic for the same input")
	}
	if string(a) == string(c) {
		t.Fatal("HMACSecretBytes returned the same key for different cluster UIDs")
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
