# Releases

## Versioning

This project uses [Semantic Versioning](https://semver.org/).

## Creating a Release

1. Ensure `main` is in a releasable state (CI passing)
2. Tag the release: `git tag v1.0.0 && git push origin v1.0.0`
3. GitHub Actions builds binaries and creates the release automatically

## Getting Binaries

### Released Version
Download from the [Releases](https://github.com/kernel/hypeman/releases) page.

### Building from Source
```bash
git clone https://github.com/kernel/hypeman
cd hypeman
make build
# Binary at ./bin/hypeman
```

## Prereleases

For release candidates before major versions, use semver prerelease syntax:
```
v2.0.0-rc.1 → v2.0.0-rc.2 → v2.0.0
```

Prerelease tags are incremented, not replaced.
