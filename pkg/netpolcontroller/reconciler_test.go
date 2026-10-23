// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package netpolcontroller

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

func reconcilerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("failed to add client-go scheme: %v", err)
	}
	if err := gatewayv1.Install(s); err != nil {
		t.Fatalf("failed to add gateway-api scheme: %v", err)
	}

	return s
}

func managedGateway() *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: testNsGateway},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: envoygateway.GatewayClassName},
	}
}

// getPolicy fetches a NetworkPolicy in the Gateway namespace, returning nil if
// it does not exist.
func getPolicy(t *testing.T, cl client.Client, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	np := &networkingv1.NetworkPolicy{}
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNsGateway, Name: name}, np)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("unexpected error getting NetworkPolicy %s/%s: %v", testNsGateway, name, err)
	}

	return np
}

// A managed Gateway in the namespace yields the hop-1 ingress policy and the
// hop-2 egress policy (DNS + xDS only — no route-derived backend rules).
func TestReconcileWritesIngressAndEgress(t *testing.T) {
	scheme := reconcilerTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(managedGateway()).Build()

	r := &Reconciler{ShootClient: cl}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: nsName()}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	// Hop 1 (ingress) in the Gateway namespace.
	if np := getPolicy(t, cl, envoygateway.DataPlaneNetworkPolicyName); np == nil {
		t.Fatal("hop-1 proxy ingress policy was not created")
	}

	// Hop 2 (egress) in the Gateway namespace: exactly DNS + xDS.
	egress := getPolicy(t, cl, envoygateway.ProxyEgressNetworkPolicyName)
	if egress == nil {
		t.Fatal("hop-2 proxy egress policy was not created")
	}
	if len(egress.Spec.Egress) != 2 {
		t.Fatalf("expected 2 egress rules (DNS + xDS), got %d", len(egress.Spec.Egress))
	}
}

func TestReconcilePrunesNamespaceWithoutGateway(t *testing.T) {
	scheme := reconcilerTestScheme(t)

	// Pre-seed both hops as if a Gateway previously existed, but no Gateway
	// object now lives in gw-ns.
	hop1 := envoygateway.DataPlaneProxyIngressPolicy(testNsGateway)
	hop2 := envoygateway.ProxyEgressPolicy(testNsGateway)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hop1, hop2).Build()
	r := &Reconciler{ShootClient: cl}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: nsName()}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	if np := getPolicy(t, cl, envoygateway.DataPlaneNetworkPolicyName); np != nil {
		t.Error("hop-1 policy should have been pruned")
	}
	if np := getPolicy(t, cl, envoygateway.ProxyEgressNetworkPolicyName); np != nil {
		t.Error("hop-2 policy should have been pruned")
	}
}

func TestReconcileSelfHealRecreatesDeletedPolicy(t *testing.T) {
	scheme := reconcilerTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(managedGateway()).Build()

	r := &Reconciler{ShootClient: cl}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nsName()}); err != nil {
		t.Fatalf("initial reconcile failed: %v", err)
	}

	// A user deletes the hop-1 policy. A second reconcile (what the self-heal
	// watch triggers) must recreate it.
	if err := cl.Delete(ctx, getPolicy(t, cl, envoygateway.DataPlaneNetworkPolicyName)); err != nil {
		t.Fatalf("failed to delete hop-1 policy: %v", err)
	}
	if np := getPolicy(t, cl, envoygateway.DataPlaneNetworkPolicyName); np != nil {
		t.Fatal("precondition: hop-1 policy should be gone before the self-heal reconcile")
	}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nsName()}); err != nil {
		t.Fatalf("self-heal reconcile failed: %v", err)
	}
	if np := getPolicy(t, cl, envoygateway.DataPlaneNetworkPolicyName); np == nil {
		t.Fatal("hop-1 policy was not recreated by the self-heal reconcile")
	}
}

func nsName() types.NamespacedName {
	return types.NamespacedName{Namespace: testNsGateway}
}
