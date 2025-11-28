# Hypeman CLI

The official CLI for the Hypeman REST API.

It is generated with [Stainless](https://www.stainless.com/).

## Installation

### Installing with Homebrew

```sh
brew tap onkernel/tap
brew install hypeman
```

### Installing with Go

<!-- x-release-please-start-version -->

```sh
go install 'github.com/onkernel/hypeman-cli/cmd/hypeman@latest'
```

### Running Locally

<!-- x-release-please-start-version -->

```sh
go run cmd/hypeman/main.go
```

<!-- x-release-please-end -->

## Usage

```sh
# Pull an image
hypeman pull nginx:alpine

# Run an instance (auto-pulls image if needed)
hypeman run nginx:alpine
hypeman run --name my-app -e PORT=3000 nginx:alpine

# List running instances
hypeman ps
hypeman ps -a    # show all instances

# View logs
hypeman logs <instance-id>
hypeman logs -f <instance-id>   # follow logs

# Execute a command in a running instance
hypeman exec <instance-id> -- /bin/sh
hypeman exec -it <instance-id>  # interactive shell
```

For details about specific commands, use the `--help` flag.

The CLI also provides resource-based commands for more advanced usage:

```sh
hypeman [resource] [command] [flags]
```

## Global Flags

- `--debug` - Enable debug logging (includes HTTP request/response details)
- `--version`, `-v` - Show the CLI version

## Development

### Testing Preview Branches

When developing features in the main [hypeman](https://github.com/onkernel/hypeman) repo, Stainless automatically creates preview branches in `stainless-sdks/hypeman-cli` with your API changes. You can check out these branches locally to test the CLI changes:

```bash
# Checkout preview/<branch> (e.g., if working on "devices" branch in hypeman)
./scripts/checkout-preview devices

# Checkout an exact branch name
./scripts/checkout-preview -b main
./scripts/checkout-preview -b preview/my-feature
```

The script automatically adds the `stainless` remote if it doesn't exist.

After checking out a preview branch, you can build and test the CLI:

```bash
go build -o hypeman ./cmd/hypeman
./hypeman --help
```
