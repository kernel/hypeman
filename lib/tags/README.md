# Metadata Tags

This package defines the product contract for user-provided metadata tags used across mutable Hypeman resources.

## What Tags Are

Metadata tags are optional string key/value pairs that let users label resources for ownership, environment, automation, and filtering.

Domain code uses the shared `tags.Metadata` type alias so resource packages reference one metadata type consistently.

Examples:
- `team=backend`
- `env=staging`
- `cost_center=ml-platform`

## Contract

- Type: `map[string]string`
- Maximum entries per resource: `50`
- Key length: `1..128`
- Value length: `0..256`
- Allowed characters in keys/values: `[A-Za-z0-9 _.:/=+@-]`

## Behavior

- Tags are optional.
- Tag filters use exact, case-sensitive matching.
- Multiple filter pairs are ANDed together.
- Resources with no tags do not match non-empty tag filters.

## API Expectations

- Create endpoints accept optional `metadata`.
- Get/List responses include `metadata` when present.
- List endpoints support filtering by `metadata[...]` where enabled.
