# Security Audit — 2026-08-11

## Scope

The review covered authentication and authorization, browser security, public API serialization, request bounds, cache behavior, database access, worker endpoints, dependency vulnerabilities, containers, deployment examples, and the sign-in conversion surfaces changed in this release.

Three independent review passes examined authentication, API/data handling, and deployment/runtime configuration. Findings were reproduced against the code before remediation.

## Findings and disposition

| Severity | Finding | Disposition |
| --- | --- | --- |
| High | Account API keys inherited administrator and worker privileges through the shared user lookup path. | Fixed. Privileged browser routes now require a session-backed user; account keys remain limited to account/API use. Regression tests cover administrator, worker, and key-rotation paths. |
| High | Public paper JSON and the general metadata LRU retained full PDF text and internal filesystem paths. | Fixed. Internal fields are excluded from JSON and metadata lookups omit `pdf_text`; full text is loaded only by the dedicated PDF-text flow. |
| High | `.secrets/` could enter the Docker build context and cache. | Fixed. Secret directories and private-key extensions are excluded by `.dockerignore`; the validated application build context contains no secret files. |
| High | The Qwen worker dependency set included known Transformers remote-code-execution advisories. | Fixed. Worker dependencies were upgraded to a compatible, vulnerability-free pinned set. The worker now runs as an unprivileged user. |
| High | The deployed PostgreSQL password was weak. | Fixed operationally. The database role and mounted application secrets were atomically rotated to a randomly generated 64-character credential with mode `0600`; application health was verified afterward. No credential is stored in Git. |
| Medium | Opening Account or API setup silently created and displayed a raw API key. | Fixed. GET requests now perform metadata-only lookup; key creation and rotation require an explicit session-backed POST. Secret-bearing pages use `private, no-store`. |
| Medium | Third-party scripts could run while an authenticated user browsed public pages. | Reduced. Analytics is disabled for signed-in users and all remaining pinned CDN scripts use Subresource Integrity and anonymous CORS. Sensitive account, login, API setup, authentication, and administration pages load no analytics. |
| Medium | Metadata fetch identifiers could alter the upstream arXiv query and upstream bodies were unbounded. | Fixed. arXiv IDs use an exact allowlisted grammar, query strings use `url.Values`, and Atom/HTML responses have explicit size limits. |
| Medium | Worker JSON bodies, author cache keys, PDF search, and local graph rebuilds lacked dedicated resource bounds. | Fixed. Worker bodies are capped, author parameters are bounded, PDF search requires authentication with rate/concurrency limits, and graph rebuild is single-flight. |
| Medium | Local mode accepted browser requests through arbitrary Host/Origin combinations. | Fixed. Local browser traffic is restricted to loopback hosts and same-origin mutation requests while command-line clients remain supported. |
| Medium | Forwarded client-IP headers could bypass rate limits if the origin was directly reachable. | Fixed. Forwarded headers are accepted only when explicitly enabled and the direct peer is a loopback or private-network proxy. |
| Medium | API and MCP errors could expose backend details. | Fixed. Client responses are generic and detailed errors remain server-side logs. |
| Medium | Deployment examples encouraged unauthenticated plaintext remote inference. | Fixed. Examples use a loopback endpoint reached through an authenticated SSH tunnel. |

## Validation

- `go test ./... -count=1`
- `go test -race ./... -count=1 -timeout 240s`
- `go vet ./...`
- `govulncheck ./...` — no vulnerabilities found
- Direct pinned Python dependency audits for the application tools and Qwen worker — no known vulnerabilities found
- Python compilation and shell syntax checks
- Docker Compose configuration rendering
- Production application image build
- `git diff --check`
- Runtime application health check after credential rotation

## Residual risks

- Python auditing covered every directly pinned package. The environment lacks Python `venv`/`ensurepip`, so a separately resolved transitive lockfile audit was not available; the production image successfully resolved and installed the dependency set.
- Base container tags are versioned but not all are digest-pinned. Pin and intentionally refresh image digests as a follow-up supply-chain hardening task.
- PostgreSQL was already managed as a long-running container outside the current Compose project labels. It is healthy and attached to the external application network, but the next maintenance window should reconcile it with the documented Compose lifecycle to prevent network-alias drift.
