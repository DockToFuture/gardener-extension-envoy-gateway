// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	extensionsconfigv1alpha1 "github.com/gardener/gardener/extensions/pkg/apis/config/v1alpha1"
	extensionsutil "github.com/gardener/gardener/extensions/pkg/util"
	"github.com/gardener/gardener/pkg/controllerutils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

// gatewayExistsFinalizer is the finalizer the upstream Envoy Gateway controller
// puts on a GatewayClass while Gateways reference it.
const gatewayExistsFinalizer = "gateway-exists-finalizer.gateway.networking.k8s.io"

const (
	// gatewayDeletionPollInterval is how often WaitUntilGatewaysDeleted polls.
	gatewayDeletionPollInterval = 2 * time.Second
	// GatewayDeletionTimeout bounds how long the actuator waits for user Gateways
	// (and their LB Services) to drain. Kept under the gardenlet's pre-apiserver wait.
	GatewayDeletionTimeout = 2 * time.Minute
)

// ErrGatewaysStillDeleting means the wait timed out with the shoot API server
// reachable and Gateways still present — retryable, so the caller should requeue.
var ErrGatewaysStillDeleting = errors.New("user Gateways still present in shoot")

// GatewayLister lists Gateway objects in a shoot and clears the leftover
// GatewayClass finalizer during shoot deletion.
type GatewayLister interface {
	// ListGateways returns the names of Gateway objects (namespace/name) that
	// exist in the shoot
	ListGateways(ctx context.Context, seedNamespace string) ([]string, error)

	// ClearGatewayClassFinalizer removes the gateway-exists finalizer from the
	// extension's GatewayClass in the shoot.
	ClearGatewayClassFinalizer(ctx context.Context, seedNamespace string) error

	// ListGatewayNamespaces returns the deduped, sorted set of namespaces that
	// hold at least one Gateway object in the shoot identified by the given
	// seed-side control-plane namespace.
	ListGatewayNamespaces(ctx context.Context, seedNamespace string) ([]string, error)

	// DeleteGateways deletes all Gateway objects across all namespaces in the
	// shoot identified by the given seed-side control-plane namespace.
	DeleteGateways(ctx context.Context, seedNamespace string) error

	// WaitUntilGatewaysDeleted blocks until no Gateway objects remain in the
	// shoot, or the context is done.
	WaitUntilGatewaysDeleted(ctx context.Context, seedNamespace string) error
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
	list, err := r.fetchGatewayList(ctx, seedNamespace)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(list.Items))
	for _, g := range list.Items {
		names = append(names, fmt.Sprintf("%s/%s", g.Namespace, g.Name))
	}

	return names, nil
}

func (r *realGatewayLister) ListGatewayNamespaces(ctx context.Context, seedNamespace string) ([]string, error) {
	list, err := r.fetchGatewayList(ctx, seedNamespace)
	if err != nil {
		return nil, err
	}

	return dedupeGatewayNamespaces(list), nil
}

// dedupeGatewayNamespaces returns the deduped, sorted set of namespaces that
// hold at least one Gateway in the list.
func dedupeGatewayNamespaces(list *gatewayapiv1.GatewayList) []string {
	seen := make(map[string]struct{}, len(list.Items))
	for _, g := range list.Items {
		seen[g.Namespace] = struct{}{}
	}

	namespaces := make([]string, 0, len(seen))
	for ns := range seen {
		namespaces = append(namespaces, ns)
	}
	slices.Sort(namespaces)

	return namespaces
}

// fetchGatewayList builds a shoot-scoped client and returns the raw GatewayList.
// Both the delete guard and the NetworkPolicy emission derive from it.
func (r *realGatewayLister) fetchGatewayList(ctx context.Context, seedNamespace string) (*gatewayapiv1.GatewayList, error) {
	shootClient, err := r.shootClient(ctx, seedNamespace)
	if err != nil {
		return nil, err
	}

	list := &gatewayapiv1.GatewayList{}
	if err := shootClient.List(ctx, list); err != nil {
		return nil, fmt.Errorf("failed to list Gateways in shoot: %w", err)
	}

	return list, nil
}

// ClearGatewayClassFinalizer removes the gateway-exists finalizer from the
// extension's GatewayClass so it can finish deleting once the envoy-gateway
// control plane is gone.
func (r *realGatewayLister) ClearGatewayClassFinalizer(ctx context.Context, seedNamespace string) error {
	shootClient, err := r.shootClient(ctx, seedNamespace)
	if err != nil {
		return err
	}

	gwc := &gatewayapiv1.GatewayClass{}
	if err := shootClient.Get(ctx, client.ObjectKey{Name: envoygateway.GatewayClassName}, gwc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("failed to get GatewayClass %q in shoot: %w", envoygateway.GatewayClassName, err)
	}

	if err := controllerutils.RemoveFinalizers(ctx, shootClient, gwc, gatewayExistsFinalizer); err != nil {
		return fmt.Errorf("failed to clear %q finalizer from GatewayClass %q: %w", gatewayExistsFinalizer, envoygateway.GatewayClassName, err)
	}

	return nil
}

// DeleteGateways deletes all Gateway objects in the shoot.
func (r *realGatewayLister) DeleteGateways(ctx context.Context, seedNamespace string) error {
	shootClient, err := r.shootClient(ctx, seedNamespace)
	if err != nil {
		return err
	}

	list := &gatewayapiv1.GatewayList{}
	if err := shootClient.List(ctx, list); err != nil {
		return fmt.Errorf("failed to list Gateways in shoot: %w", err)
	}

	for i := range list.Items {
		gw := &list.Items[i]
		if gw.DeletionTimestamp != nil {
			// Already terminating; nothing to do.
			continue
		}
		if err := shootClient.Delete(ctx, gw); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete Gateway %s/%s in shoot: %w", gw.Namespace, gw.Name, err)
		}
	}

	return nil
}

func (r *realGatewayLister) WaitUntilGatewaysDeleted(ctx context.Context, seedNamespace string) error {
	// lastErr distinguishes a timeout with the API server reachable (Gateways
	// still present, retryable) from one where it was unreachable (terminal).
	var lastErr error

	waitErr := wait.PollUntilContextCancel(ctx, gatewayDeletionPollInterval, true, func(ctx context.Context) (bool, error) {
		list, err := r.fetchGatewayList(ctx, seedNamespace)
		if err != nil {
			// API server likely gone; remember why and keep polling until deadline.
			lastErr = err

			return false, nil
		}

		lastErr = nil

		return len(list.Items) == 0, nil
	})
	if waitErr == nil {
		return nil
	}

	if lastErr != nil {
		return fmt.Errorf("shoot API server unreachable while waiting for Gateways to be deleted: %w", lastErr)
	}

	return ErrGatewaysStillDeleting
}

func (r *realGatewayLister) shootClient(ctx context.Context, seedNamespace string) (client.Client, error) {
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

	return shootClient, nil
}

// gatewaysInUseError indicates that the extension cannot be deleted because
// user-owned Gateway objects still exist in the shoot.
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
