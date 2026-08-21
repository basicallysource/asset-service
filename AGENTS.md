# Asset Service

Asset Service is a generic service for authenticated asset uploads, storage,
processing, and delivery. This repository is public.

Start with [`agent-docs/architecture.md`](agent-docs/architecture.md): it is the
committed design, the invariants, and the list of things deliberately not built
yet with the seam each one arrives at. Change it in the same commit that changes
what it describes.

```
cmd/asset-service      wiring, the operator CLI, graceful shutdown
internal/config        environment -> one validated struct, once
internal/httpx         middleware and the single way to write a response
internal/auth          who is calling, and what they may do
internal/objstore      S3-compatible storage: SigV4, a client, an in-memory double
internal/catalog       SQLite: assets and API keys, with migrations
internal/assets        the domain: hash, name, store, resolve
internal/api           routes and their access rules
deploy                 how a host runs and updates it
```

## Public Repository Rules

- Keep code, configuration, examples, tests, and documentation generally useful
  to external users and contributors.
- Do not commit secrets or material that identifies people, infrastructure,
  customers, or internal operations.
- Do not add special-purpose behavior for one deployment or caller. Generalize
  it, or keep it outside this repository.

## Engineering

- Keep the service minimal, direct, and low-complexity.
- Prefer obvious data flow and few abstractions over framework-like machinery.
- Refactor when extending a feature requires remembering to update several
  places, creates an error-prone path, or makes the code harder to understand.
- Delete obsolete code and paths in the same change.

## Documentation

- `agent-docs/` contains repository-wide documentation for agents and external
  contributors. Keep it accurate, public-safe, and useful without access to
  private context.
- A decision is committed once it is written in `agent-docs/architecture.md`,
  and not before. Anything absent from it is still open.
