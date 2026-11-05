# Changelog

## [v1.3.0] - 2026-11-05

- Add backend-neutral access logging through `IAccessLogger`, `AccessLoggerFunc`, and a slog adapter.
- Add independent lifecycle/payload policies, route/method resolution, and per-event filtering for errors, slow requests, or application-specific rules.
- Add optional compact header values without losing multi-value headers, and separate request/response body limits.
- Preserve existing slog, skip, capture configuration, log messages, and default output behavior; document their replacements.
- Add regression tests and a runnable example for route policies, filtered capture, detached headers, output precedence, and adapters.

## [v1.2.0] - 2026-11-04

Typed handlers and standard-context support; source-isolated binding; composable responses; configurable logging, recovery and request IDs.

Requires Go 1.27.3 or later.

This update retains the v1 module path and includes API changes; see the README migration section.
