# Asset Service

Asset Service is intended to be a generic service for authenticated asset
uploads, storage, processing, and delivery. This repository is designed to be
public-ready.

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
- Do not treat a Go service, a particular hosting provider, or an edge runtime
  as a committed architecture until it is documented as a decision.
