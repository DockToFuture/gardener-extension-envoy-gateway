// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

// Package netpolcontroller contains the event-driven controller that keeps the
// data-plane NetworkPolicies in a shoot in sync with its Gateways and routes.
package netpolcontroller

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

// serviceKind and serviceGroup identify a core/v1 Service backendRef. The
// Gateway API defaults an unset Kind to "Service" and an unset Group to the
// core group (empty string).
const (
	serviceKind  = "Service"
	serviceGroup = ""
)

// RouteInfo is a route-type-neutral view of a Gateway API route, carrying only
// what backend resolution needs: the route's own namespace and its flattened
// list of backend references. HTTPRoute, GRPCRoute, and the experimental
// TCP/UDP/TLSRoute all normalize into this shape.
type RouteInfo struct {
	// Namespace is the namespace the route object lives in; it is the default
	// namespace for any backendRef that does not set one explicitly.
	Namespace string
	// BackendRefs is the flattened set of backend references across all of the
	// route's rules.
	BackendRefs []gatewayv1.BackendObjectReference
}

// SkipReason records a backendRef the controller could not derive a
// NetworkPolicy for, so the caller can log it with enough context for an
// operator to act. It is never fatal: resolution continues past a skip.
type SkipReason struct {
	// Namespace and Name identify the backend Service (or other referent).
	Namespace string
	Name      string
	// Reason is a short, stable, machine-greppable cause.
	Reason string
	// Detail is a human-readable explanation.
	Detail string
}

func (s SkipReason) String() string {
	return fmt.Sprintf("%s/%s: %s (%s)", s.Namespace, s.Name, s.Reason, s.Detail)
}

// Skip reasons.
const (
	// ReasonNotService — the backendRef points at a non-Service kind; the
	// controller only derives policies for Service backends.
	ReasonNotService = "NotService"
	// ReasonNoReferenceGrant — a cross-namespace backendRef is not permitted by
	// any ReferenceGrant in the backend namespace.
	ReasonNoReferenceGrant = "NoReferenceGrant"
	// ReasonServiceNotFound — the referenced Service does not exist.
	ReasonServiceNotFound = "ServiceNotFound"
	// ReasonSelectorless — the Service has no spec.selector (headless with
	// manually-managed Endpoints, ExternalName, or otherwise selector-less), so
	// its backend pods cannot be selected by a NetworkPolicy.
	ReasonSelectorless = "SelectorlessService"
)

// ResolveBackends resolves a route's backendRefs into the deduplicated set of
// BackendTargets the data-plane proxies must be allowed to reach, together with
// the refs that were skipped (and why). It reads Services and ReferenceGrants
// through the given reader, so tests can drive it with a fake client. The error
// return is reserved for unexpected API failures; a missing Service or a denied
// cross-namespace ref is reported as a skip, not an error.
func ResolveBackends(ctx context.Context, reader client.Reader, route RouteInfo) ([]envoygateway.BackendTarget, []SkipReason, error) {
	var (
		targets []envoygateway.BackendTarget
		skips   []SkipReason
		seen    = map[string]struct{}{}
	)

	for _, ref := range route.BackendRefs {
		group := serviceGroup
		if ref.Group != nil {
			group = string(*ref.Group)
		}
		kind := serviceKind
		if ref.Kind != nil {
			kind = string(*ref.Kind)
		}
		name := string(ref.Name)

		backendNs := route.Namespace
		if ref.Namespace != nil {
			backendNs = string(*ref.Namespace)
		}

		if group != serviceGroup || kind != serviceKind {
			skips = append(skips, SkipReason{
				Namespace: backendNs, Name: name, Reason: ReasonNotService,
				Detail: fmt.Sprintf("backendRef kind %q.%q is not a core Service", group, kind),
			})

			continue
		}

		// Cross-namespace backendRefs require a ReferenceGrant in the backend
		// namespace permitting this route kind from the route's namespace to the
		// Service. Same-namespace refs never need one.
		if backendNs != route.Namespace {
			allowed, err := referenceGrantAllows(ctx, reader, route.Namespace, backendNs, name)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to evaluate ReferenceGrants in namespace %q: %w", backendNs, err)
			}
			if !allowed {
				skips = append(skips, SkipReason{
					Namespace: backendNs, Name: name, Reason: ReasonNoReferenceGrant,
					Detail: fmt.Sprintf("no ReferenceGrant in %q permits a Service backendRef from namespace %q", backendNs, route.Namespace),
				})

				continue
			}
		}

		svc := &corev1.Service{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: backendNs, Name: name}, svc); err != nil {
			if apierrors.IsNotFound(err) {
				skips = append(skips, SkipReason{
					Namespace: backendNs, Name: name, Reason: ReasonServiceNotFound,
					Detail: "referenced Service does not exist",
				})

				continue
			}

			return nil, nil, fmt.Errorf("failed to get Service %s/%s: %w", backendNs, name, err)
		}

		// A Service without a spec.selector (ExternalName, headless with
		// manually-managed Endpoints, or any selector-less Service) has no pods
		// the controller can select, so no backend NetworkPolicy can be derived.
		if len(svc.Spec.Selector) == 0 {
			skips = append(skips, SkipReason{
				Namespace: backendNs, Name: name, Reason: ReasonSelectorless,
				Detail: "Service has no spec.selector (headless/ExternalName/manual Endpoints); write its NetworkPolicy by hand",
			})

			continue
		}

		key := backendNs + "/" + name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		targets = append(targets, envoygateway.BackendTarget{
			Namespace:   backendNs,
			PodSelector: copyStringMap(svc.Spec.Selector),
		})
	}

	return targets, skips, nil
}

// referenceGrantAllows reports whether some ReferenceGrant in backendNs permits
// a Service backendRef originating from a route in fromNs. A grant matches when
// one of its From entries names the route's namespace with a route Kind in the
// Gateway API group, and one of its To entries names Service (optionally scoped
// to the specific Service name). Pure over the reader so it is unit-testable.
func referenceGrantAllows(ctx context.Context, reader client.Reader, fromNs, backendNs, serviceName string) (bool, error) {
	grants := &gatewayv1.ReferenceGrantList{}
	if err := reader.List(ctx, grants, client.InNamespace(backendNs)); err != nil {
		return false, err
	}

	for _, g := range grants.Items {
		if !grantFromMatches(g.Spec.From, fromNs) {
			continue
		}
		if grantToMatchesService(g.Spec.To, serviceName) {
			return true, nil
		}
	}

	return false, nil
}

// grantFromMatches reports whether any From entry permits a Gateway API route
// in fromNs. We accept any route Kind in the Gateway API group rather than
// pinning each route type, because the From Kind must name the specific route
// kind and the caller already knows the ref came from a route.
func grantFromMatches(from []gatewayv1.ReferenceGrantFrom, fromNs string) bool {
	for _, f := range from {
		if string(f.Group) == envoygateway.APIGroupGatewayAPI && string(f.Namespace) == fromNs && isRouteKind(string(f.Kind)) {
			return true
		}
	}

	return false
}

// grantToMatchesService reports whether any To entry permits a Service backend,
// optionally scoped to serviceName (an unset To.Name permits every Service of
// the kind in the namespace).
func grantToMatchesService(to []gatewayv1.ReferenceGrantTo, serviceName string) bool {
	for _, t := range to {
		if string(t.Group) != serviceGroup || string(t.Kind) != serviceKind {
			continue
		}
		if t.Name == nil || string(*t.Name) == serviceName {
			return true
		}
	}

	return false
}

// isRouteKind reports whether kind is one of the Gateway API route kinds that
// can carry a Service backendRef.
func isRouteKind(kind string) bool {
	switch kind {
	case "HTTPRoute", "GRPCRoute", "TCPRoute", "TLSRoute", "UDPRoute":
		return true
	default:
		return false
	}
}

// copyStringMap returns a shallow copy of m, so a Service's live selector map is
// never aliased into a long-lived NetworkPolicy object.
func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	maps.Copy(out, m)

	return out
}
