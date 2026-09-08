// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"fmt"

	extensionsconfigv1alpha1 "github.com/gardener/gardener/extensions/pkg/apis/config/v1alpha1"
	extensionsutil "github.com/gardener/gardener/extensions/pkg/util"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// GatewayLister lists Gateway objects in a shoot. It is injectable so that
// unit tests can replace the production implementation (which requires a
// running shoot API server) with an in-memory fake.
type GatewayLister interface {
	// ListGateways returns the names of Gateway objects (namespace/name) that
	// exist in the shoot identified by the given seed-side control-plane
	// namespace. An empty slice means the shoot is empty of user Gateways.
	ListGateways(ctx context.Context, seedNamespace string) ([]string, error)
}

// realGatewayLister builds a shoot client from the seed and lists Gateway
// resources from the shoot API server.
type realGatewayLister struct {
	seedClient client.Client
}

// NewRealGatewayLister returns a GatewayLister that talks to a real shoot
// API server via util.NewClientForShoot. This is the production default.
func NewRealGatewayLister(seedClient client.Client) GatewayLister {
	return &realGatewayLister{seedClient: seedClient}
}

func (r *realGatewayLister) ListGateways(ctx context.Context, seedNamespace string) ([]string, error) {
	// Build a shoot-scoped client. The function returns immediately after
	// reading the kubeconfig secret — connectivity to the shoot API server
	// is only checked on the first call we make below.
	scheme, err := newGatewayScheme()
	if err != nil {
		return nil, err
	}

	_, shootClient, err := extensionsutil.NewClientForShoot(
		ctx,
		r.seedClient,
		seedNamespace,
		client.Options{Scheme: scheme},
		extensionsconfigv1alpha1.RESTOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build shoot client: %w", err)
	}

	list := &gatewayapiv1.GatewayList{}
	if err := shootClient.List(ctx, list); err != nil {
		return nil, fmt.Errorf("failed to list Gateways in shoot: %w", err)
	}

	names := make([]string, 0, len(list.Items))
	for _, g := range list.Items {
		names = append(names, fmt.Sprintf("%s/%s", g.Namespace, g.Name))
	}

	return names, nil
}

// gatewaysInUseError indicates that the extension cannot be deleted because
// user-owned Gateway objects still exist in the shoot. The Gardener admission
// chain surfaces this back to the user on the Shoot update that disabled the
// extension.
type gatewaysInUseError struct {
	names []string
}

func (e *gatewaysInUseError) Error() string {
	const maxPreview = 5
	preview := e.names
	more := ""
	if len(preview) > maxPreview {
		preview = preview[:maxPreview]
		more = fmt.Sprintf(" (and %d more)", len(e.names)-maxPreview)
	}

	return fmt.Sprintf(
		"envoy-gateway extension cannot be removed while %d user Gateway object(s) still exist in the shoot. "+
			"Delete the following Gateways first and try again: %v%s",
		len(e.names), preview, more,
	)
}

// newGatewayScheme returns a runtime scheme that knows the Gateway API
// standard-channel types so the shoot client can decode GatewayList responses.
func newGatewayScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := gatewayapiv1.Install(scheme); err != nil {
		return nil, fmt.Errorf("failed to register gateway-api scheme: %w", err)
	}

	return scheme, nil
}
