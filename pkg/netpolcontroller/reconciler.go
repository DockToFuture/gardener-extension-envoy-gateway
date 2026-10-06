// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package netpolcontroller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
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
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

// Reconciler keeps the three-hop data-plane NetworkPolicies in a shoot in sync
// with its Gateways and routes. It is keyed by Gateway namespace: one reconcile
// request covers every Gateway, route, and derived policy anchored at that
// namespace.
type Reconciler struct {
	// ShootClient reads and writes against the shoot API server. It is the
	// cached client of the shoot cluster, so reads are served from informers and
	// writes go straight through.
	ShootClient client.Client
	// Experimental enables resolving the experimental-channel route kinds.
	Experimental bool
}

// Reconcile converges all data-plane NetworkPolicies anchored at a single
// Gateway namespace. If the namespace holds no Gateway of our GatewayClass, all
// policies anchored there (hop-1, hop-2, and the hop-3 policies this Gateway
// namespace produced in backend namespaces) are pruned.
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

	// Resolve backends from the namespace's routes, then write hop 2 (proxy
	// egress) and hop 3 (backend ingress).
	targets, err := r.resolveNamespaceBackends(ctx, log, gatewayNs)
	if err != nil {
		return reconcile.Result{}, err
	}

	if err := r.apply(ctx, envoygateway.ProxyEgressPolicy(gatewayNs, targets)); err != nil {
		return reconcile.Result{}, err
	}

	desiredBackend := make([]client.ObjectKey, 0, len(targets))
	for _, t := range targets {
		policy := envoygateway.BackendIngressPolicy(t.Namespace, gatewayNs, t.PodSelector)
		if err := r.apply(ctx, policy); err != nil {
			return reconcile.Result{}, err
		}
		desiredBackend = append(desiredBackend, client.ObjectKey{Namespace: t.Namespace, Name: policy.Name})
	}

	// Prune hop-3 policies this Gateway namespace previously produced but no
	// longer needs (a backend was removed from a route). Hop-3 policies are
	// named deterministically from the Gateway namespace, so we can select only
	// the ones this namespace owns.
	if err := r.pruneBackendIngress(ctx, gatewayNs, desiredBackend); err != nil {
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

// resolveNamespaceBackends collects the deduplicated backend targets across all
// routes whose parentRefs attach to a Gateway in the given namespace. Skips are
// logged, never fatal.
func (r *Reconciler) resolveNamespaceBackends(ctx context.Context, log logr.Logger, gatewayNs string) ([]envoygateway.BackendTarget, error) {
	routes, err := r.listRouteInfos(ctx, gatewayNs)
	if err != nil {
		return nil, err
	}

	var (
		targets []envoygateway.BackendTarget
		seen    = map[string]struct{}{}
	)
	for _, route := range routes {
		resolved, skips, err := ResolveBackends(ctx, r.ShootClient, route)
		if err != nil {
			return nil, err
		}
		for _, s := range skips {
			log.Info("skipping backendRef for data-plane NetworkPolicy derivation", "reason", s.Reason, "backend", s.Namespace+"/"+s.Name, "detail", s.Detail)
		}
		for _, t := range resolved {
			key := t.Namespace + "/" + fmt.Sprint(t.PodSelector)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			targets = append(targets, t)
		}
	}

	return targets, nil
}

// listRouteInfos gathers every route (HTTP/GRPC, plus experimental TCP/UDP/TLS)
// in the Gateway namespace and normalizes it into a RouteInfo. Routes are
// matched by namespace: a route in the Gateway's namespace targeting that
// Gateway is the common case, and cross-namespace route attachment is governed
// by the Gateway's AllowedRoutes, which we do not re-evaluate here — listing by
// the Gateway namespace keeps the derivation conservative and local.
func (r *Reconciler) listRouteInfos(ctx context.Context, gatewayNs string) ([]RouteInfo, error) {
	var infos []RouteInfo

	httpRoutes := &gatewayv1.HTTPRouteList{}
	if err := r.ShootClient.List(ctx, httpRoutes, client.InNamespace(gatewayNs)); err != nil {
		return nil, fmt.Errorf("failed to list HTTPRoutes: %w", err)
	}
	for _, rt := range httpRoutes.Items {
		if !routeAttachesToNamespace(rt.Spec.ParentRefs, rt.Namespace, gatewayNs) {
			continue
		}
		info := RouteInfo{Namespace: rt.Namespace}
		for _, rule := range rt.Spec.Rules {
			for _, br := range rule.BackendRefs {
				info.BackendRefs = append(info.BackendRefs, br.BackendObjectReference)
			}
		}
		infos = append(infos, info)
	}

	grpcRoutes := &gatewayv1.GRPCRouteList{}
	if err := r.ShootClient.List(ctx, grpcRoutes, client.InNamespace(gatewayNs)); err != nil {
		return nil, fmt.Errorf("failed to list GRPCRoutes: %w", err)
	}
	for _, rt := range grpcRoutes.Items {
		if !routeAttachesToNamespace(rt.Spec.ParentRefs, rt.Namespace, gatewayNs) {
			continue
		}
		info := RouteInfo{Namespace: rt.Namespace}
		for _, rule := range rt.Spec.Rules {
			for _, br := range rule.BackendRefs {
				info.BackendRefs = append(info.BackendRefs, br.BackendObjectReference)
			}
		}
		infos = append(infos, info)
	}

	if r.Experimental {
		experimental, err := r.listExperimentalRouteInfos(ctx, gatewayNs)
		if err != nil {
			return nil, err
		}
		infos = append(infos, experimental...)
	}

	return infos, nil
}

// listExperimentalRouteInfos lists the experimental-channel route kinds. Their
// backendRefs are plain BackendRef (no per-backend filters), so normalization
// is a straight copy.
func (r *Reconciler) listExperimentalRouteInfos(ctx context.Context, gatewayNs string) ([]RouteInfo, error) {
	var infos []RouteInfo

	tcpRoutes := &gatewayv1alpha2.TCPRouteList{}
	if err := r.ShootClient.List(ctx, tcpRoutes, client.InNamespace(gatewayNs)); err != nil {
		return nil, fmt.Errorf("failed to list TCPRoutes: %w", err)
	}
	for _, rt := range tcpRoutes.Items {
		if !routeAttachesToNamespace(rt.Spec.ParentRefs, rt.Namespace, gatewayNs) {
			continue
		}
		info := RouteInfo{Namespace: rt.Namespace}
		for _, rule := range rt.Spec.Rules {
			for _, br := range rule.BackendRefs {
				info.BackendRefs = append(info.BackendRefs, br.BackendObjectReference)
			}
		}
		infos = append(infos, info)
	}

	udpRoutes := &gatewayv1alpha2.UDPRouteList{}
	if err := r.ShootClient.List(ctx, udpRoutes, client.InNamespace(gatewayNs)); err != nil {
		return nil, fmt.Errorf("failed to list UDPRoutes: %w", err)
	}
	for _, rt := range udpRoutes.Items {
		if !routeAttachesToNamespace(rt.Spec.ParentRefs, rt.Namespace, gatewayNs) {
			continue
		}
		info := RouteInfo{Namespace: rt.Namespace}
		for _, rule := range rt.Spec.Rules {
			for _, br := range rule.BackendRefs {
				info.BackendRefs = append(info.BackendRefs, br.BackendObjectReference)
			}
		}
		infos = append(infos, info)
	}

	tlsRoutes := &gatewayv1alpha2.TLSRouteList{}
	if err := r.ShootClient.List(ctx, tlsRoutes, client.InNamespace(gatewayNs)); err != nil {
		return nil, fmt.Errorf("failed to list TLSRoutes: %w", err)
	}
	for _, rt := range tlsRoutes.Items {
		if !routeAttachesToNamespace(rt.Spec.ParentRefs, rt.Namespace, gatewayNs) {
			continue
		}
		info := RouteInfo{Namespace: rt.Namespace}
		for _, rule := range rt.Spec.Rules {
			for _, br := range rule.BackendRefs {
				info.BackendRefs = append(info.BackendRefs, br.BackendObjectReference)
			}
		}
		infos = append(infos, info)
	}

	return infos, nil
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

// pruneNamespace removes every data-plane policy anchored at the given Gateway
// namespace: its hop-1 and hop-2 policies, plus every hop-3 policy it produced
// in backend namespaces.
func (r *Reconciler) pruneNamespace(ctx context.Context, gatewayNs string) error {
	for _, name := range []string{envoygateway.DataPlaneNetworkPolicyName, envoygateway.ProxyEgressNetworkPolicyName} {
		obj := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: gatewayNs, Name: name}}
		if err := r.ShootClient.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete NetworkPolicy %s/%s: %w", gatewayNs, name, err)
		}
	}

	return r.pruneBackendIngress(ctx, gatewayNs, nil)
}

// pruneBackendIngress removes the hop-3 policies produced by the given Gateway
// namespace that are not in the desired set. Because the hop-3 name is a
// deterministic function of the Gateway namespace, every backend-namespace
// policy carrying that name belongs to this Gateway namespace, so pruning is
// precise even across many Gateway namespaces sharing a backend namespace.
func (r *Reconciler) pruneBackendIngress(ctx context.Context, gatewayNs string, desired []client.ObjectKey) error {
	list := &networkingv1.NetworkPolicyList{}
	if err := r.ShootClient.List(ctx, list, envoygateway.ManagedHopPolicyLabels(envoygateway.HopBackendIngress)); err != nil {
		return fmt.Errorf("failed to list hop-3 NetworkPolicies: %w", err)
	}

	wantName := envoygateway.BackendIngressPolicyName(gatewayNs)
	// Restrict the candidate set to the policies this Gateway namespace owns.
	owned := make([]networkingv1.NetworkPolicy, 0, len(list.Items))
	for _, p := range list.Items {
		if p.Name == wantName {
			owned = append(owned, p)
		}
	}

	for _, stale := range envoygateway.StalePolicies(owned, desired) {
		if err := r.ShootClient.Delete(ctx, &stale); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete stale hop-3 NetworkPolicy %s/%s: %w", stale.Namespace, stale.Name, err)
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

	b := builder.ControllerManagedBy(mgr).
		Named("envoy-gateway-netpol").
		WatchesRawSource(source.Kind(c, &gatewayv1.Gateway{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapGatewayToRequests),
			managedGatewayPredicate(),
		)).
		WatchesRawSource(source.Kind(c, &gatewayv1.HTTPRoute{},
			handler.TypedEnqueueRequestsFromMapFunc(mapHTTPRouteToRequests),
		)).
		WatchesRawSource(source.Kind(c, &gatewayv1.GRPCRoute{},
			handler.TypedEnqueueRequestsFromMapFunc(mapGRPCRouteToRequests),
		)).
		WatchesRawSource(source.Kind(c, &corev1.Service{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapServiceToRequests),
			serviceSelectorChangedPredicate(),
		)).
		WatchesRawSource(source.Kind(c, &gatewayv1.ReferenceGrant{},
			handler.TypedEnqueueRequestsFromMapFunc(r.mapReferenceGrantToRequests),
		)).
		// Self-heal: watch our own managed NetworkPolicies so a manual delete or
		// edit triggers immediate recreation, not just recovery on the next
		// resync. The policy's namespace is the Gateway namespace for hop-1/hop-2;
		// for hop-3 it is a backend namespace, so we fan out to the Gateway
		// namespaces its ingress peer names.
		WatchesRawSource(source.Kind(c, &networkingv1.NetworkPolicy{},
			handler.TypedEnqueueRequestsFromMapFunc(mapManagedPolicyToRequests),
			managedPolicyPredicate(),
		))

	if r.Experimental {
		b = b.
			WatchesRawSource(source.Kind(c, &gatewayv1alpha2.TCPRoute{},
				handler.TypedEnqueueRequestsFromMapFunc(mapTCPRouteToRequests),
			)).
			WatchesRawSource(source.Kind(c, &gatewayv1alpha2.UDPRoute{},
				handler.TypedEnqueueRequestsFromMapFunc(mapUDPRouteToRequests),
			)).
			WatchesRawSource(source.Kind(c, &gatewayv1alpha2.TLSRoute{},
				handler.TypedEnqueueRequestsFromMapFunc(mapTLSRouteToRequests),
			))
	}

	return b.Complete(r)
}
