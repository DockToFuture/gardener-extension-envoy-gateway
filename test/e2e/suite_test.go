// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

// Package e2e_test contains end-to-end tests for the envoy-gateway
// extension. Tests are gated on environment variables and skip when no
// Gardener landscape is configured.
package e2e_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "envoy-gateway E2E Test Suite")
}
