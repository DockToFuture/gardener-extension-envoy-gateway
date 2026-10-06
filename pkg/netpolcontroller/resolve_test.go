// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package netpolcontroller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

func resolveScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("failed to add client-go scheme: %v", err)
	}
	if err := gatewayv1.Install(s); err != nil {
		t.Fatalf("failed to add gateway-api v1 scheme: %v", err)
	}

	return s
}

func svc(ns, name string, selector map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.ServiceSpec{Selector: selector},
	}
}

func externalNameSvc(ns, name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "example.com"},
	}
}

func backendRef(ns, name string, kind, group *string) gatewayv1.BackendObjectReference {
	ref := gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(name)}
	if ns != "" {
		n := gatewayv1.Namespace(ns)
		ref.Namespace = &n
	}
	if kind != nil {
		k := gatewayv1.Kind(*kind)
		ref.Kind = &k
	}
	if group != nil {
		g := gatewayv1.Group(*group)
		ref.Group = &g
	}

	return ref
}

func refGrant(ns string, fromKind, fromNs string, toServiceName *string) *gatewayv1.ReferenceGrant {
	to := gatewayv1.ReferenceGrantTo{Group: "", Kind: "Service"}
	if toServiceName != nil {
		n := gatewayv1.ObjectName(*toServiceName)
		to.Name = &n
	}

	return &gatewayv1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: testGrantName},
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{
				Group:     envoygateway.APIGroupGatewayAPI,
				Kind:      gatewayv1.Kind(fromKind),
				Namespace: gatewayv1.Namespace(fromNs),
			}},
			To: []gatewayv1.ReferenceGrantTo{to},
		},
	}
}

func resolve(t *testing.T, objs []client.Object, route RouteInfo) ([]envoygateway.BackendTarget, []SkipReason) {
	t.Helper()
	c := fakeclient.NewClientBuilder().WithScheme(resolveScheme(t)).WithObjects(objs...).Build()
	targets, skips, err := ResolveBackends(context.Background(), c, route)
	if err != nil {
		t.Fatalf("ResolveBackends returned error: %v", err)
	}

	return targets, skips
}

func TestResolveBackends_SameNamespace(t *testing.T) {
	targets, skips := resolve(t,
		[]client.Object{svc(testAppLabel, testBackendName, map[string]string{testAppLabel: testBackendName})},
		RouteInfo{Namespace: testAppLabel, BackendRefs: []gatewayv1.BackendObjectReference{backendRef("", testBackendName, nil, nil)}},
	)
	if len(skips) != 0 {
		t.Fatalf("expected no skips, got %v", skips)
	}
	if len(targets) != 1 || targets[0].Namespace != testAppLabel || targets[0].PodSelector[testAppLabel] != testBackendName {
		t.Fatalf("expected one same-ns target with the Service selector, got %v", targets)
	}
}

func TestResolveBackends_CrossNamespaceWithoutGrant(t *testing.T) {
	targets, skips := resolve(t,
		[]client.Object{svc(testNsOther, testBackendName, map[string]string{testAppLabel: "x"})},
		RouteInfo{Namespace: testAppLabel, BackendRefs: []gatewayv1.BackendObjectReference{backendRef(testNsOther, testBackendName, nil, nil)}},
	)
	if len(targets) != 0 {
		t.Fatalf("expected no targets without a ReferenceGrant, got %v", targets)
	}
	if len(skips) != 1 || skips[0].Reason != ReasonNoReferenceGrant {
		t.Fatalf("expected a NoReferenceGrant skip, got %v", skips)
	}
}

func TestResolveBackends_CrossNamespaceWithGrant(t *testing.T) {
	targets, skips := resolve(t,
		[]client.Object{
			svc(testNsOther, testBackendName, map[string]string{testAppLabel: "x"}),
			refGrant(testNsOther, testKindHTTPRoute, testAppLabel, nil),
		},
		RouteInfo{Namespace: testAppLabel, BackendRefs: []gatewayv1.BackendObjectReference{backendRef(testNsOther, testBackendName, nil, nil)}},
	)
	if len(skips) != 0 {
		t.Fatalf("expected no skips with a matching grant, got %v", skips)
	}
	if len(targets) != 1 || targets[0].Namespace != testNsOther {
		t.Fatalf("expected one cross-ns target, got %v", targets)
	}
}

func TestResolveBackends_GrantScopedToOtherService(t *testing.T) {
	// A grant that names a different Service must not permit this ref.
	_, skips := resolve(t,
		[]client.Object{
			svc(testNsOther, testBackendName, map[string]string{testAppLabel: "x"}),
			refGrant(testNsOther, testKindHTTPRoute, testAppLabel, new("different")),
		},
		RouteInfo{Namespace: testAppLabel, BackendRefs: []gatewayv1.BackendObjectReference{backendRef(testNsOther, testBackendName, nil, nil)}},
	)
	if len(skips) != 1 || skips[0].Reason != ReasonNoReferenceGrant {
		t.Fatalf("expected a NoReferenceGrant skip for a name-scoped grant to a different Service, got %v", skips)
	}
}

func TestResolveBackends_GrantScopedToThisService(t *testing.T) {
	targets, skips := resolve(t,
		[]client.Object{
			svc(testNsOther, testBackendName, map[string]string{testAppLabel: "x"}),
			refGrant(testNsOther, testKindHTTPRoute, testAppLabel, new(testBackendName)),
		},
		RouteInfo{Namespace: testAppLabel, BackendRefs: []gatewayv1.BackendObjectReference{backendRef(testNsOther, testBackendName, nil, nil)}},
	)
	if len(skips) != 0 || len(targets) != 1 {
		t.Fatalf("expected the name-scoped grant to permit its Service, got targets=%v skips=%v", targets, skips)
	}
}

func TestResolveBackends_SelectorlessSkipped(t *testing.T) {
	_, skips := resolve(t,
		[]client.Object{svc(testAppLabel, "headless", nil)},
		RouteInfo{Namespace: testAppLabel, BackendRefs: []gatewayv1.BackendObjectReference{backendRef("", "headless", nil, nil)}},
	)
	if len(skips) != 1 || skips[0].Reason != ReasonSelectorless {
		t.Fatalf("expected a SelectorlessService skip, got %v", skips)
	}
}

func TestResolveBackends_ExternalNameSkipped(t *testing.T) {
	_, skips := resolve(t,
		[]client.Object{externalNameSvc(testAppLabel, "ext")},
		RouteInfo{Namespace: testAppLabel, BackendRefs: []gatewayv1.BackendObjectReference{backendRef("", "ext", nil, nil)}},
	)
	if len(skips) != 1 || skips[0].Reason != ReasonSelectorless {
		t.Fatalf("expected an ExternalName Service to be skipped as selector-less, got %v", skips)
	}
}

func TestResolveBackends_NonServiceSkipped(t *testing.T) {
	_, skips := resolve(t,
		nil,
		RouteInfo{Namespace: testAppLabel, BackendRefs: []gatewayv1.BackendObjectReference{
			backendRef("", "my-backend", new("Backend"), new("gateway.envoyproxy.io")),
		}},
	)
	if len(skips) != 1 || skips[0].Reason != ReasonNotService {
		t.Fatalf("expected a NotService skip, got %v", skips)
	}
}

func TestResolveBackends_MissingServiceSkipped(t *testing.T) {
	_, skips := resolve(t,
		nil,
		RouteInfo{Namespace: testAppLabel, BackendRefs: []gatewayv1.BackendObjectReference{backendRef("", "ghost", nil, nil)}},
	)
	if len(skips) != 1 || skips[0].Reason != ReasonServiceNotFound {
		t.Fatalf("expected a ServiceNotFound skip, got %v", skips)
	}
}

func TestResolveBackends_Dedupe(t *testing.T) {
	targets, skips := resolve(t,
		[]client.Object{svc(testAppLabel, testBackendName, map[string]string{testAppLabel: testBackendName})},
		RouteInfo{Namespace: testAppLabel, BackendRefs: []gatewayv1.BackendObjectReference{
			backendRef("", testBackendName, nil, nil),
			backendRef(testAppLabel, testBackendName, nil, nil),
		}},
	)
	if len(skips) != 0 {
		t.Fatalf("expected no skips, got %v", skips)
	}
	if len(targets) != 1 {
		t.Fatalf("expected duplicate backendRefs to collapse to one target, got %v", targets)
	}
}
