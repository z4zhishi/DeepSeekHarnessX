# DSHX plugin API

`backend/core` is the stable, implementation-independent contract for plugins. A
plugin declares metadata and capabilities, receives a scoped `core.Context`, and
must release all resources through its returned handle.

## Lifecycle

Plugins are loaded, enabled, and closed by the host. Core owns task and event
resources so a plugin does not need to manage long-lived goroutines.

## Session semantics

Every event and task carries a `SessionContext`. Synchronous work must wait only
for its own session; asynchronous work should publish a result event. Plugins
must not block global locks or other sessions.

## Compatibility

API versions are explicit in `Metadata.APIVersion`. Additive changes preserve old
implementations; breaking changes require a new API version.
