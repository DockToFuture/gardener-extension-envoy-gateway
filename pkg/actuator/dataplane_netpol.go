// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"fmt"

	extensionsconfigv1alpha1 "github.com/gardener/gardener/extensions/pkg/apis/config/v1alpha1"
	extensionsutil "github.com/gardener/gardener/extensions/pkg/util"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

// DataPlaneNetworkPolicyReconciler removes the data-plane NetworkPolicies this
// extension manages directly from the shoot. While the feature is enabled the
// per-shoot controller owns the policies' creation and update; this reconciler
// handles the one-shot cleanup when the feature is disabled or the shoot's
// Extension is deleted while the shoot still lives.
type DataPlaneNetworkPolicyReconciler interface {
	// Reconcile removes every data-plane NetworkPolicy this extension owns in the
	// shoot, across all three hops (proxy ingress, proxy egress, backend
	// ingress). desiredNamespaces is retained for signature compatibility; a
	// non-empty set is treated as "keep nothing" the same as nil, because the
	// per-shoot controller — not this reconciler — reconciles the desired state.
	Reconcile(ctx context.Context, seedNamespace string, desiredNamespaces []string) error
}

// dataPlaneNetworkPolicyReconciler prunes the policies against the shoot API
// server through a client built from the seed.
type dataPlaneNetworkPolicyReconciler struct {
	seedClient client.Client
}

// NewDataPlaneNetworkPolicyReconciler returns a reconciler that talks to the
// shoot API server via util.NewClientForShoot.
func NewDataPlaneNetworkPolicyReconciler(seedClient client.Client) DataPlaneNetworkPolicyReconciler {
	return &dataPlaneNetworkPolicyReconciler{seedClient: seedClient}
}

func (r *dataPlaneNetworkPolicyReconciler) Reconcile(ctx context.Context, seedNamespace string, _ []string) error {
	scheme, err := kubernetesScheme()
	if err != nil {
		return err
	}

	// A direct (uncached) shoot client: this is an occasional one-shot prune, so
	// a cache would add a shoot-wide NetworkPolicy informer for no benefit.
	_, shootClient, err := extensionsutil.NewClientForShoot(
		ctx,
		r.seedClient,
		seedNamespace,
		client.Options{Scheme: scheme},
		extensionsconfigv1alpha1.RESTOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to build shoot client for data-plane NetworkPolicy cleanup: %w", err)
	}

	// Remove every data-plane policy this extension owns in the shoot, across all
	// three hops: hop-1 proxy ingress, plus the hop-2 proxy egress and hop-3
	// backend ingress policies the per-shoot controller wrote while the feature
	// was enabled.
	list := &networkingv1.NetworkPolicyList{}
	if err := shootClient.List(ctx, list, envoygateway.ManagedDataPlanePolicyLabels()); err != nil {
		return fmt.Errorf("failed to list managed data-plane NetworkPolicies in shoot: %w", err)
	}

	for i := range list.Items {
		policy := &list.Items[i]
		if err := shootClient.Delete(ctx, policy); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete managed data-plane NetworkPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
		}
	}

	return nil
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
