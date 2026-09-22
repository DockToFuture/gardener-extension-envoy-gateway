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
	kubernetesscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

// DataPlaneNetworkPolicyReconciler reconciles the per-Gateway-namespace
// data-plane ingress NetworkPolicies directly against the shoot API server.
// They cannot ride the shoot ManagedResource: gardener-resource-manager
// reconciles only a fixed set of namespaces and cannot reach arbitrary Gateway
// namespaces.
type DataPlaneNetworkPolicyReconciler interface {
	// Reconcile makes the data-plane NetworkPolicies in the shoot match
	// desiredNamespaces: the policy is created/updated in every desired namespace
	// and deleted everywhere else. An empty desiredNamespaces removes all managed
	// policies. It is idempotent.
	Reconcile(ctx context.Context, seedNamespace string, desiredNamespaces []string) error
}

// dataPlaneNetworkPolicyReconciler reconciles the policies against the shoot API
// server through a client built from the seed.
type dataPlaneNetworkPolicyReconciler struct {
	seedClient client.Client
}

// NewDataPlaneNetworkPolicyReconciler returns a reconciler that talks to the
// shoot API server via util.NewClientForShoot.
func NewDataPlaneNetworkPolicyReconciler(seedClient client.Client) DataPlaneNetworkPolicyReconciler {
	return &dataPlaneNetworkPolicyReconciler{seedClient: seedClient}
}

func (r *dataPlaneNetworkPolicyReconciler) Reconcile(ctx context.Context, seedNamespace string, desiredNamespaces []string) error {
	scheme, err := kubernetesScheme()
	if err != nil {
		return err
	}

	// A direct (uncached) shoot client: the reconciler only issues occasional
	// writes and a prune List, so a cache would add a shoot-wide NetworkPolicy
	// informer for no benefit.
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

	// Prune managed policies whose namespace is no longer desired. With an empty
	// desired set this removes all of them.
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

// stalePolicies returns the existing managed policies whose namespace is not in
// desiredNamespaces. Pure so the prune decision is unit-testable.
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
// single Gateway namespace. It selects the Envoy data-plane proxy pods and
// allows ingress on the data-plane ports. The rule has no From, so it admits
// traffic regardless of source: a Gateway may be fronted by an external or an
// internal load balancer, and the extension does not manage which, so it does
// not restrict the source here. Reachability is still bounded by the load
// balancer's own scope and by the podSelector.
func dataPlaneNetworkPolicy(namespace string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	httpPort := intstr.FromInt(envoygateway.DataPlaneHTTPPort)
	httpsPort := intstr.FromInt(envoygateway.DataPlaneHTTPSPort)
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
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &httpPort},
					{Protocol: &tcp, Port: &httpsPort},
					{Protocol: &tcp, Port: &readyPort},
				},
			}},
		},
	}
}

// kubernetesScheme returns a runtime scheme with the built-in Kubernetes types
// so the shoot client can operate on NetworkPolicy objects.
func kubernetesScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := kubernetesscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to register kubernetes scheme: %w", err)
	}

	return scheme, nil
}
