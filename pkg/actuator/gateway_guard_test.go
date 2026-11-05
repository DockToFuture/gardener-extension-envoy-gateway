// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// testSeedNamespace is the seed-side control-plane namespace used across the
// gateway-guard tests.
const testSeedNamespace = "shoot--p--c"

// testGatewayName is a user Gateway (namespace/name) used across the tests.
const testGatewayName = "default/gw"

// fakeLister implements GatewayLister for unit tests without touching a real
// shoot API server.
type fakeLister struct {
	names []string
	err   error

	clearErr          error
	clearedNamespaces []string

	deleteErr         error
	deletedNamespaces []string

	waitErr        error
	waitNamespaces []string
}

func (f *fakeLister) ListGateways(_ context.Context, _ string) ([]string, error) {
	return f.names, f.err
}

func (f *fakeLister) ClearGatewayClassFinalizer(_ context.Context, seedNamespace string) error {
	f.clearedNamespaces = append(f.clearedNamespaces, seedNamespace)

	return f.clearErr
}

func (f *fakeLister) DeleteGateways(_ context.Context, seedNamespace string) error {
	f.deletedNamespaces = append(f.deletedNamespaces, seedNamespace)

	return f.deleteErr
}

func (f *fakeLister) WaitUntilGatewaysDeleted(_ context.Context, seedNamespace string) error {
	f.waitNamespaces = append(f.waitNamespaces, seedNamespace)

	return f.waitErr
}

func newActuatorForTest(t *testing.T, lister GatewayLister) *Actuator {
	t.Helper()

	return &Actuator{gatewayLister: lister}
}

func TestDrainUserGatewaysForShootDeletion_HappyDrain(t *testing.T) {
	f := &fakeLister{names: []string{testGatewayName}}
	a := newActuatorForTest(t, f)

	if err := a.drainUserGatewaysForShootDeletion(context.Background(), logr.Discard(), testSeedNamespace); err != nil {
		t.Fatalf("expected nil error on a clean drain, got: %v", err)
	}

	if got := f.deletedNamespaces; len(got) != 1 || got[0] != testSeedNamespace {
		t.Errorf("expected DeleteGateways called once, got: %v", got)
	}
	if got := f.waitNamespaces; len(got) != 1 || got[0] != testSeedNamespace {
		t.Errorf("expected WaitUntilGatewaysDeleted called once, got: %v", got)
	}
	if got := f.clearedNamespaces; len(got) != 1 || got[0] != testSeedNamespace {
		t.Errorf("expected ClearGatewayClassFinalizer called once, got: %v", got)
	}
}

func TestDrainUserGatewaysForShootDeletion_StillDraining_Requeues(t *testing.T) {
	f := &fakeLister{waitErr: ErrGatewaysStillDeleting}
	a := newActuatorForTest(t, f)

	// Gateways still present with a reachable API server must requeue, not
	// proceed to clear the finalizer and orphan LB Services.
	err := a.drainUserGatewaysForShootDeletion(context.Background(), logr.Discard(), testSeedNamespace)
	if !errors.Is(err, ErrGatewaysStillDeleting) {
		t.Fatalf("expected ErrGatewaysStillDeleting, got: %v", err)
	}
	if len(f.clearedNamespaces) != 0 {
		t.Errorf("expected finalizer NOT cleared while requeuing, got: %v", f.clearedNamespaces)
	}
}

func TestDrainUserGatewaysForShootDeletion_APIServerGone_DoesNotWedge(t *testing.T) {
	f := &fakeLister{waitErr: errors.New("shoot API server unreachable: connection refused")}
	a := newActuatorForTest(t, f)

	// An unreachable API server must not wedge deletion: proceed and clear.
	if err := a.drainUserGatewaysForShootDeletion(context.Background(), logr.Discard(), testSeedNamespace); err != nil {
		t.Fatalf("expected nil error when API server is gone, got: %v", err)
	}
	if len(f.clearedNamespaces) != 1 {
		t.Errorf("expected finalizer cleared after giving up the wait, got: %v", f.clearedNamespaces)
	}
}

func TestCheckNoUserGateways_ShootBeingDeleted_WaitsForGateways(t *testing.T) {
	f := &fakeLister{names: []string{testGatewayName}}
	a := newActuatorForTest(t, f)
	now := metav1.NewTime(time.Now())
	shoot := &gardencorev1beta1.Shoot{DeletionTimestamp: &now}

	if err := a.checkNoUserGateways(context.Background(), logr.Discard(), shoot, testSeedNamespace); err != nil {
		t.Fatalf("expected nil error when shoot is being deleted, got: %v", err)
	}

	if got := f.deletedNamespaces; len(got) != 1 || got[0] != testSeedNamespace {
		t.Errorf("expected DeleteGateways called once, got: %v", got)
	}
	if got := f.waitNamespaces; len(got) != 1 || got[0] != testSeedNamespace {
		t.Errorf("expected WaitUntilGatewaysDeleted called once, got: %v", got)
	}
}

func TestCheckNoUserGateways_ShootBeingDeleted_GatewaysStillPresent_Requeues(t *testing.T) {
	f := &fakeLister{waitErr: ErrGatewaysStillDeleting}
	a := newActuatorForTest(t, f)
	now := metav1.NewTime(time.Now())
	shoot := &gardencorev1beta1.Shoot{DeletionTimestamp: &now}

	// Gateways still present with a reachable API server must requeue, not
	// proceed to clear the finalizer and orphan LB Services.
	err := a.checkNoUserGateways(context.Background(), logr.Discard(), shoot, testSeedNamespace)
	if !errors.Is(err, ErrGatewaysStillDeleting) {
		t.Fatalf("expected ErrGatewaysStillDeleting, got: %v", err)
	}
	if len(f.clearedNamespaces) != 0 {
		t.Errorf("expected finalizer NOT cleared while requeuing, got: %v", f.clearedNamespaces)
	}
}

func TestCheckNoUserGateways_ShootBeingDeleted_APIServerGone_DoesNotWedge(t *testing.T) {
	f := &fakeLister{waitErr: errors.New("shoot API server unreachable: connection refused")}
	a := newActuatorForTest(t, f)
	now := metav1.NewTime(time.Now())
	shoot := &gardencorev1beta1.Shoot{DeletionTimestamp: &now}

	// An unreachable API server must not wedge deletion: proceed and clear.
	if err := a.checkNoUserGateways(context.Background(), logr.Discard(), shoot, testSeedNamespace); err != nil {
		t.Fatalf("expected nil error when API server is gone, got: %v", err)
	}
	if len(f.clearedNamespaces) != 1 {
		t.Errorf("expected finalizer cleared after giving up the wait, got: %v", f.clearedNamespaces)
	}
}

func TestCheckNoUserGateways_ShootBeingDeleted_DeleteErrorDoesNotWedge(t *testing.T) {
	f := &fakeLister{deleteErr: errors.New("shoot api server unreachable")}
	a := newActuatorForTest(t, f)
	now := metav1.NewTime(time.Now())
	shoot := &gardencorev1beta1.Shoot{DeletionTimestamp: &now}

	// A delete failure is logged, not fatal; the flow still reaches the finalizer.
	if err := a.checkNoUserGateways(context.Background(), logr.Discard(), shoot, testSeedNamespace); err != nil {
		t.Fatalf("expected nil error even when deleting Gateways fails, got: %v", err)
	}
	if len(f.deletedNamespaces) != 1 {
		t.Errorf("expected DeleteGateways to have been attempted once, got: %v", f.deletedNamespaces)
	}
	if len(f.clearedNamespaces) != 1 {
		t.Errorf("expected ClearGatewayClassFinalizer to still run after a delete error, got: %v", f.clearedNamespaces)
	}
}

func TestCheckNoUserGateways_ShootBeingDeleted_BypassesGuard(t *testing.T) {
	f := &fakeLister{names: []string{testGatewayName}}
	a := newActuatorForTest(t, f)
	now := metav1.NewTime(time.Now())
	shoot := &gardencorev1beta1.Shoot{DeletionTimestamp: &now}

	if err := a.checkNoUserGateways(context.Background(), logr.Discard(), shoot, testSeedNamespace); err != nil {
		t.Fatalf("expected nil error when shoot is being deleted, got: %v", err)
	}

	// Guard is bypassed, but the leftover GatewayClass finalizer must still be
	// cleared so teardown can complete.
	if got := f.clearedNamespaces; len(got) != 1 || got[0] != testSeedNamespace {
		t.Errorf("expected ClearGatewayClassFinalizer called once with the seed namespace, got: %v", got)
	}
}

func TestCheckNoUserGateways_ShootBeingDeleted_ClearErrorDoesNotBlock(t *testing.T) {
	f := &fakeLister{clearErr: errors.New("shoot api server unreachable")}
	a := newActuatorForTest(t, f)
	now := metav1.NewTime(time.Now())
	shoot := &gardencorev1beta1.Shoot{DeletionTimestamp: &now}

	// A finalizer-clear failure must not turn a clean delete into a hang.
	if err := a.checkNoUserGateways(context.Background(), logr.Discard(), shoot, testSeedNamespace); err != nil {
		t.Fatalf("expected nil error even when clearing the finalizer fails, got: %v", err)
	}
	if len(f.clearedNamespaces) != 1 {
		t.Errorf("expected ClearGatewayClassFinalizer to have been attempted once, got: %v", f.clearedNamespaces)
	}
}

func TestCheckNoUserGateways_NoGateways_Passes(t *testing.T) {
	a := newActuatorForTest(t, &fakeLister{names: nil})

	if err := a.checkNoUserGateways(context.Background(), logr.Discard(), &gardencorev1beta1.Shoot{}, testSeedNamespace); err != nil {
		t.Fatalf("expected nil error when no Gateways exist, got: %v", err)
	}
}

func TestCheckNoUserGateways_LiveGateways_Refused(t *testing.T) {
	a := newActuatorForTest(t, &fakeLister{names: []string{"default/gw1", "team-a/gw2"}})

	err := a.checkNoUserGateways(context.Background(), logr.Discard(), &gardencorev1beta1.Shoot{}, testSeedNamespace)
	if err == nil {
		t.Fatal("expected error when user Gateways exist, got nil")
	}

	if _, ok := errors.AsType[*gatewaysInUseError](err); !ok {
		t.Fatalf("expected *gatewaysInUseError, got %T: %v", err, err)
	}

	if !strings.Contains(err.Error(), "default/gw1") || !strings.Contains(err.Error(), "team-a/gw2") {
		t.Errorf("error message should reference the offending Gateways, got: %v", err)
	}
}

func TestCheckNoUserGateways_ListError_Surfaced(t *testing.T) {
	want := errors.New("api server unreachable")
	a := newActuatorForTest(t, &fakeLister{err: want})

	err := a.checkNoUserGateways(context.Background(), logr.Discard(), &gardencorev1beta1.Shoot{}, testSeedNamespace)
	if err == nil {
		t.Fatal("expected error to be propagated, got nil")
	}

	if !errors.Is(err, want) {
		t.Errorf("expected wrapped %v, got: %v", want, err)
	}
}

func TestGatewaysInUseError_TruncatesLongLists(t *testing.T) {
	names := []string{"a/0", "a/1", "a/2", "a/3", "a/4", "a/5", "a/6", "a/7"}
	err := &gatewaysInUseError{names: names}
	msg := err.Error()

	if !strings.Contains(msg, "and 3 more") {
		t.Errorf("expected truncation hint in long-list error, got: %s", msg)
	}
}

func TestGatewayCRDAbsent(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "no kind match (CRD never installed)",
			err:  &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: gvr.Group, Kind: "Gateway"}},
			want: true,
		},
		{
			name: "not found",
			err:  apierrors.NewNotFound(gvr.GroupResource(), ""),
			want: true,
		},
		{
			name: "unrelated error is not treated as absent",
			err:  errors.New("shoot API server unreachable: connection refused"),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gatewayCRDAbsent(tc.err); got != tc.want {
				t.Errorf("gatewayCRDAbsent(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
