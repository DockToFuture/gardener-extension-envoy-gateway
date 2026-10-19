# Connectivity scenarios

Self-contained manifests for discussing and e2e-testing the data-plane NetworkPolicy controller.
Every scenario creates its own namespaces, each with a `default-deny` (Ingress+Egress) policy, so
nothing is reachable unless the controller (or, where noted, a hand-written policy) opens it.
Backends are `go-httpbin`; where a scenario has two backends they are split by path (`/a`, `/b`).

Two cross-namespace axes recur:

- **Route attachment** — route namespace vs Gateway namespace. When they differ the Gateway needs
  `allowedRoutes.namespaces.from: All`.
- **BackendRef** — backend namespace vs route namespace. When they differ the backend namespace
  needs a `ReferenceGrant`.

| # | Description |
|---|-------------|
| 01 | One Gateway, one route, one Service — all in one namespace (baseline). |
| 02 | One Gateway, two routes, two Services — all in one namespace. |
| 03 | One Gateway, one route referencing two Services — all in one namespace. |
| 04 | Gateway + route in one namespace, Service in another (cross-namespace backendRef + grant). |
| 05 | Gateway + two routes in one namespace, two Services in another (cross-namespace backendRefs). |
| 06 | Gateway + one route referencing two Services in another namespace. |
| 07 | Gateway in one namespace; route and Service in another (cross-namespace route attachment). |
| 08 | Gateway in one namespace; two routes and two Services in another. |
| 09 | Gateway in one namespace; one route referencing two Services, all in another namespace. |
| 10 | One Gateway serving a same-namespace route and a cross-namespace route (both axes at once). |
| 11 | One Gateway, two backends in two different namespaces. |
| 12 | Cross-namespace backendRef with no ReferenceGrant — expected NOT reachable (negative control). |
| 13 | Cross-namespace route attach with `allowedRoutes: Same` — expected NOT reachable (negative control). |
| 14 | Selectorless (hand-managed) backend Service — controller skips it; reachability is hand-written. |
| 15 | Two Gateways in two namespaces sharing one backend namespace. |

Each file's header comment spells out the axes, required ingredients, and expected result.
