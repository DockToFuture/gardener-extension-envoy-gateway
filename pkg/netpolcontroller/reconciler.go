// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

// Package netpolcontroller contains the event-driven controller that keeps the
// data-plane NetworkPolicies for the Envoy proxies in a shoot in sync with its
// Gateways: the hop-1 client → proxy ingress policy and the hop-2 proxy egress
// policy (DNS + control-plane xDS) the proxies need to boot. Egress to route
// backends is managed separately.
package netpolcontroller

import (
	"context"
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

// Reconciler keeps the data-plane proxy NetworkPolicies in a shoot in sync with
// its Gateways. It is keyed by Gateway namespace: one reconcile request covers
// the hop-1 ingress and hop-2 egress policies anchored at that namespace.
type Reconciler struct {
	// ShootClient reads and writes against the shoot API server. It is the
	// cached client of the shoot cluster, so reads are served from informers and
	// writes go straight through.
	ShootClient client.Client
}

// Reconcile converges the data-plane proxy NetworkPolicies anchored at a single
// Gateway namespace. If the namespace holds no Gateway of our GatewayClass, the
// policies anchored there are pruned.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := ctrllog.FromContext(ctx).WithValues("gatewayNamespace", req.Namespace)
	gatewayNs := req.Namespace

	hasGateway, err := r.namespaceHasManagedGateway(ctx, gatewayNs)
	if err != nil {
		return reconcile.Result{}, err
	}

	if !hasGateway {
		log.Info("no managed Gateway in namespace; pruning data-plane policies")
		if err := r.pruneNamespace(ctx, gatewayNs); err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	// Hop 1: client → proxy ingress in the Gateway namespace.
	if err := r.apply(ctx, envoygateway.DataPlaneProxyIngressPolicy(gatewayNs)); err != nil {
		return reconcile.Result{}, err
	}

	// Hop 2: proxy egress to DNS and the control plane (xDS), so the proxies can
	// boot under a default-deny posture. Egress to route backends is managed
	// separately and intentionally not derived here.
	if err := r.apply(ctx, envoygateway.ProxyEgressPolicy(gatewayNs)); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// namespaceHasManagedGateway reports whether the given namespace holds at least
// one Gateway bound to this extension's GatewayClass.
func (r *Reconciler) namespaceHasManagedGateway(ctx context.Context, namespace string) (bool, error) {
	gateways := &gatewayv1.GatewayList{}
	if err := r.ShootClient.List(ctx, gateways, client.InNamespace(namespace)); err != nil {
		return false, fmt.Errorf("failed to list Gateways in namespace %q: %w", namespace, err)
	}
	for _, gw := range gateways.Items {
		if string(gw.Spec.GatewayClassName) == envoygateway.GatewayClassName {
			return true, nil
		}
	}

	return false, nil
}

// apply creates or updates the given NetworkPolicy, carrying over its labels and
// spec. The object identity (name/namespace) comes from the desired policy.
func (r *Reconciler) apply(ctx context.Context, desired *networkingv1.NetworkPolicy) error {
	obj := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.ShootClient, obj, func() error {
		obj.Labels = desired.Labels
		obj.Spec = desired.Spec

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to apply NetworkPolicy %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	return nil
}

// pruneNamespace removes the data-plane proxy policies anchored at the given
// Gateway namespace: its hop-1 ingress and hop-2 egress policies.
func (r *Reconciler) pruneNamespace(ctx context.Context, gatewayNs string) error {
	for _, name := range []string{envoygateway.DataPlaneNetworkPolicyName, envoygateway.ProxyEgressNetworkPolicyName} {
		obj := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: gatewayNs, Name: name}}
		if err := r.ShootClient.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete NetworkPolicy %s/%s: %w", gatewayNs, name, err)
		}
	}

	return nil
}

// AddToManager registers the controller with the manager, watching the shoot
// cluster's cache for the resources that influence the derived policies. Every
// watch maps its object back to the Gateway namespace(s) it affects, so the
// reconciler is always keyed by Gateway namespace.
func (r *Reconciler) AddToManager(mgr manager.Manager, shootCluster cluster.Cluster) error {
	c := shootCluster.GetCache()

	return builder.ControllerManagedBy(mgr).
		Named("envoy-gateway-netpol").
		WatchesRawSource(source.Kind(c, &gatewayv1.Gateway{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapGatewayToRequests),
			managedGatewayPredicate(),
		)).
		// Self-heal: watch our own managed NetworkPolicies so a manual delete or
		// edit triggers immediate recreation, not just recovery on the next
		// resync. Both the hop-1 and hop-2 policies live in the Gateway
		// namespace, so the policy's namespace is the reconcile key.
		WatchesRawSource(source.Kind(c, &networkingv1.NetworkPolicy{},
			handler.TypedEnqueueRequestsFromMapFunc(mapManagedPolicyToRequests),
			managedPolicyPredicate(),
		)).
		Complete(r)
}
