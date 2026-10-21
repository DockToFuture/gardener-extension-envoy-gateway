// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package netpolcontroller

import (
	"context"
	"slices"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestMapGatewayToRequests(t *testing.T) {
	r := &Reconciler{}
	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: testNsGateway}}

	got := requestNamespaces(r.mapGatewayToRequests(context.Background(), gw))
	if want := []string{testNsGateway}; !slices.Equal(got, want) {
		t.Fatalf("mapGatewayToRequests() = %v, want %v", got, want)
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
				Labels:    map[string]string{envoygateway.LabelHop: string(envoygateway.HopProxyIngress)},
			}},
			want: []string{testNsGateway},
		},
		{
			name: "hop-2 proxy egress maps to its own namespace",
			policy: &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
				Name:      envoygateway.ProxyEgressNetworkPolicyName,
				Namespace: testNsGateway,
				Labels:    map[string]string{envoygateway.LabelHop: string(envoygateway.HopProxyEgress)},
			}},
			want: []string{testNsGateway},
		},
		{
			name: "a policy without a managed hop label yields nothing",
			policy: &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
				Name:      "something-else",
				Namespace: testNsGateway,
				Labels:    map[string]string{},
			}},
			want: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := requestNamespaces(mapManagedPolicyToRequests(context.Background(), tc.policy))
			if !slices.Equal(got, tc.want) {
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

func TestManagedPolicyPredicate(t *testing.T) {
	p := managedPolicyPredicate()

	managed := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		envoygateway.LabelManagedBy: envoygateway.LabelManagedByValue,
		envoygateway.LabelHop:       string(envoygateway.HopProxyIngress),
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
