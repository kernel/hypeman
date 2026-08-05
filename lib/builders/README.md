# Builders

Builders retain build cache across repeated Hypeman builds. Without a Builder,
each build gets a temporary 3 GB cache that disappears with its build VM. With
a Builder, disposable build VMs reuse one persistent cache disk.

A Builder is useful for an application, repository, or pipeline that is built
repeatedly. Reusing it reduces repeated downloads and work in later builds.

## Identity and isolation

Each Builder has an opaque ID and owns one cache disk. Pass that ID as
`builder_id` when creating a build. The ID—not the optional name or the caller's
credentials—is the cache boundary.

Names are optional and non-unique. Tags support filtering. Automation should
store and use the Builder ID.

Omitting `builder_id` preserves the original build behavior: Hypeman uses a
temporary 3 GB cache and does not retain its contents. There is no implicit or
default Builder.

## Build behavior

A build that targets a Builder still runs in a new disposable VM. Hypeman
attaches the Builder's cache disk for the build and detaches it when VM execution
ends. Source archives, build secrets, and VM configuration are not stored on the
Builder disk.

One build runs on a Builder at a time. Additional builds for the same Builder
wait in submission order. Use separate Builders when build streams need to run
concurrently or must not share cache state.

Builder details report the active build, queued build IDs, and
`max_concurrency`. These are point-in-time values; `max_concurrency` is currently
fixed at `1`.

## Cache lifecycle

Creating a Builder creates a fixed-size cache disk. Disk size cannot be changed;
create a new Builder to use a different size.

The current build backend protects cache usage below 70% of disk capacity and
allows it to grow to 90% before garbage collection reclaims entries. Cache
contents are an optimization, not durable build output. Hypeman may recreate an
empty disk after missing storage, interrupted cleanup, or an incompatible
backend version.

`POST /builders/{id}/prune` clears cache contents while preserving the Builder's
ID, name, tags, and disk size. Pruning is asynchronous: the Builder reports
`pruning` until a fresh disk is ready. Builds, pruning, and deletion are mutually
exclusive for a given Builder.

`DELETE /builders/{id}` permanently removes both the Builder and its cache disk.
Deletion is rejected while the Builder has queued or active work.

## Hypeman restarts

Builders' metadata survives Hypeman process restarts. Startup recovery
reconciles interrupted prune or delete operations and stale VM attachments.
Builds that were queued or running during the restart may be started again.

Builders are local to the Hypeman server that owns their disk. The initial
implementation does not provide warm Builder VMs, concurrent builds on one
Builder, cache migration between servers, or a durability guarantee for cached
data.
