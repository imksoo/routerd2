# Read-only operator console

A configured `web_console` service runs in the existing Go module executable.
It owns its TCP listener and HTTP lifetime; there is no external web server,
JavaScript build, second resource store, or systemd service.

```yaml
schema_version: 2
resources:
  - id: service/console
    service:
      name: console
      web_console:
        listen: 198.18.0.1:8080
        base_path: /ops/
        title: routerd2
```

Declare the local address and its firewall access separately. The listener uses
HTTP, as the original read-only WebConsole does. `ManagementAccess` import checks
its declared trusted interface/zone and binding; it does not invent a source ACL.

The initial pages display live control health, scoped kernel interfaces/routes,
resource-condition pages, and confirmed-commit metadata. Health failure remains
HTTP 503 with the actual observation. Missing observations and truncated kernel
views remain explicit. Resource condition readiness is not a kernel readback.
The resource continuation uses the control API's existing observation identity;
a concurrent change returns 409 and the operator can start a fresh page.

The old console's title, base path, links, escaped HTML and read-only method
handling are retained. Its resource-store adapter and React status model are
replaced by the current typed control APIs. This is an initial management path:
history search, client inventory, connections, protocol detail, VPN/SAM pages,
and the old complete UI surface are still outstanding. Configuration documents,
mutations, and private worker APIs are not exposed by this listener.
