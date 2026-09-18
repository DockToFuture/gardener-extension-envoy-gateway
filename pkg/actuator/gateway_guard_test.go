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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// fakeLister implements GatewayLister for unit tests without touching a real
// shoot API server.
type fakeLister struct {
	names      []string
	namespaces []string
	err        error

	clearErr          error
	clearedNamespaces []string
}

func (f *fakeLister) ListGateways(_ context.Context, _ string) ([]string, error) {
	return f.names, f.err
}

func (f *fakeLister) ClearGatewayClassFinalizer(_ context.Context, seedNamespace string) error {
	f.clearedNamespaces = append(f.clearedNamespaces, seedNamespace)

	return f.clearErr
}

func (f *fakeLister) ListGatewayNamespaces(_ context.Context, _ string) ([]string, error) {
	return f.namespaces, f.err
}

func newActuatorForTest(t *testing.T, lister GatewayLister) *Actuator {
	t.Helper()

	return &Actuator{gatewayLister: lister}
}

func TestCheckNoUserGateways_ShootBeingDeleted_BypassesGuard(t *testing.T) {
	f := &fakeLister{names: []string{"default/gw"}}
	a := newActuatorForTest(t, f)
	now := metav1.NewTime(time.Now())
	shoot := &gardencorev1beta1.Shoot{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}}

	if err := a.checkNoUserGateways(context.Background(), logr.Discard(), shoot, "shoot--p--c"); err != nil {
		t.Fatalf("expected nil error when shoot is being deleted, got: %v", err)
	}

	// Guard is bypassed, but the leftover GatewayClass finalizer must still be
	// cleared so teardown can complete.
	if got := f.clearedNamespaces; len(got) != 1 || got[0] != "shoot--p--c" {
		t.Errorf("expected ClearGatewayClassFinalizer called once with the seed namespace, got: %v", got)
	}
}

func TestCheckNoUserGateways_ShootBeingDeleted_ClearErrorDoesNotBlock(t *testing.T) {
	f := &fakeLister{clearErr: errors.New("shoot api server unreachable")}
	a := newActuatorForTest(t, f)
	now := metav1.NewTime(time.Now())
	shoot := &gardencorev1beta1.Shoot{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}}

	// A finalizer-clear failure must not turn a clean delete into a hang.
	if err := a.checkNoUserGateways(context.Background(), logr.Discard(), shoot, "shoot--p--c"); err != nil {
		t.Fatalf("expected nil error even when clearing the finalizer fails, got: %v", err)
	}
	if len(f.clearedNamespaces) != 1 {
		t.Errorf("expected ClearGatewayClassFinalizer to have been attempted once, got: %v", f.clearedNamespaces)
	}
}

func TestCheckNoUserGateways_NoGateways_Passes(t *testing.T) {
	a := newActuatorForTest(t, &fakeLister{names: nil})

	if err := a.checkNoUserGateways(context.Background(), logr.Discard(), &gardencorev1beta1.Shoot{}, "shoot--p--c"); err != nil {
		t.Fatalf("expected nil error when no Gateways exist, got: %v", err)
	}
}

func TestCheckNoUserGateways_LiveGateways_Refused(t *testing.T) {
	a := newActuatorForTest(t, &fakeLister{names: []string{"default/gw1", "team-a/gw2"}})

	err := a.checkNoUserGateways(context.Background(), logr.Discard(), &gardencorev1beta1.Shoot{}, "shoot--p--c")
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

	err := a.checkNoUserGateways(context.Background(), logr.Discard(), &gardencorev1beta1.Shoot{}, "shoot--p--c")
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

func TestDedupeGatewayNamespaces_DedupesAndSorts(t *testing.T) {
	list := &gatewayapiv1.GatewayList{
		Items: []gatewayapiv1.Gateway{
			{ObjectMeta: metav1.ObjectMeta{Namespace: nsTeamB, Name: "gw1"}},
			{ObjectMeta: metav1.ObjectMeta{Namespace: nsTeamA, Name: "gw2"}},
			// Second Gateway in team-b must not duplicate the namespace.
			{ObjectMeta: metav1.ObjectMeta{Namespace: nsTeamB, Name: "gw3"}},
		},
	}

	got := dedupeGatewayNamespaces(list)
	want := []string{nsTeamA, nsTeamB}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected sorted deduped namespaces %v, got %v", want, got)
		}
	}
}

func TestDedupeGatewayNamespaces_Empty(t *testing.T) {
	if got := dedupeGatewayNamespaces(&gatewayapiv1.GatewayList{}); len(got) != 0 {
		t.Errorf("expected no namespaces for an empty list, got %v", got)
	}
}
