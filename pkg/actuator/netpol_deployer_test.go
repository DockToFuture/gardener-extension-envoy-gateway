// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"testing"

	extensionscontroller "github.com/gardener/gardener/extensions/pkg/controller"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardenerutils "github.com/gardener/gardener/pkg/utils/gardener"
	"github.com/gardener/gardener/pkg/utils/imagevector"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

func netpolTestImageVector(t *testing.T) imagevector.ImageVector {
	t.Helper()
	iv, err := imagevector.Read([]byte(`---
images:
- name: envoy-gateway
  repository: docker.io/envoyproxy/gateway
  tag: "v1.8.3"
- name: gardener-extension-envoy-gateway
  repository: docker.io/gardener/gardener-extension-envoy-gateway
  tag: "test"
`))
	if err != nil {
		t.Fatalf("failed to construct test image vector: %v", err)
	}

	return iv
}

// netpolTestExtensionImage is the extension's own image in netpolTestImageVector;
// the netpol-controller container must run it (not the upstream envoy-gateway image).
const netpolTestExtensionImage = "docker.io/gardener/gardener-extension-envoy-gateway:test"

func netpolTestCluster() *extensionscontroller.Cluster {
	return &extensionscontroller.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "shoot--proj--name"},
		Shoot:      &gardencorev1beta1.Shoot{},
	}
}

const netpolTestNamespace = "shoot--proj--name"

func newNetpolSeedClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("failed to build scheme: %v", err)
	}

	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func shootAccessSecretName() string {
	return gardenerutils.SecretNamePrefixShootAccess + envoygateway.NetpolControllerShootAccessSecretName
}

func TestNetpolDeployerEnsureCreatesDeploymentAndSecret(t *testing.T) {
	cl := newNetpolSeedClient(t)
	d := NewNetpolControllerDeployer(cl, netpolTestImageVector(t))

	if err := d.Ensure(context.Background(), logr.Discard(), netpolTestCluster(), netpolTestNamespace, false); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}

	deployment := &appsv1.Deployment{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: netpolTestNamespace, Name: envoygateway.NetpolControllerName}, deployment); err != nil {
		t.Fatalf("expected Deployment to exist: %v", err)
	}
	if replicas := deployment.Spec.Replicas; replicas == nil || *replicas != 1 {
		t.Fatalf("expected replicas=1, got %v", replicas)
	}

	// Pod carries the networking labels needed to reach the shoot API server.
	podLabels := deployment.Spec.Template.Labels
	if podLabels[envoygateway.LabelToAllShootsKubeAPIServer] != "allowed" {
		t.Errorf("pod is missing the to-all-shoots-kube-apiserver label: %v", podLabels)
	}

	// The container runs the netpol-controller subcommand without the
	// experimental flag.
	args := deployment.Spec.Template.Spec.Containers[0].Args
	if len(args) == 0 || args[0] != "netpol-controller" {
		t.Fatalf("expected netpol-controller subcommand, got %v", args)
	}
	for _, a := range args {
		if a == "--experimental-channel=true" {
			t.Errorf("did not expect the experimental flag when experimental=false: %v", args)
		}
	}

	// The container must run the extension's OWN image (which carries the
	// netpol-controller subcommand), not the upstream envoy-gateway image.
	if img := deployment.Spec.Template.Spec.Containers[0].Image; img != netpolTestExtensionImage {
		t.Errorf("expected container image %q, got %q", netpolTestExtensionImage, img)
	}

	secret := &corev1.Secret{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: netpolTestNamespace, Name: shootAccessSecretName()}, secret); err != nil {
		t.Fatalf("expected shoot-access secret to exist: %v", err)
	}
}

func TestNetpolDeployerEnsureExperimentalFlag(t *testing.T) {
	cl := newNetpolSeedClient(t)
	d := NewNetpolControllerDeployer(cl, netpolTestImageVector(t))

	if err := d.Ensure(context.Background(), logr.Discard(), netpolTestCluster(), netpolTestNamespace, true); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}

	deployment := &appsv1.Deployment{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: netpolTestNamespace, Name: envoygateway.NetpolControllerName}, deployment); err != nil {
		t.Fatalf("expected Deployment to exist: %v", err)
	}
	found := false
	for _, a := range deployment.Spec.Template.Spec.Containers[0].Args {
		if a == "--experimental-channel=true" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the experimental flag when experimental=true, got %v", deployment.Spec.Template.Spec.Containers[0].Args)
	}
}

func TestNetpolDeployerTeardownRemovesBoth(t *testing.T) {
	existing := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: envoygateway.NetpolControllerName, Namespace: netpolTestNamespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: shootAccessSecretName(), Namespace: netpolTestNamespace}},
	}
	cl := newNetpolSeedClient(t, existing...)
	d := NewNetpolControllerDeployer(cl, netpolTestImageVector(t))

	if err := d.Teardown(context.Background(), logr.Discard(), netpolTestNamespace); err != nil {
		t.Fatalf("Teardown failed: %v", err)
	}

	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: netpolTestNamespace, Name: envoygateway.NetpolControllerName}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected Deployment to be gone, got err=%v", err)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: netpolTestNamespace, Name: shootAccessSecretName()}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected shoot-access secret to be gone, got err=%v", err)
	}
}

func TestNetpolDeployerTeardownIsIdempotent(t *testing.T) {
	cl := newNetpolSeedClient(t)
	d := NewNetpolControllerDeployer(cl, netpolTestImageVector(t))

	// Nothing exists; Teardown must not error.
	if err := d.Teardown(context.Background(), logr.Discard(), netpolTestNamespace); err != nil {
		t.Fatalf("Teardown on absent objects should be a no-op, got %v", err)
	}
}

func TestNetpolDeployerScaleDown(t *testing.T) {
	cl := newNetpolSeedClient(t)
	d := NewNetpolControllerDeployer(cl, netpolTestImageVector(t))

	if err := d.Ensure(context.Background(), logr.Discard(), netpolTestCluster(), netpolTestNamespace, false); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if err := d.ScaleDown(context.Background(), logr.Discard(), netpolTestNamespace); err != nil {
		t.Fatalf("ScaleDown failed: %v", err)
	}

	deployment := &appsv1.Deployment{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: netpolTestNamespace, Name: envoygateway.NetpolControllerName}, deployment); err != nil {
		t.Fatalf("expected Deployment to still exist after scale-down: %v", err)
	}
	if replicas := deployment.Spec.Replicas; replicas == nil || *replicas != 0 {
		t.Fatalf("expected replicas=0 after scale-down, got %v", replicas)
	}
}

func TestNetpolDeployerScaleDownAbsentIsNoop(t *testing.T) {
	cl := newNetpolSeedClient(t)
	d := NewNetpolControllerDeployer(cl, netpolTestImageVector(t))

	if err := d.ScaleDown(context.Background(), logr.Discard(), netpolTestNamespace); err != nil {
		t.Fatalf("ScaleDown on absent Deployment should be a no-op, got %v", err)
	}
}
