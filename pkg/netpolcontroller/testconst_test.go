// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package netpolcontroller

// Shared string constants for the package's tests. Extracted to satisfy the
// goconst linter (repeated literals across the test files) and to keep the
// fixtures consistent.
const (
	testNsGateway = "gw-ns"
	testNsTeamA   = "team-a"
	testNsTeamB   = "team-b"
	testNsBackend = "backend-ns"
	testNsOther   = "other"

	testAppLabel      = "app"
	testBackendName   = "backend"
	testGrantName     = "grant"
	testKindHTTPRoute = "HTTPRoute"
)
