# Turborepo <=> Bazel remote cache proxy

[Turborepo]: https://turbo.build/

## Introduction

`tbc` is a proxy tool that enables Turborepo to utilize [a variety of open-source and
commercially available caches built for Bazel](https://github.com/bazelbuild/remote-apis?tab=readme-ov-file#servers).

The main motivation for building `tbc` was to leverage existing remote cache infrastructure.
At [Plaid](https://plaid.com/), a Bazel cache was already set up, and we didn't want
to deploy another cache (nor we wanted to use Bazel to build TS/JS, but
that's a different story :).

## Installation

`go install github.com/be9/tbc@latest`

## Usage

`tbc` is a small CLI app: just wrap `turbo run`:

```
tbc --host bazel.proxy.host:1234 turbo run build
```

When you run the above command, `tbc`:

* Connects to the specified remote cache host.
* Starts an HTTP proxy at `127.0.0.1:8080`.
* Invokes `turbo run build` with the following environment variables set:
    * `TURBO_API`
    * `TURBO_TOKEN`
    * `TURBO_TEAM`

These variables enable remote caching in Turborepo.

## Configuration

### Secure Proxy Connection

To use TLS for the cache connection, add the following options to your command:

```bash
tbc --host bazel.proxy.host:1234 \
    --tls_client_certificate /path/to/cert.pem --tls_client_key /path/to/key.pem \
    turbo run build
```

Alternatively, you can set the environment variables:

```bash
export TBC_CLIENT_CERT=/path/to/cert.pem
export TBC_CLIENT_KEY=/path/to/key.pem
```

### Summary

The `--summary` option makes `tbc` print cache stats upon exit.

Example outputs for a monorepo with two packages:

`2024/07/08 10:43:49 INFO server stats uploads=2 downloads_not_found=2 ul_bytes=858231`:
Turborepo couldn't find artifacts in the remote cache, so it built and uploaded them.

`2024/07/08 10:43:50 INFO server stats downloads=2 dl_bytes=858231`: Two artifacts were
downloaded from the remote cache; the build was skipped.

`2024/07/08 09:52:02 INFO server stats cache_requests=0`: All
artifacts were found in the [local task cache](https://turbo.build/repo/docs/crafting-your-repository/caching);
the remote cache wasn't used.

### Cache Invalidation and Disabling

`tbc` uses `teamId` that originates from `--team` value passed to Turborepo
(or `TURBO_TEAM` variable) to scope cache keys. That is, changing the "team" value would
effectively invalidate the cache.

The `--auto-env` option (enabled by default) sets `TURBO_TEAM=ignore`, but it won't overwrite
the variable if it's already set:

```bash
# Bump version here if you want to bust the existing cache:
export TURBO_TEAM=cache-version-1
tbc --host bazel.proxy.host:1234 turbo run build
```

Adding `--disable` would make `tbc` just run the passed command without starting the proxy server.

### Robust Builds with `--ignore-failures`

On startup, `tbc` connects to the remote cache server and checks its capabilities. Should there
be any failures, `tbc` would exit prematurely.

To work around possible remote cache malfunction, use the `--ignore-failures` option. In this case
`tbc` would run the wrapped command even if remote cache was misconfigured/unavailable or
proxy server failed to start.

### Artifact Integrity

`tbc` fully
supports [signed artifacts](https://turbo.build/repo/docs/core-concepts/remote-caching#artifact-integrity-and-authenticity-verification).
You need to set `TURBO_REMOTE_CACHE_SIGNATURE_KEY` in the environment
and add this snippet to your `turbo.json`:

```json
{
  "remoteCache": {
    "signature": true
  }
}
```

## How It Works Under the Hood

For every key, a
pseudo [`Command`](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto#L555C1-L564)
with the key,
and
an [`Action`](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto#L480)
are created.
These two protobuf messages and the binary artifact are uploaded
to [CAS](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto#L341).
The three hashes are consolidated
in [`ActionResult`](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto#L1056)
passed to `ActionCache.UpdateActionResult`. `ActionResult` also stores metadata like `X-Artifact-Tag` used for signing.

To retrieve the artifact, the key is again mapped to `Command` and `Action`; feeding `Action`
to `ActionCache.GetActionResult` yields
`ActionResult` with the SHA256 of the artifact...

Artifact uploading and downloading
utilizes [Bytestream API](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto).
