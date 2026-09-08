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
)

// fakeLister implements GatewayLister for unit tests without touching a real
// shoot API server.
type fakeLister struct {
	names []string
	err   error
}

func (f *fakeLister) ListGateways(_ context.Context, _ string) ([]string, error) {
	return f.names, f.err
}

func newActuatorForTest(t *testing.T, lister GatewayLister) *Actuator {
	t.Helper()

	return &Actuator{gatewayLister: lister}
}

func TestCheckNoUserGateways_ShootBeingDeleted_BypassesGuard(t *testing.T) {
	a := newActuatorForTest(t, &fakeLister{names: []string{"default/gw"}})
	now := metav1.NewTime(time.Now())
	shoot := &gardencorev1beta1.Shoot{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}}

	if err := a.checkNoUserGateways(context.Background(), logr.Discard(), shoot, "shoot--p--c"); err != nil {
		t.Fatalf("expected nil error when shoot is being deleted, got: %v", err)
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
