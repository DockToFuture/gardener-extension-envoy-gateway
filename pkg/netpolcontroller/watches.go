// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package netpolcontroller

import (
	"context"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

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

// mapManagedPolicyToRequests maps one of our own managed NetworkPolicies back to
// the Gateway namespace that must reconcile to restore it. Both the hop-1 and
// hop-2 policies live in the Gateway namespace, so that namespace is the key.
func mapManagedPolicyToRequests(_ context.Context, np *networkingv1.NetworkPolicy) []reconcile.Request {
	switch envoygateway.Hop(np.Labels[envoygateway.LabelHop]) {
	case envoygateway.HopProxyIngress, envoygateway.HopProxyEgress:
		return []reconcile.Request{requestFor(np.Namespace)}
	default:
		// Not one of our managed hops; nothing to enqueue.
		return nil
	}
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

// managedPolicyPredicate restricts the self-heal NetworkPolicy watch to the
// policies this extension manages. It accepts creates too: the informer replays
// the shoot's existing managed policies as create events on (re)start, and that
// is the only signal that prunes policies orphaned while the controller was down
// (e.g. a Gateway deleted during the downtime no longer emits a Gateway event).
// Reconciling on our own CreateOrUpdate writes does not loop because the write
// is idempotent — re-applying the same spec produces no further event.
func managedPolicyPredicate() predicate.TypedPredicate[*networkingv1.NetworkPolicy] {
	isManaged := func(np *networkingv1.NetworkPolicy) bool {
		return np != nil && np.Labels[envoygateway.LabelManagedBy] == envoygateway.LabelManagedByValue && np.Labels[envoygateway.LabelHop] != ""
	}

	return predicate.TypedFuncs[*networkingv1.NetworkPolicy]{
		CreateFunc: func(e event.TypedCreateEvent[*networkingv1.NetworkPolicy]) bool { return isManaged(e.Object) },
		DeleteFunc: func(e event.TypedDeleteEvent[*networkingv1.NetworkPolicy]) bool { return isManaged(e.Object) },
		UpdateFunc: func(e event.TypedUpdateEvent[*networkingv1.NetworkPolicy]) bool { return isManaged(e.ObjectNew) },
		GenericFunc: func(e event.TypedGenericEvent[*networkingv1.NetworkPolicy]) bool {
			return isManaged(e.Object)
		},
	}
}
