// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package netpolcontroller

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

// requestNamespaces extracts and sorts the namespaces from a slice of requests
// so tests can compare the (order-independent) fan-out set.
func requestNamespaces(reqs []reconcile.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Namespace)
	}
	slices.Sort(out)

	return out
}

func kindPtr(k string) *gatewayv1.Kind {
	v := gatewayv1.Kind(k)

	return &v
}

func nsPtr(n string) *gatewayv1.Namespace {
	v := gatewayv1.Namespace(n)

	return &v
}

func TestParentNamespaces(t *testing.T) {
	tests := []struct {
		name       string
		parentRefs []gatewayv1.ParentReference
		routeNs    string
		want       []string
	}{
		{
			name:       "unset namespace defaults to route namespace",
			parentRefs: []gatewayv1.ParentReference{{Name: "gw"}},
			routeNs:    testNsTeamA,
			want:       []string{testNsTeamA},
		},
		{
			name:       "explicit namespace wins",
			parentRefs: []gatewayv1.ParentReference{{Name: "gw", Namespace: nsPtr(testNsGateway)}},
			routeNs:    testNsTeamA,
			want:       []string{testNsGateway},
		},
		{
			name: "non-Gateway parent is ignored",
			parentRefs: []gatewayv1.ParentReference{
				{Name: "svc", Kind: kindPtr("Service")},
				{Name: "gw", Namespace: nsPtr(testNsGateway)},
			},
			routeNs: testNsTeamA,
			want:    []string{testNsGateway},
		},
		{
			name: "duplicate namespaces are deduplicated",
			parentRefs: []gatewayv1.ParentReference{
				{Name: "gw1", Namespace: nsPtr(testNsGateway)},
				{Name: "gw2", Namespace: nsPtr(testNsGateway)},
			},
			routeNs: testNsTeamA,
			want:    []string{testNsGateway},
		},
		{
			name:       "no parentRefs yields no requests",
			parentRefs: nil,
			routeNs:    testNsTeamA,
			want:       []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := requestNamespaces(parentNamespaces(tc.parentRefs, tc.routeNs))
			if !equalStrings(got, tc.want) {
				t.Fatalf("parentNamespaces() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRouteAttachesToNamespace(t *testing.T) {
	tests := []struct {
		name       string
		parentRefs []gatewayv1.ParentReference
		routeNs    string
		gatewayNs  string
		want       bool
	}{
		{
			name:       "same-namespace attachment",
			parentRefs: []gatewayv1.ParentReference{{Name: "gw"}},
			routeNs:    testNsTeamA,
			gatewayNs:  testNsTeamA,
			want:       true,
		},
		{
			name:       "cross-namespace attachment",
			parentRefs: []gatewayv1.ParentReference{{Name: "gw", Namespace: nsPtr(testNsGateway)}},
			routeNs:    testNsTeamA,
			gatewayNs:  testNsGateway,
			want:       true,
		},
		{
			name:       "no attachment to the queried namespace",
			parentRefs: []gatewayv1.ParentReference{{Name: "gw", Namespace: nsPtr(testNsOther)}},
			routeNs:    testNsTeamA,
			gatewayNs:  testNsGateway,
			want:       false,
		},
		{
			name:       "non-Gateway parent does not attach",
			parentRefs: []gatewayv1.ParentReference{{Name: "svc", Kind: kindPtr("Service")}},
			routeNs:    testNsTeamA,
			gatewayNs:  testNsTeamA,
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := routeAttachesToNamespace(tc.parentRefs, tc.routeNs, tc.gatewayNs); got != tc.want {
				t.Fatalf("routeAttachesToNamespace() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMapServiceToRequests(t *testing.T) {
	scheme := watchesTestScheme(t)

	managedGw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: testNsGateway},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: envoygateway.GatewayClassName},
	}
	foreignGw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: testNsOther, Namespace: "other-ns"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "some-other-class"},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(managedGw, foreignGw).Build()
	r := &Reconciler{ShootClient: cl}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: testBackendName, Namespace: testNsTeamA}}
	got := requestNamespaces(r.mapServiceToRequests(context.Background(), svc))

	// Own namespace plus every managed Gateway namespace; the foreign-class
	// Gateway namespace must not appear.
	want := []string{testNsGateway, testNsTeamA}
	if !equalStrings(got, want) {
		t.Fatalf("mapServiceToRequests() = %v, want %v", got, want)
	}
}

func TestMapReferenceGrantToRequests(t *testing.T) {
	r := &Reconciler{}
	rg := &gatewayv1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: testGrantName, Namespace: testNsBackend},
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{
				{Group: envoygateway.APIGroupGatewayAPI, Kind: testKindHTTPRoute, Namespace: testNsGateway},
				{Group: envoygateway.APIGroupGatewayAPI, Kind: "GRPCRoute", Namespace: testNsGateway},
				{Group: envoygateway.APIGroupGatewayAPI, Kind: testKindHTTPRoute, Namespace: testNsTeamB},
			},
		},
	}

	got := requestNamespaces(r.mapReferenceGrantToRequests(context.Background(), rg))
	want := []string{testNsGateway, testNsTeamB}
	if !equalStrings(got, want) {
		t.Fatalf("mapReferenceGrantToRequests() = %v, want %v", got, want)
	}
}

func TestMapManagedPolicyToRequests(t *testing.T) {
	tests := []struct {
		name   string
		policy *networkingv1.NetworkPolicy
		want   []string
	}{
		{
			name: "hop-1 proxy ingress maps to its own namespace",
			policy: &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
				Name:      envoygateway.DataPlaneNetworkPolicyName,
				Namespace: testNsGateway,
				Labels:    map[string]string{envoygateway.LabelHop: envoygateway.HopProxyIngress},
			}},
			want: []string{testNsGateway},
		},
		{
			name: "hop-2 proxy egress maps to its own namespace",
			policy: &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
				Name:      envoygateway.ProxyEgressNetworkPolicyName,
				Namespace: testNsGateway,
				Labels:    map[string]string{envoygateway.LabelHop: envoygateway.HopProxyEgress},
			}},
			want: []string{testNsGateway},
		},
		{
			name: "hop-3 backend ingress maps back to the Gateway namespace from its ingress peer",
			policy: &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      envoygateway.BackendIngressPolicyName(testNsGateway),
					Namespace: testNsBackend,
					Labels:    map[string]string{envoygateway.LabelHop: envoygateway.HopBackendIngress},
				},
				Spec: networkingv1.NetworkPolicySpec{
					Ingress: []networkingv1.NetworkPolicyIngressRule{{
						From: []networkingv1.NetworkPolicyPeer{{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{envoygateway.MetadataNameLabel: testNsGateway},
							},
						}},
					}},
				},
			},
			want: []string{testNsGateway},
		},
		{
			name: "hop-3 without a namespace-named peer yields nothing",
			policy: &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      envoygateway.BackendIngressPolicyName(testNsGateway),
					Namespace: testNsBackend,
					Labels:    map[string]string{envoygateway.LabelHop: envoygateway.HopBackendIngress},
				},
			},
			want: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := requestNamespaces(mapManagedPolicyToRequests(context.Background(), tc.policy))
			if !equalStrings(got, tc.want) {
				t.Fatalf("mapManagedPolicyToRequests() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManagedGatewayPredicate(t *testing.T) {
	p := managedGatewayPredicate()

	managed := &gatewayv1.Gateway{Spec: gatewayv1.GatewaySpec{GatewayClassName: envoygateway.GatewayClassName}}
	foreign := &gatewayv1.Gateway{Spec: gatewayv1.GatewaySpec{GatewayClassName: testNsOther}}

	if !p.Create(event.TypedCreateEvent[*gatewayv1.Gateway]{Object: managed}) {
		t.Error("managed Gateway create should pass")
	}
	if p.Create(event.TypedCreateEvent[*gatewayv1.Gateway]{Object: foreign}) {
		t.Error("foreign-class Gateway create should be filtered out")
	}
	if !p.Delete(event.TypedDeleteEvent[*gatewayv1.Gateway]{Object: managed}) {
		t.Error("managed Gateway delete should pass")
	}
}

func TestServiceSelectorChangedPredicate(t *testing.T) {
	p := serviceSelectorChangedPredicate()

	if !p.Create(event.TypedCreateEvent[*corev1.Service]{Object: &corev1.Service{}}) {
		t.Error("service create should always pass")
	}
	if !p.Delete(event.TypedDeleteEvent[*corev1.Service]{Object: &corev1.Service{}}) {
		t.Error("service delete should always pass")
	}

	oldSvc := &corev1.Service{Spec: corev1.ServiceSpec{Selector: map[string]string{testAppLabel: "a"}}}
	sameSvc := &corev1.Service{Spec: corev1.ServiceSpec{Selector: map[string]string{testAppLabel: "a"}}}
	changedSvc := &corev1.Service{Spec: corev1.ServiceSpec{Selector: map[string]string{testAppLabel: "b"}}}

	if p.Update(event.TypedUpdateEvent[*corev1.Service]{ObjectOld: oldSvc, ObjectNew: sameSvc}) {
		t.Error("unchanged selector should be filtered out")
	}
	if !p.Update(event.TypedUpdateEvent[*corev1.Service]{ObjectOld: oldSvc, ObjectNew: changedSvc}) {
		t.Error("changed selector should pass")
	}
}

func TestManagedPolicyPredicate(t *testing.T) {
	p := managedPolicyPredicate()

	managed := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		envoygateway.LabelManagedBy: envoygateway.LabelManagedByValue,
		envoygateway.LabelHop:       envoygateway.HopProxyIngress,
	}}}
	foreign := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		envoygateway.LabelManagedBy: "someone-else",
	}}}

	// Create is ignored to avoid self-triggering on our own CreateOrUpdate writes.
	if p.Create(event.TypedCreateEvent[*networkingv1.NetworkPolicy]{Object: managed}) {
		t.Error("create of a managed policy must be ignored to avoid a reconcile loop")
	}
	if !p.Delete(event.TypedDeleteEvent[*networkingv1.NetworkPolicy]{Object: managed}) {
		t.Error("delete of a managed policy must trigger self-heal")
	}
	if p.Delete(event.TypedDeleteEvent[*networkingv1.NetworkPolicy]{Object: foreign}) {
		t.Error("delete of a non-managed policy must be ignored")
	}
	if !p.Update(event.TypedUpdateEvent[*networkingv1.NetworkPolicy]{ObjectNew: managed}) {
		t.Error("update of a managed policy must trigger self-heal")
	}
}

func TestSelectorsEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string]string
		want bool
	}{
		{"both nil", nil, nil, true},
		{"equal", map[string]string{"a": "1"}, map[string]string{"a": "1"}, true},
		{"different length", map[string]string{"a": "1"}, map[string]string{"a": "1", "b": "2"}, false},
		{"different value", map[string]string{"a": "1"}, map[string]string{"a": "2"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectorsEqual(tc.a, tc.b); got != tc.want {
				t.Fatalf("selectorsEqual() = %v, want %v", got, tc.want)
			}
		})
	}
}

// watchesTestScheme builds a scheme with core + gateway-api v1 types for the
// fake client used by the mapping tests.
func watchesTestScheme(t *testing.T) *runtime.Scheme {
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

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
