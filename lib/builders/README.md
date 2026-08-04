# Builders

Builders give repeated Hypeman builds an explicit, persistent BuildKit cache.
Without a Builder, every build uses a new 3 GB in-memory BuildKit root and loses
its local cache when the build VM exits. With a Builder, builds reuse the same
cache disk across otherwise disposable build VMs.

A Builder is useful when the same application, repository, or build pipeline is
built repeatedly. Reusing one lets BuildKit retain downloaded base-image data,
intermediate layers, and build metadata, reducing subsequent build time and
registry traffic.

## Identity and isolation

Each Builder has an opaque ID and owns one cache disk. Pass that ID as
`builder_id` when creating a build. The ID—not the optional display name, API
credential, or caller identity—is the cache boundary.

Names and tags are organizational metadata. Names are optional and are not
unique, so automation should always store and use the Builder ID.

Omitting `builder_id` preserves the original build behavior: the build runs with
a temporary 3 GB BuildKit root and does not retain local BuildKit state.

## Build behavior

A build that targets a Builder still runs in a new disposable VM. Hypeman
attaches the Builder's disk at `/var/lib/buildkit` for the duration of the build,
then detaches it when VM execution ends. Source archives, build secrets, and VM
configuration are not stored on the Builder disk.

One build runs on a Builder at a time. Additional builds for the same Builder
wait in submission order without consuming a global build-execution slot. Use
separate Builders when independent build streams need to run concurrently or
must not share cache state.

Builder details report the active build, queued build IDs, and
`max_concurrency`, which is fixed at `1`.

## Cache lifecycle

Creating a Builder eagerly creates its fixed-size ext4 cache disk. Disk size is
immutable; create a new Builder to use a different size.

BuildKit garbage collection is derived from the disk size and reclaims cache
before the disk fills. Cache contents are an optimization, not durable build
output: Hypeman may recreate an empty disk after missing storage, interrupted
cleanup, or an incompatible BuildKit version.

`POST /builders/{id}/prune` clears cache contents while preserving the Builder's
ID, name, tags, and disk size. Pruning is asynchronous: the Builder reports
`pruning` until a fresh disk is ready. Builds, pruning, and deletion are mutually
exclusive for a given Builder.

`DELETE /builders/{id}` permanently removes both the Builder and its cache disk.
Deletion does not retain cache state and is rejected while the Builder has
queued or active work.

## Operational model

Builders are local to the Hypeman server that owns their disk. Their metadata
survives process restarts, and startup recovery reconciles interrupted prune or
delete operations and stale VM attachments.

The initial implementation deliberately keeps build VMs disposable. It does not
provide warm Builder VMs, concurrent BuildKit solves on one Builder, cache
migration between servers, or a durability guarantee for cached data.
