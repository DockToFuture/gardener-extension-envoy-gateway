// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package netpolcontroller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

func httpRoute(backendName string, backendNs *string) *gatewayv1.HTTPRoute {
	parent := gatewayv1.ParentReference{Name: gatewayv1.ObjectName("gw")}
	ref := gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{
		BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(backendName)},
	}}
	if backendNs != nil {
		ref.Namespace = nsPtr(*backendNs)
	}

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: testNsGateway},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
			Rules:           []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{ref}}},
		},
	}
}

func selectorService(name, namespace string, selector map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.ServiceSpec{Selector: selector},
	}
}

// getPolicy fetches a NetworkPolicy, returning nil if it does not exist.
func getPolicy(t *testing.T, cl client.Client, namespace, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	np := &networkingv1.NetworkPolicy{}
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, np)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("unexpected error getting NetworkPolicy %s/%s: %v", namespace, name, err)
	}

	return np
}

func TestReconcileSameNamespaceBackend(t *testing.T) {
	scheme := reconcilerTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		managedGateway(),
		httpRoute(testBackendName, nil),
		selectorService(testBackendName, testNsGateway, map[string]string{testAppLabel: testBackendName}),
	).Build()

	r := &Reconciler{ShootClient: cl}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: nsName()}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	// Hop 1 (ingress) in the Gateway namespace.
	if np := getPolicy(t, cl, testNsGateway, envoygateway.DataPlaneNetworkPolicyName); np == nil {
		t.Fatal("hop-1 proxy ingress policy was not created")
	}

	// Hop 2 (egress) in the Gateway namespace: DNS + xDS + one backend rule.
	egress := getPolicy(t, cl, testNsGateway, envoygateway.ProxyEgressNetworkPolicyName)
	if egress == nil {
		t.Fatal("hop-2 proxy egress policy was not created")
	}
	if len(egress.Spec.Egress) != 3 {
		t.Fatalf("expected 3 egress rules (DNS + xDS + 1 backend), got %d", len(egress.Spec.Egress))
	}

	// Hop 3 (ingress) in the backend namespace (== Gateway namespace here).
	hop3Name := envoygateway.BackendIngressPolicyName(testNsGateway)
	hop3 := getPolicy(t, cl, testNsGateway, hop3Name)
	if hop3 == nil {
		t.Fatal("hop-3 backend ingress policy was not created")
	}
	if got := hop3.Spec.PodSelector.MatchLabels[testAppLabel]; got != testBackendName {
		t.Fatalf("hop-3 podSelector = %v, want app=backend", hop3.Spec.PodSelector.MatchLabels)
	}
}

func TestReconcileCrossNamespaceBackendRequiresGrant(t *testing.T) {
	scheme := reconcilerTestScheme(t)
	backendNs := testNsTeamB

	build := func(objs ...client.Object) client.Client {
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	}

	// Capacity 4: three base objects plus the ReferenceGrant appended below.
	baseObjs := make([]client.Object, 0, 4)
	baseObjs = append(baseObjs,
		managedGateway(),
		httpRoute(testBackendName, &backendNs),
		selectorService(testBackendName, backendNs, map[string]string{testAppLabel: testBackendName}),
	)

	// Without a ReferenceGrant the cross-namespace backend is skipped: no hop-3
	// policy, and hop-2 has only the DNS and xDS rules (no backend rule).
	cl := build(baseObjs...)
	r := &Reconciler{ShootClient: cl}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: nsName()}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if np := getPolicy(t, cl, backendNs, envoygateway.BackendIngressPolicyName(testNsGateway)); np != nil {
		t.Fatal("hop-3 policy must not be created without a ReferenceGrant")
	}
	egress := getPolicy(t, cl, testNsGateway, envoygateway.ProxyEgressNetworkPolicyName)
	if egress == nil || len(egress.Spec.Egress) != 2 {
		t.Fatalf("expected hop-2 to carry only the DNS + xDS rules, got %+v", egress)
	}

	// With a matching ReferenceGrant the backend resolves.
	grant := &gatewayv1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: testGrantName, Namespace: backendNs},
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: envoygateway.APIGroupGatewayAPI, Kind: testKindHTTPRoute, Namespace: testNsGateway}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}
	cl = build(append(baseObjs, grant)...)
	r = &Reconciler{ShootClient: cl}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: nsName()}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if np := getPolicy(t, cl, backendNs, envoygateway.BackendIngressPolicyName(testNsGateway)); np == nil {
		t.Fatal("hop-3 policy must be created once a ReferenceGrant permits the backend")
	}
}

func TestReconcilePrunesNamespaceWithoutGateway(t *testing.T) {
	scheme := reconcilerTestScheme(t)

	// Pre-seed all three hops as if a Gateway previously existed, but no Gateway
	// object now lives in gw-ns.
	hop1 := envoygateway.DataPlaneProxyIngressPolicy(testNsGateway)
	hop2 := envoygateway.ProxyEgressPolicy(testNsGateway, nil)
	hop3 := envoygateway.BackendIngressPolicy(testNsBackend, testNsGateway, map[string]string{testAppLabel: testBackendName})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hop1, hop2, hop3).Build()
	r := &Reconciler{ShootClient: cl}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: nsName()}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	if np := getPolicy(t, cl, testNsGateway, envoygateway.DataPlaneNetworkPolicyName); np != nil {
		t.Error("hop-1 policy should have been pruned")
	}
	if np := getPolicy(t, cl, testNsGateway, envoygateway.ProxyEgressNetworkPolicyName); np != nil {
		t.Error("hop-2 policy should have been pruned")
	}
	if np := getPolicy(t, cl, testNsBackend, envoygateway.BackendIngressPolicyName(testNsGateway)); np != nil {
		t.Error("hop-3 policy should have been pruned")
	}
}

func TestReconcileSelfHealRecreatesDeletedPolicy(t *testing.T) {
	scheme := reconcilerTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		managedGateway(),
		httpRoute(testBackendName, nil),
		selectorService(testBackendName, testNsGateway, map[string]string{testAppLabel: testBackendName}),
	).Build()

	r := &Reconciler{ShootClient: cl}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nsName()}); err != nil {
		t.Fatalf("initial reconcile failed: %v", err)
	}

	// A user deletes the hop-1 policy. A second reconcile (what the self-heal
	// watch triggers) must recreate it.
	if err := cl.Delete(ctx, getPolicy(t, cl, testNsGateway, envoygateway.DataPlaneNetworkPolicyName)); err != nil {
		t.Fatalf("failed to delete hop-1 policy: %v", err)
	}
	if np := getPolicy(t, cl, testNsGateway, envoygateway.DataPlaneNetworkPolicyName); np != nil {
		t.Fatal("precondition: hop-1 policy should be gone before the self-heal reconcile")
	}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nsName()}); err != nil {
		t.Fatalf("self-heal reconcile failed: %v", err)
	}
	if np := getPolicy(t, cl, testNsGateway, envoygateway.DataPlaneNetworkPolicyName); np == nil {
		t.Fatal("hop-1 policy was not recreated by the self-heal reconcile")
	}
}

func TestReconcilePrunesStaleBackendOnRouteChange(t *testing.T) {
	scheme := reconcilerTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		managedGateway(),
		httpRoute("backend-a", nil),
		selectorService("backend-a", testNsGateway, map[string]string{testAppLabel: "a"}),
		selectorService("backend-b", testNsGateway, map[string]string{testAppLabel: "b"}),
	).Build()

	r := &Reconciler{ShootClient: cl}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nsName()}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	// Only one hop-3 policy name exists per Gateway namespace; it must be present.
	if np := getPolicy(t, cl, testNsGateway, envoygateway.BackendIngressPolicyName(testNsGateway)); np == nil {
		t.Fatal("hop-3 policy for the single backend should exist")
	}

	// Repoint the route at a backend with no selector service removed — simulate
	// the route losing all backends; hop-3 must be pruned.
	rt := &gatewayv1.HTTPRoute{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: testNsGateway, Name: "route"}, rt); err != nil {
		t.Fatalf("failed to get route: %v", err)
	}
	rt.Spec.Rules = nil
	if err := cl.Update(ctx, rt); err != nil {
		t.Fatalf("failed to update route: %v", err)
	}

	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nsName()}); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	if np := getPolicy(t, cl, testNsGateway, envoygateway.BackendIngressPolicyName(testNsGateway)); np != nil {
		t.Fatal("hop-3 policy should have been pruned after the route lost its backend")
	}
}

func nsName() types.NamespacedName {
	return types.NamespacedName{Namespace: testNsGateway}
}
