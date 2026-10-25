# OpenCode guidance attribution

routerd2's AGENTS.md adapts selected language-independent ideas from OpenCode:

- Validate unknown values at the owning boundary and use typed values internally.
- Keep cohesive work together; extract helpers for real reuse or meaningful boundaries.
- Avoid redundant variables, aliases, indirection, and speculative generalization.
- Prefer a clear normal flow, early exits, and nearby supporting functions.
- Keep pure work synchronous and comments focused on non-obvious reasons.
- Use real implementation paths in tests rather than duplicating their logic.
- Keep durable facts minimal and derive views instead of duplicating mutable state.
- Keep dependency direction and actual runtime behavior explicit.

The rules are adapted to Go and to routerd2's requirements. Bun, TypeScript,
Effect, UI, and OpenCode-specific session conventions are not imported.

Reviewed on 2026-10-25 at commit
`84578e123ffd31e31a174457e7350961da4fabbc`.

- [Upstream AGENTS.md](https://github.com/anomalyco/opencode/blob/84578e123ffd31e31a174457e7350961da4fabbc/AGENTS.md)
- [Upstream LICENSE](https://github.com/anomalyco/opencode/blob/84578e123ffd31e31a174457e7350961da4fabbc/LICENSE)

Copyright (c) 2025 opencode

The full upstream MIT License is retained in [LICENSE](LICENSE).
This attribution covers adapted development guidance and does not change
routerd2's existing product license.
