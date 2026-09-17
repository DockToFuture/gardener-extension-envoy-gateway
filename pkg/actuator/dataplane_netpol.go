// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"fmt"

	extensionsconfigv1alpha1 "github.com/gardener/gardener/extensions/pkg/apis/config/v1alpha1"
	extensionsutil "github.com/gardener/gardener/extensions/pkg/util"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

// DataPlaneNetworkPolicyReconciler reconciles the per-Gateway-namespace
// data-plane ingress NetworkPolicies directly against the shoot API server.
//
// The shoot's gardener-resource-manager caches only a fixed set of namespaces
// (kube-system, kubernetes-dashboard, kube-node-lease) and hard-errors on any
// other namespace, so the policies cannot ride the shoot ManagedResource into an
// arbitrary Gateway namespace. Instead the extension writes them itself through
// an uncached shoot client, which is subject only to RBAC and can therefore
// operate on any namespace.
type DataPlaneNetworkPolicyReconciler interface {
	// Reconcile brings the set of data-plane NetworkPolicies in the shoot
	// identified by the given seed-side control-plane namespace into agreement
	// with desiredNamespaces: it creates/updates the envoy-gateway-proxies policy
	// in every desired namespace and deletes every managed policy whose namespace
	// is not desired. An empty desiredNamespaces removes all managed policies
	// (feature disabled or extension being deleted). It is idempotent.
	Reconcile(ctx context.Context, seedNamespace string, desiredNamespaces []string) error
}

// realDataPlaneNetworkPolicyReconciler builds an uncached shoot client from the
// seed and reconciles the policies against the shoot API server.
type realDataPlaneNetworkPolicyReconciler struct {
	seedClient client.Client
}

// NewRealDataPlaneNetworkPolicyReconciler returns a reconciler that talks to a
// real shoot API server via util.NewClientForShoot. This is the production
// default.
func NewRealDataPlaneNetworkPolicyReconciler(seedClient client.Client) DataPlaneNetworkPolicyReconciler {
	return &realDataPlaneNetworkPolicyReconciler{seedClient: seedClient}
}

func (r *realDataPlaneNetworkPolicyReconciler) Reconcile(ctx context.Context, seedNamespace string, desiredNamespaces []string) error {
	scheme, err := newNetworkPolicyScheme()
	if err != nil {
		return err
	}

	_, shootClient, err := extensionsutil.NewClientForShoot(
		ctx,
		r.seedClient,
		seedNamespace,
		client.Options{Scheme: scheme},
		extensionsconfigv1alpha1.RESTOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to build shoot client for data-plane NetworkPolicy reconciliation: %w", err)
	}

	for _, ns := range desiredNamespaces {
		desired := dataPlaneNetworkPolicy(ns)
		obj := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: ns}}
		if _, err := controllerutil.CreateOrUpdate(ctx, shootClient, obj, func() error {
			obj.Labels = desired.Labels
			obj.Spec = desired.Spec

			return nil
		}); err != nil {
			return fmt.Errorf("failed to reconcile data-plane NetworkPolicy in namespace %q: %w", ns, err)
		}
	}

	// Prune: list every managed policy across the shoot and delete the ones whose
	// namespace is no longer desired. This reclaims policies from namespaces that
	// dropped their last Gateway, and (with an empty desired set) removes all of
	// them when the feature is disabled or the extension is removed.
	list := &networkingv1.NetworkPolicyList{}
	if err := shootClient.List(ctx, list, managedDataPlanePolicyLabels()); err != nil {
		return fmt.Errorf("failed to list managed data-plane NetworkPolicies in shoot: %w", err)
	}

	for _, stale := range stalePolicies(list.Items, desiredNamespaces) {
		if err := shootClient.Delete(ctx, &stale); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete stale data-plane NetworkPolicy %s/%s: %w", stale.Namespace, stale.Name, err)
		}
	}

	return nil
}

// stalePolicies returns the subset of existing managed policies whose namespace
// is not in desiredNamespaces. It is a pure function so the prune decision can be
// unit-tested without a shoot API server.
func stalePolicies(existing []networkingv1.NetworkPolicy, desiredNamespaces []string) []networkingv1.NetworkPolicy {
	desired := make(map[string]struct{}, len(desiredNamespaces))
	for _, ns := range desiredNamespaces {
		desired[ns] = struct{}{}
	}

	var stale []networkingv1.NetworkPolicy
	for _, p := range existing {
		if _, ok := desired[p.Namespace]; !ok {
			stale = append(stale, p)
		}
	}

	return stale
}

// managedDataPlanePolicyLabels are the labels the extension stamps on the
// data-plane NetworkPolicies it manages. They scope the prune List precisely to
// policies this extension owns.
func managedDataPlanePolicyLabels() client.MatchingLabels {
	return client.MatchingLabels{
		envoygateway.LabelManagedBy: envoygateway.LabelManagedByValue,
		envoygateway.LabelName:      envoygateway.DataPlaneNetworkPolicyName,
	}
}

// dataPlaneNetworkPolicy returns the data-plane ingress NetworkPolicy for a
// single Gateway namespace. It selects the canonical Envoy data-plane proxy pods
// (managed-by=envoy-gateway, name=envoy) and allows ingress from anywhere on the
// two well-known data-plane ports. A Gateway exists precisely to accept external
// traffic, so allow-from-anywhere is what the operator opts into by enabling the
// feature.
func dataPlaneNetworkPolicy(namespace string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	httpPort := intstr.FromInt(envoygateway.DataPlaneHTTPPort)
	readyPort := intstr.FromInt(envoygateway.DataPlaneReadyPort)

	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "networking.k8s.io/v1",
			Kind:       "NetworkPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      envoygateway.DataPlaneNetworkPolicyName,
			Namespace: namespace,
			Labels: map[string]string{
				envoygateway.LabelManagedBy: envoygateway.LabelManagedByValue,
				envoygateway.LabelName:      envoygateway.DataPlaneNetworkPolicyName,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					envoygateway.LabelManagedBy: envoygateway.EnvoyProxyManagedByValue,
					envoygateway.LabelName:      envoygateway.EnvoyProxyNameValue,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				// Empty From: ingress from anywhere.
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &httpPort},
					{Protocol: &tcp, Port: &readyPort},
				},
			}},
		},
	}
}

// newNetworkPolicyScheme returns a runtime scheme that knows the core
// networking.k8s.io types so the shoot client can operate on NetworkPolicy
// objects.
func newNetworkPolicyScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to register client-go scheme: %w", err)
	}

	return scheme, nil
}
