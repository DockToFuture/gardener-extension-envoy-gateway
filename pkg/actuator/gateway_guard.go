// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	extensionsconfigv1alpha1 "github.com/gardener/gardener/extensions/pkg/apis/config/v1alpha1"
	extensionsutil "github.com/gardener/gardener/extensions/pkg/util"
	"github.com/gardener/gardener/pkg/controllerutils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
	// extension's GatewayClass in the shoot. It reports whether the finalizer was
	// actually present (and thus removed), so callers can surface a stray
	// finalizer that would otherwise silently wedge teardown.
	ClearGatewayClassFinalizer(ctx context.Context, seedNamespace string) (removed bool, err error)

	// DeleteGateways deletes all Gateway objects across all namespaces in the
	// shoot identified by the given seed-side control-plane namespace.
	DeleteGateways(ctx context.Context, seedNamespace string) error

	// WaitUntilGatewaysDeleted blocks until no Gateway objects remain in the
	// shoot, or the context is done.
	WaitUntilGatewaysDeleted(ctx context.Context, seedNamespace string) error

	// WaitUntilGatewayCreationBlocked blocks until a dry-run create of a Gateway
	// for the extension's GatewayClass is forbidden, i.e. the block-gateway-creation
	// policy is enforcing, or the context deadline is reached.
	WaitUntilGatewayCreationBlocked(ctx context.Context, seedNamespace string) error
}

// realGatewayLister builds a shoot client from the seed and lists Gateway
// resources from the shoot API server.
type realGatewayLister struct {
	seedClient client.Client
}

// NewGatewayLister returns a GatewayLister that talks to a real shoot
// API server via util.NewClientForShoot. This is the production default.
func NewGatewayLister(seedClient client.Client) GatewayLister {
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

// fetchGatewayList builds a shoot-scoped client and returns the Gateways bound
// to this extension's GatewayClass. Gateways of other GatewayClasses are not
// ours and must not block the extension's delete/disable, so they are filtered
// out. The delete guard derives from this.
func (r *realGatewayLister) fetchGatewayList(ctx context.Context, seedNamespace string) (*gatewayapiv1.GatewayList, error) {
	shootClient, err := r.shootClient(ctx, seedNamespace)
	if err != nil {
		return nil, err
	}

	list := &gatewayapiv1.GatewayList{}
	if err := shootClient.List(ctx, list); err != nil {
		// A shoot where the Gateway CRD was never installed (or was already
		// removed) has no user Gateways by definition. Treat the absent kind as
		// an empty list so the delete guard does not error permanently and wedge
		// extension teardown.
		if gatewayCRDAbsent(err) {
			return &gatewayapiv1.GatewayList{}, nil
		}

		return nil, fmt.Errorf("failed to list Gateways in shoot: %w", err)
	}

	list.Items = filterManagedGateways(list.Items)

	return list, nil
}

// filterManagedGateways keeps only Gateways bound to this extension's
// GatewayClass; Gateways of other GatewayClasses are not ours.
func filterManagedGateways(items []gatewayapiv1.Gateway) []gatewayapiv1.Gateway {
	ours := items[:0]
	for _, g := range items {
		if string(g.Spec.GatewayClassName) == envoygateway.GatewayClassName {
			ours = append(ours, g)
		}
	}

	return ours
}

// gatewayCRDAbsent reports whether a List error means the Gateway kind is not
// served by the shoot API server (CRD never installed or already removed), in
// which case there are no user Gateways to guard against.
func gatewayCRDAbsent(err error) bool {
	return meta.IsNoMatchError(err) || apierrors.IsNotFound(err)
}

// ClearGatewayClassFinalizer removes the gateway-exists finalizer from the
// extension's GatewayClass so it can finish deleting once the envoy-gateway
// control plane is gone. It reports whether the finalizer was present.
func (r *realGatewayLister) ClearGatewayClassFinalizer(ctx context.Context, seedNamespace string) (bool, error) {
	shootClient, err := r.shootClient(ctx, seedNamespace)
	if err != nil {
		return false, err
	}

	gwc := &gatewayapiv1.GatewayClass{}
	if err := shootClient.Get(ctx, client.ObjectKey{Name: envoygateway.GatewayClassName}, gwc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("failed to get GatewayClass %q in shoot: %w", envoygateway.GatewayClassName, err)
	}

	if !controllerutil.ContainsFinalizer(gwc, gatewayExistsFinalizer) {
		return false, nil
	}

	if err := controllerutils.RemoveFinalizers(ctx, shootClient, gwc, gatewayExistsFinalizer); err != nil {
		return false, fmt.Errorf("failed to clear %q finalizer from GatewayClass %q: %w", gatewayExistsFinalizer, envoygateway.GatewayClassName, err)
	}

	return true, nil
}

// DeleteGateways deletes the Gateways bound to this extension's GatewayClass in
// the shoot. Gateways of other GatewayClasses are left untouched.
func (r *realGatewayLister) DeleteGateways(ctx context.Context, seedNamespace string) error {
	shootClient, err := r.shootClient(ctx, seedNamespace)
	if err != nil {
		return err
	}

	list, err := r.fetchGatewayList(ctx, seedNamespace)
	if err != nil {
		return err
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

// WaitUntilGatewayCreationBlocked polls with a dry-run Gateway create (never
// persisted) until admission forbids it, proving the block-gateway-creation
// policy is enforcing rather than merely present.
func (r *realGatewayLister) WaitUntilGatewayCreationBlocked(ctx context.Context, seedNamespace string) error {
	shootClient, err := r.shootClient(ctx, seedNamespace)
	if err != nil {
		return err
	}

	probe := func() *gatewayapiv1.Gateway {
		return &gatewayapiv1.Gateway{
			GenerateName: "envoy-gateway-guard-probe-", Namespace: metav1.NamespaceDefault,
			Spec: gatewayapiv1.GatewaySpec{
				GatewayClassName: gatewayapiv1.ObjectName(envoygateway.GatewayClassName),
				Listeners: []gatewayapiv1.Listener{{
					Name:     "probe",
					Protocol: gatewayapiv1.HTTPProtocolType,
					Port:     gatewayapiv1.PortNumber(80),
				}},
			},
		}
	}

	waitErr := wait.PollUntilContextCancel(ctx, gatewayDeletionPollInterval, true, func(ctx context.Context) (bool, error) {
		err := shootClient.Create(ctx, probe(), client.DryRunAll)
		// Only a denial by our policy counts as "enforcing". A bare IsForbidden
		// is ambiguous — an RBAC authorization failure is also 403 — so match the
		// policy name in the message to avoid mistaking a missing permission for
		// the guard being in effect. Admitted or a transient error = keep polling.
		return deniedByGuardPolicy(err), nil
	})
	if waitErr != nil {
		return fmt.Errorf("gave up waiting for the block-gateway-creation policy to take effect: %w", waitErr)
	}

	return nil
}

// deniedByGuardPolicy reports whether err is an admission denial from this
// extension's block-gateway-creation ValidatingAdmissionPolicy, as opposed to
// an RBAC authorization failure (which is also Forbidden) or any other error.
// ValidatingAdmissionPolicy denials surface the policy name in the status
// message, so require both Forbidden and the policy name.
func deniedByGuardPolicy(err error) bool {
	return apierrors.IsForbidden(err) && strings.Contains(err.Error(), envoygateway.GatewayCreateGuardPolicyName)
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
