# routerd2 Development Policy

## 1. Highest priority: make the router solve the real problem

**Bring routerd2 to its first release by making it work with real peer devices and clients.**
Implement the needed routing functions with a simple, understandable architecture.
Judge completion by the forwarding, interoperability, failure handling, and recovery the feature actually needs.
Passing tests, increasing coverage, or producing more code and reports does not establish that the problem is solved.

**Unnecessary defensive production code and test code are prohibited.**
Every addition must serve the function being built or a reachable failure implied by real protocol, OS, resource, or persistence constraints.
Prefer removing the cause, state, branch, or layer over adding another guard around it.
Optimize for the understandable whole system, including its callers, data flow, and lifetime.

These project rules replace conflicting local workflow requirements, including mandatory test-first commits and test-only PRs.
Current explicit user instructions and existing task authorization take precedence. Do not request approval already given.
`Must` and `do not` mark requirements and prohibitions; `prefer` marks defaults to apply with engineering judgment.

## 2. New product development: no prototype compatibility obligations

- routerd2 is a distinct new product. It is unreleased, has no deployed users, and is still being designed.
- There is no compatibility obligation to existing routerd2 APIs, IPC protocols, commands, schemas, stored formats, or prototype behavior. This includes interfaces currently used by development code.
- Existing implementation, architecture, ADRs, plans, and tests are working material. Read them for context; change or discard them when a clearer design serves the task. Their existence does not make them requirements.
- Design each feature from the operator's problem and the real protocol, OS, and operating constraints. Do not treat current behavior as its specification.
- Change producers, consumers, callers, stored representations, fixtures, and documentation together. Remove the superseded path as part of the same coherent change.
- Recreate disposable development configuration and state for the chosen format when needed. Do not build migrations between discarded prerelease formats.
- Do not add compatibility readers, adapters, dual representations, fallback APIs, version negotiation, or version matrices to keep prototype interfaces alive.
- Internal messages use the chosen current contract. Update both ends together. Reject unsupported input through ordinary boundary handling instead of imagining old producers and implementing their formats.
- Use routerd as evidence about needed functions and real operational failures. Its architecture and internal compatibility obligations do not transfer to routerd2.
- Implement update and recovery functions when the task calls for them. Any future supported product upgrades start from the first released format, not from discarded prototypes.

## 3. Reach real communication early

1. Identify the operator problem, peer or client, intended result, and what would demonstrate it.
2. Read the actual production path, callers, data ownership, and deployment setup.
3. Resolve unknown device, OS, or protocol behavior in the smallest suitable authorized environment.
4. Implement a useful complete path and exercise real communication early.
5. Diagnose observed failures, fix their cause, check the affected behavior, and continue toward the release.

- Use independent peers or established implementations when checking interoperability. Two copies of our own logic agreeing is insufficient evidence.
- Exercise networking behavior with actual product processes and the kernel. Inspect relevant packets, peer or client results, routes, withdrawal, failover, and recovery.
- VMs and network namespaces can establish the real kernel behavior they exercise. They do not establish qualification of a target physical device, NIC, or performance level.
- Choose the environment for the behavior in doubt. Do not impose a mandatory ladder of unit tests, VMs, cloud tests, and physical tests.
- State required setup. Do not rely on leftover configuration or manual repair to make the product appear to work.
- If an environment is unavailable, identify the minimum missing setup and continue independent work. Report the resulting limit on what was demonstrated.
- Do not make a comprehensive unit suite, fake environment, or fault-injection framework a prerequisite for useful communication with a real peer.

## 4. Keep architecture, state, and contracts small

- Trace the operation from input through decisions to its actual effect. Keep that flow visible in the code.
- Check what the feature needs and what production callers actually use. Remove parameters, returned data, and APIs that have no purpose in the chosen design; update callers and tests with them.
- Give mutable state and external side effects clear owners. An owner can be an ordinary value or function; it does not require a new manager, registry, service, or process.
- Fix conflicting ownership, dependencies, lifecycle, and ordering at their source. Do not compensate with retries, reconciliation, and guards in every caller.
- Separate operator intent from observations and derived execution state. Do not write learned values back into the intent.
- Store facts and recovery state the feature needs. Derive other views rather than maintaining duplicate mutable truth.
- Keep a representation appropriate to each real boundary. Do not serialize typed values merely to move them between internal functions.
- Separate work determined by configuration from decisions that depend on observations or time. Keep useful static results with the configuration that produced them instead of repeatedly rebuilding them during observation updates.
- Do not construct a simulated producer lifecycle, registry, or background system just to turn already available values into a temporary snapshot.
- Prefer one explicit contract over optional required capabilities, omission flags, and alternative paths. Remove a redundant state representation before adding a cache, validity flag, or synchronization mechanism.
- Choose package, executable, process, and instance boundaries for actual ownership, privileges, dependencies, or failure isolation. Their existing number and arrangement are not constraints.
- Keep dependencies directed. Domain data and pure rules should not depend on transport, CLI, or process orchestration. Do not create extra layers solely to enforce this rule.
- Keep protocol engines and their dependencies out of the boot or supervisor path unless that path needs them.

## 5. Validation, ownership, and data flow

- Decode unknown input at the boundary that owns it. Validate and normalize it there, then pass typed values inward.
- Rely on invariants actually established by a type, construction, or preceding check in the operation. A struct type alone does not prove semantic validity.
- Do not repeatedly parse, hash, encode, decode, or inspect unchanged data merely because it crosses an internal function.
- Pass information already available to the operation directly to its consumer. Do not reread files or reconstruct objects to recover values still available on the call stack.
- Prepare canonical bytes once when the operation needs them. Do not reconstruct a returned object if the caller does not need it.
- Make sharing and mutation explicit: use read-only data, transfer ownership, or copy the mutable parts at the boundary that needs to change them. Do not use serialization round trips as generic cloning.
- Recheck a property when its premise can change through new external input, concurrent mutation, elapsed time, or a wait. Put the check with the responsible operation, not in every forwarding layer.
- Account for actual retries, process restarts, and delayed or expired observations in the design being built. These do not justify supporting discarded versions.
- Establish authentication, integrity, conflict handling, and write atomicity where the feature requires them. Design that boundary directly; existing validation functions, state fields, and acknowledgement mechanisms are not requirements in themselves.
- Do not turn a batch that must reject invalid input before writing into a sequence of partial writes while removing duplicate checks.
- Do not introduce a universal `Validated<T>` framework, wrapper hierarchy, validation cache, or flags to avoid passing a small typed value directly.

## 6. Write idiomatic, direct Go

### Packages and APIs

- Use the toolchain declared by `go.mod`, the standard library, and suitable existing dependencies. Do not introduce a library for a trivial helper.
- Give packages specific responsibilities. Avoid dumping grounds named `util`, `common`, `helpers`, or `base`.
- Keep identifiers unexported unless another package needs them. Do not export production hooks or APIs solely for tests.
- Use short, contextual names and standard Go initialisms: `ID`, `HTTP`, `URL`, `JSON`. Avoid redundant package-name stutter.
- Keep normal import names. Alias only for a real collision or a misleading name; do not use dot imports.
- Avoid hidden setup in `init`, mutable package globals, and implicit service registries. Make dependencies and lifecycle visible.
- Regenerate generated artifacts from their definitions; do not hand-edit generated output.

### Types, interfaces, and construction

- Start with concrete types and ordinary functions.
- Define a small interface at the consuming boundary when actual substitution or separation is needed. Do not create an interface for every struct or anticipated implementation.
- Return concrete implementations unless the abstraction itself is the API. Do not design production interfaces around mocks.
- Required capabilities belong in the type contract. Let the compiler check them instead of probing for required methods at runtime and taking fallback paths.
- Do not use `any`, `interface{}`, or `map[string]any` for application messages, state, dependencies, parameters, results, fields, or collections. Define the actual data and operations.
- Decode application messages into explicit structs and named types. Use typed discriminators and explicit payload types for real variants, not arbitrary values and runtime guesses.
- Pass concrete values directly to standard-library or existing dependency APIs that accept `any`; `json.Marshal(cfg)` is fine. Do not add adapters, per-type encoders, wrappers, or generic forwarding functions merely to avoid an upstream signature.
- When an upstream API requires or returns dynamic values, confine them to that boundary and narrow application data before passing it inward.
- `any` as a type-parameter constraint is allowed for a real shared typed algorithm. Do not use aliases, empty interfaces, generics, reflection, or serialized payloads to disguise the same untyped application contract.
- Prefer useful zero values and struct literals. Use constructors to establish invariants or acquire resources, not merely to wrap literals.
- Construct the needed object once. Do not create a default instance and immediately discard it for another constructor.
- Do not add builders, functional options, factories, or variadic pseudo-options for a fixed set of required arguments.
- Introduce generics when a real operation is shared across concrete types. Do not turn a few direct operations into a framework.
- Use value receivers for appropriate small values and pointer receivers for mutation, identity, or noncopyable state. Never copy a used mutex; do not use pointers everywhere by habit.
- Do not add getters, setters, aliases, or wrappers that only repeat a field or method without enforcing a meaningful boundary.

### Functions, variables, and control flow

- Keep a cohesive operation together. Extract a function for actual reuse, a named domain operation, or a meaningful resource lifetime, error boundary, or external side effect with clear inputs and results.
- A single caller does not make such a function wrong. A one-use helper that merely renames an expression or forwards arguments usually adds indirection.
- Let the main function show the normal flow and keep supporting helpers nearby. Do not impose function-length quotas.
- Return early on errors and invalid input. Avoid `else` after an unconditional return and long `if err == nil` ladders.
- Prefer a clear `switch` for distinct states over dense conditions or several independent booleans encoding the same state.
- Use short declarations and inferred local types where clear. Use `const` for constants and keep variables in the smallest useful scope.
- Avoid redundant aliases and trivial one-use temporaries. Keep a named local when it explains meaning, captures one observation, or clarifies error handling.
- Prefer direct field access over copying fields into aliases that hide the owner.
- Use straightforward `for` and `range` loops. Do not import map/filter pipelines or callback chains from another language.
- Keep pure parsing, calculation, and transformation synchronous. Add cancellation or concurrency when the work actually needs it.
- Do not compress code into unreadable expressions to reduce line count. Reduce branches, mutable state, and indirection.
- Run `gofmt`; do not maintain hand-aligned formatting.

### Errors and resource ownership

- Handle errors explicitly. Add useful operation context with `%w` when callers need the cause; inspect causes with `errors.Is` or `errors.As`.
- Do not wrap the same error at every forwarding layer, flatten it with `errors.New(err.Error())`, or parse error strings for control flow.
- Let the responsible boundary report a failure once. Avoid logging and returning the same error at every layer.
- Do not swallow errors, fabricate success, or use broad `recover` handlers to keep invalid state running. Ordinary failures use errors, not panics.
- Do not repeat nil checks for dependencies whose presence is established by construction. If absence is a supported state, represent its meaning.
- Make ownership of files, sockets, transactions, locks, and buffers clear. Put cleanup beside acquisition and use `defer` when its lifetime is appropriate.
- Do not add retries, backoff, fallbacks, or cache invalidation to hide an unexplained failure. Give necessary retries one owner, stopping conditions, and bounds appropriate to the operation.
- Comments explain non-obvious constraints, ownership, and reasons. Do not narrate assignments or justify avoidable complexity with defensive commentary.

### Concurrency and lifetime

- Use synchronous calls by default. Start a goroutine only for work requiring concurrent progress.
- Each goroutine needs an owner, termination conditions, and a clear completion or error path.
- Pass the relevant context explicitly as the first parameter through blocking work. Do not discard cancellation during ordinary work or keep a request context in a long-lived object.
- Cleanup or recovery that must outlive a request needs an explicit owner and a bounded lifetime.
- Do not use context values as an option bag or dependency container. Trivial pure helpers need no context.
- Prefer one owner for mutable state. Use mutexes for appropriate shared state and channels for actual communication; do not layer actors, locks, and queues without a concrete need.
- Tie lock scope to the protected state. Do not hold shared locks or scarce database connections across unrelated parsing, encoding, or slow I/O.
- Bound queues, work, and waits where the operation needs limits. Do not build a general scheduler or worker-pool framework for one simple operation.
- Remove duplicate work before considering caches or pools. Add them only for a demonstrated need with a clear owner and lifetime.

## 7. Tests must earn their cost

- Unnecessary defensive test code is prohibited. Make the requirement or reproduced failure evident from the test name, inputs, and assertions; no separate justification document is required.
- Use focused local tests for deterministic parsing, calculation, diagnosis, or useful regression prevention. Test count and coverage percentage are not product goals.
- Test observable results through the real implementation. Do not copy its algorithm or state machine into an expected-result generator.
- Avoid elaborate mocks, global replacement hooks, impossible-state matrices, and assertions that freeze private implementation details.
- Put test-only helpers in `*_test.go`. Do not add production wrappers, optional parameters, or alternate entry points solely for tests.
- Rewrite or delete tests tied to discarded designs. A test passing against the current prototype is not a reason to preserve its API or behavior.
- Automate useful communication checks with the actual product binary and a small fixture. Do not build a parallel simulator of the product.
- Select existing checks by the function being changed and the uncertainty being resolved. Their age or existence does not make their assumptions requirements.
- Rerun or broaden checks for relevant changes, failures, environment differences, or unresolved concerns. Group related real-environment work instead of repeating costly suites without a reason.
- Never claim an unrun check passed, weaken the intended result to hide failure, or treat a test-runner failure as a product result.
- Use and update the project `Makefile` for applicable build and check commands. `make check` provides supporting Go/Python checks; it does not establish real communication.
- Documentation-only changes need content, reference, and diff review, not a router test campaign.

## 8. Product behavior and execution discipline

- Operator configuration expresses intent. Reapplying it should converge on the intended managed state without duplicate effects or manual repairs.
- Distinguish managed kernel resources from other resources. Do not adopt or remove resources without ownership; choose a clear ownership mechanism for the design.
- Base actions needing current state on current observations. Represent missing, stale, and unknown information honestly instead of reporting healthy state.
- Build Linux Go release artifacts through the project build path with `CGO_ENABLED=0`. Update that path with the design instead of deploying unrelated ad hoc builds.
- Keep externally active behavior under explicit product configuration. A coding task does not by itself authorize changes to production or cloud environments.
- Use the authorized environment and scope. Check management access and recovery before real network changes; do not ask again for permission already provided.
- Protect concurrent work. Use isolated worktrees when concurrent code changes would conflict, and stage explicit paths rather than `git add -A` or `git add .`.
- Publishing, merging, releasing, deployment, destructive actions, and billable resource changes follow the actual task authorization. Do not invent an additional approval workflow for routine implementation.
- Keep credentials, private keys, session data, and unnecessary private infrastructure details out of publication. Retain applicable licenses and notices for reused material.
- Report the executed version, relevant setup, peer or client, observed result, and untested scope concisely. Distinguish implementation progress, real communication, and physical-device qualification.
- If changed code fails the intended function, fix the cause and pause affected publication or deployment. Continue independent work.
- Finish the requested outcome. Do not extend the task with speculative robustness work or new issues after it is complete.

## 9. Simplification is part of feature implementation

Before completing a change, inspect its production call path and diff.

- Remove superseded code, obsolete tests, unused results and parameters, redundant checks, and temporary scaffolding made unnecessary by the new design.
- Check whether using the actual owner and already available data can eliminate an abstraction, state field, branch, conversion, or background task.
- Judge the result by the concepts, mutable state, and execution paths a maintainer needs to understand. Neither preserving code nor rewriting code is a goal in itself.
- Work through the complete feature path that advances the release. Do not make a repository-wide cleanup campaign a prerequisite for implementing that feature.
- Keep this review proportional to the change. Do not create a framework, lint system, report bureaucracy, recurring verification loop, or per-helper approval requirement to enforce simplicity.

## 10. Communication and sources

- Keep this policy in English. Explain work to the user in Japanese unless asked otherwise.
- Lead with the result and explain the evidence and limits. Distinguish observations, inferences, and proposals.
- In Japanese, translate ordinary operations and states; preserve official names, identifiers, paths, commands, logs, and values. Define unfamiliar abbreviations, use consistent terms, and split overloaded sentences.
- Review Japanese output yourself. Preserve code, numeric meaning, table correspondence, and document structure; inspect rendered layouts when relevant.
- Do not use Gemini API or Gemini CLI for Japanese proofreading, artifact review, or answer checking, including manual calls and hooks. Do not automatically use other external proofreading services.

These rules apply the user's priorities and findings from routerd2 production paths.
The [upstream notice](third_party/opencode-guidance/NOTICE.md) and [MIT License](third_party/opencode-guidance/LICENSE) are retained with this policy.
Related guidance: [OpenCode AGENTS.md](https://github.com/anomalyco/opencode/blob/84578e123ffd31e31a174457e7350961da4fabbc/AGENTS.md)
([upstream MIT license](https://github.com/anomalyco/opencode/blob/84578e123ffd31e31a174457e7350961da4fabbc/LICENSE)),
[Effective Go](https://go.dev/doc/effective_go), [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments),
[When To Use Generics](https://go.dev/blog/when-generics), [Package names](https://go.dev/blog/package-names),
and [Context](https://go.dev/blog/context).
Use the applicable Go idioms without importing another language's syntax preferences or turning guidance into extra architecture.
