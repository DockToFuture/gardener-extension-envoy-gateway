// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package netpolcontroller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

// requestFor returns a reconcile.Request keyed by a Gateway namespace. The name
// is intentionally empty: the controller key is the namespace, not an object.
func requestFor(gatewayNamespace string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: gatewayNamespace}}
}

// mapGatewayToRequests enqueues a Gateway's own namespace. The managed-Gateway
// predicate has already filtered to our GatewayClass.
func (r *Reconciler) mapGatewayToRequests(_ context.Context, gw *gatewayv1.Gateway) []reconcile.Request {
	return []reconcile.Request{requestFor(gw.Namespace)}
}

// parentNamespaces returns the distinct namespaces of the Gateways a route
// attaches to via its parentRefs. An unset parentRef namespace defaults to the
// route's own namespace (same-namespace attachment, the common case).
func parentNamespaces(parentRefs []gatewayv1.ParentReference, routeNamespace string) []reconcile.Request {
	seen := map[string]struct{}{}
	var reqs []reconcile.Request
	for _, p := range parentRefs {
		// Only Gateway parents matter; a route may also attach to a Service
		// (mesh), which is irrelevant to data-plane proxy policies.
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		ns := routeNamespace
		if p.Namespace != nil {
			ns = string(*p.Namespace)
		}
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		reqs = append(reqs, requestFor(ns))
	}

	return reqs
}

// routeAttachesToNamespace reports whether a route (living in routeNamespace)
// has a Gateway parentRef resolving to gatewayNamespace. Used by the reconciler
// to filter the routes it lists down to those attached to the key's namespace.
func routeAttachesToNamespace(parentRefs []gatewayv1.ParentReference, routeNamespace, gatewayNamespace string) bool {
	for _, p := range parentRefs {
		if p.Kind != nil && string(*p.Kind) != "Gateway" {
			continue
		}
		ns := routeNamespace
		if p.Namespace != nil {
			ns = string(*p.Namespace)
		}
		if ns == gatewayNamespace {
			return true
		}
	}

	return false
}

func mapHTTPRouteToRequests(_ context.Context, rt *gatewayv1.HTTPRoute) []reconcile.Request {
	return parentNamespaces(rt.Spec.ParentRefs, rt.Namespace)
}

func mapGRPCRouteToRequests(_ context.Context, rt *gatewayv1.GRPCRoute) []reconcile.Request {
	return parentNamespaces(rt.Spec.ParentRefs, rt.Namespace)
}

func mapTCPRouteToRequests(_ context.Context, rt *gatewayv1alpha2.TCPRoute) []reconcile.Request {
	return parentNamespaces(rt.Spec.ParentRefs, rt.Namespace)
}

func mapUDPRouteToRequests(_ context.Context, rt *gatewayv1alpha2.UDPRoute) []reconcile.Request {
	return parentNamespaces(rt.Spec.ParentRefs, rt.Namespace)
}

func mapTLSRouteToRequests(_ context.Context, rt *gatewayv1alpha2.TLSRoute) []reconcile.Request {
	return parentNamespaces(rt.Spec.ParentRefs, rt.Namespace)
}

// mapServiceToRequests fans a Service change out to its own namespace (a
// same-namespace backend) and, conservatively, to every Gateway namespace: a
// Service's selector change can affect a hop-3 policy in its own namespace and
// a hop-2 egress rule in any Gateway namespace whose route targets it. Per-shoot
// Gateway counts are small, so the over-enqueue is cheap and keeps the mapping
// free of a cross-namespace grant lookup on the event path.
func (r *Reconciler) mapServiceToRequests(ctx context.Context, svc *corev1.Service) []reconcile.Request {
	reqs := []reconcile.Request{requestFor(svc.Namespace)}

	gateways := &gatewayv1.GatewayList{}
	if err := r.ShootClient.List(ctx, gateways); err != nil {
		return reqs
	}
	seen := map[string]struct{}{svc.Namespace: {}}
	for _, gw := range gateways.Items {
		if string(gw.Spec.GatewayClassName) != envoygateway.GatewayClassName {
			continue
		}
		if _, ok := seen[gw.Namespace]; ok {
			continue
		}
		seen[gw.Namespace] = struct{}{}
		reqs = append(reqs, requestFor(gw.Namespace))
	}

	return reqs
}

// mapReferenceGrantToRequests enqueues the Gateway namespaces named in a
// ReferenceGrant's From entries: granting (or revoking) a cross-namespace
// reference can newly allow or deny a backend for routes in those namespaces.
func (r *Reconciler) mapReferenceGrantToRequests(_ context.Context, rg *gatewayv1.ReferenceGrant) []reconcile.Request {
	seen := map[string]struct{}{}
	var reqs []reconcile.Request
	for _, from := range rg.Spec.From {
		ns := string(from.Namespace)
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		reqs = append(reqs, requestFor(ns))
	}

	return reqs
}

// mapManagedPolicyToRequests maps one of our own managed NetworkPolicies back to
// the Gateway namespace(s) that must reconcile to restore it. Hop-1 and hop-2
// policies live in the Gateway namespace, so that namespace is the key. A hop-3
// policy lives in a backend namespace; its ingress peer names the Gateway
// namespace via the metadata.name namespaceSelector, which we read back here.
func mapManagedPolicyToRequests(_ context.Context, np *networkingv1.NetworkPolicy) []reconcile.Request {
	switch np.Labels[envoygateway.LabelHop] {
	case envoygateway.HopProxyIngress, envoygateway.HopProxyEgress:
		return []reconcile.Request{requestFor(np.Namespace)}
	case envoygateway.HopBackendIngress:
		for _, rule := range np.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.NamespaceSelector == nil {
					continue
				}
				if ns, ok := peer.NamespaceSelector.MatchLabels[envoygateway.MetadataNameLabel]; ok && ns != "" {
					return []reconcile.Request{requestFor(ns)}
				}
			}
		}
	default:
		// Not one of our managed hops; nothing to enqueue.
	}

	return nil
}

// managedGatewayPredicate restricts the Gateway watch to Gateways bound to this
// extension's GatewayClass.
func managedGatewayPredicate() predicate.TypedPredicate[*gatewayv1.Gateway] {
	isManaged := func(gw *gatewayv1.Gateway) bool {
		return gw != nil && string(gw.Spec.GatewayClassName) == envoygateway.GatewayClassName
	}

	return predicate.TypedFuncs[*gatewayv1.Gateway]{
		CreateFunc:  func(e event.TypedCreateEvent[*gatewayv1.Gateway]) bool { return isManaged(e.Object) },
		UpdateFunc:  func(e event.TypedUpdateEvent[*gatewayv1.Gateway]) bool { return isManaged(e.ObjectNew) },
		DeleteFunc:  func(e event.TypedDeleteEvent[*gatewayv1.Gateway]) bool { return isManaged(e.Object) },
		GenericFunc: func(e event.TypedGenericEvent[*gatewayv1.Gateway]) bool { return isManaged(e.Object) },
	}
}

// serviceSelectorChangedPredicate triggers on Service create/delete and on
// updates that change spec.selector. A Service's selector is a spec field, not
// a label, so the generic label-change predicates do not catch it.
func serviceSelectorChangedPredicate() predicate.TypedPredicate[*corev1.Service] {
	return predicate.TypedFuncs[*corev1.Service]{
		CreateFunc: func(e event.TypedCreateEvent[*corev1.Service]) bool { return true },
		DeleteFunc: func(e event.TypedDeleteEvent[*corev1.Service]) bool { return true },
		UpdateFunc: func(e event.TypedUpdateEvent[*corev1.Service]) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}

			return !selectorsEqual(e.ObjectOld.Spec.Selector, e.ObjectNew.Spec.Selector)
		},
		GenericFunc: func(e event.TypedGenericEvent[*corev1.Service]) bool { return false },
	}
}

// managedPolicyPredicate restricts the self-heal NetworkPolicy watch to the
// policies this extension manages, and ignores pure create events: a create is
// almost always our own CreateOrUpdate write, so reconciling on it would loop.
// Deletes and spec-changing updates are what we must heal.
func managedPolicyPredicate() predicate.TypedPredicate[*networkingv1.NetworkPolicy] {
	isManaged := func(np *networkingv1.NetworkPolicy) bool {
		return np != nil && np.Labels[envoygateway.LabelManagedBy] == envoygateway.LabelManagedByValue && np.Labels[envoygateway.LabelHop] != ""
	}

	return predicate.TypedFuncs[*networkingv1.NetworkPolicy]{
		CreateFunc: func(e event.TypedCreateEvent[*networkingv1.NetworkPolicy]) bool { return false },
		DeleteFunc: func(e event.TypedDeleteEvent[*networkingv1.NetworkPolicy]) bool { return isManaged(e.Object) },
		UpdateFunc: func(e event.TypedUpdateEvent[*networkingv1.NetworkPolicy]) bool { return isManaged(e.ObjectNew) },
		GenericFunc: func(e event.TypedGenericEvent[*networkingv1.NetworkPolicy]) bool {
			return isManaged(e.Object)
		},
	}
}

// selectorsEqual reports whether two string maps are equal.
func selectorsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}

	return true
}
