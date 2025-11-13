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

The CLI follows a resource-based command structure:

```sh
hypeman [resource] [command] [flags]
```

```sh
hypeman health check
```

For details about specific commands, use the `--help` flag.

## Global Flags

- `--debug` - Enable debug logging (includes HTTP request/response details)
- `--version`, `-v` - Show the CLI version
