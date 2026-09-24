// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubernetesscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

// ownedBy returns the labels envoy-gateway stamps on a per-Gateway data-plane
// object owned by the given Gateway.
func ownedBy(gwNamespace, gwName string) map[string]string {
	return map[string]string{
		envoygateway.LabelManagedBy:              envoygateway.EnvoyProxyManagedByValue,
		envoygateway.LabelName:                   envoygateway.EnvoyProxyNameValue,
		envoygateway.OwningGatewayNamespaceLabel: gwNamespace,
		envoygateway.OwningGatewayNameLabel:      gwName,
	}
}

func TestIsOrphanedDataPlaneObject(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name:   "per-gateway data-plane object is orphaned",
			labels: ownedBy(nsTeamA, "gw1"),
			want:   true,
		},
		{
			name: "control-plane object without owning-gateway label is kept",
			labels: map[string]string{
				envoygateway.LabelManagedBy: envoygateway.EnvoyProxyManagedByValue,
				envoygateway.LabelName:      envoygateway.DeploymentName,
			},
			want: false,
		},
		{
			name:   "owning-gateway-name present with empty value still counts as owned",
			labels: map[string]string{envoygateway.OwningGatewayNameLabel: ""},
			want:   true,
		},
		{
			name:   "no labels is kept",
			labels: nil,
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Labels: tt.labels}}
			if got := isOrphanedDataPlaneObject(obj); got != tt.want {
				t.Errorf("isOrphanedDataPlaneObject() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSweepOrphans_DeletesOnlyDataPlaneObjects verifies the sweep deletes the
// per-Gateway data-plane bundle in kube-system while leaving the control-plane
// objects and objects in other namespaces untouched, and that a second run is a
// no-op (idempotent).
func TestSweepOrphans_DeletesOnlyDataPlaneObjects(t *testing.T) {
	const (
		ns          = envoygateway.Namespace // kube-system
		orphanName  = "envoy-team-a-gw1-abcde"
		newModeName = "envoy-team-b-gw2-fghij"
	)

	orphanSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: orphanName, Namespace: ns, Labels: ownedBy(nsTeamA, "gw1")}}
	orphanDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: orphanName, Namespace: ns, Labels: ownedBy(nsTeamA, "gw1")}}
	orphanSA := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: orphanName, Namespace: ns, Labels: ownedBy(nsTeamA, "gw1")}}
	orphanCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: orphanName, Namespace: ns, Labels: ownedBy(nsTeamA, "gw1")}}
	orphanPDB := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: orphanName, Namespace: ns, Labels: ownedBy(nsTeamA, "gw1")}}

	// Control-plane objects: same managed-by label, but no owning-gateway label.
	cpDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: envoygateway.DeploymentName, Namespace: ns, Labels: map[string]string{
		envoygateway.LabelManagedBy: envoygateway.EnvoyProxyManagedByValue,
	}}}
	cpSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: envoygateway.DeploymentName, Namespace: ns, Labels: map[string]string{
		envoygateway.LabelManagedBy: envoygateway.EnvoyProxyManagedByValue,
	}}}

	// A data-plane object already living in the Gateway's own namespace (the new
	// mode): the sweep is scoped to kube-system, so it must be left alone.
	newModeSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: newModeName, Namespace: nsTeamB, Labels: ownedBy(nsTeamB, "gw2")}}

	c := fake.NewClientBuilder().WithScheme(kubernetesscheme.Scheme).WithObjects(
		orphanSvc, orphanDeploy, orphanSA, orphanCM, orphanPDB,
		cpDeploy, cpSvc, newModeSvc,
	).Build()

	ctx := context.Background()
	if err := sweepOrphans(ctx, logr.Discard(), c); err != nil {
		t.Fatalf("sweepOrphans returned error: %v", err)
	}

	// Orphans in kube-system must be gone.
	for _, o := range []client.Object{orphanSvc, orphanDeploy, orphanSA, orphanCM, orphanPDB} {
		err := c.Get(ctx, types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}, o)
		if !apierrors.IsNotFound(err) {
			t.Errorf("expected orphan %T %s to be deleted, got err=%v", o, o.GetName(), err)
		}
	}

	// Control-plane and other-namespace objects must survive.
	for _, o := range []client.Object{cpDeploy, cpSvc, newModeSvc} {
		if err := c.Get(ctx, types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}, o); err != nil {
			t.Errorf("expected %T %s/%s to survive the sweep, got err=%v", o, o.GetNamespace(), o.GetName(), err)
		}
	}

	// Second run is a no-op and must not error.
	if err := sweepOrphans(ctx, logr.Discard(), c); err != nil {
		t.Fatalf("second sweepOrphans run returned error: %v", err)
	}
}
