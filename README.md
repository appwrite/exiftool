# exiftool

`exiftool` is a small Go HTTP service built around
[ExifTool](https://exiftool.org/), Phil Harvey's library for reading and
writing image, video, and document metadata. The service checks for ExifTool
at startup and refuses to start without it.

## Requirements

- Go 1.27 or newer
- Perl 5, to run ExifTool
- ExifTool. `make exiftool` downloads the pinned release (13.55) from CPAN and
  verifies its SHA-256 checksum.

## Build and run

```sh
make build
make run
```

`make run` uses the pinned ExifTool from `make exiftool`. To use an ExifTool
already installed on your system, run the binary directly:

```sh
EXIFTOOL_PATH=$(command -v exiftool) ./exiftool-server
```

The server listens on `:8080`. These environment variables are available:

| Variable | Default | Purpose |
| --- | --- | --- |
| `ADDR` | `:8080` | HTTP listen address |
| `EXIFTOOL_PATH` | `exiftool` | ExifTool executable, either a path or a name looked up on `PATH` |
| `SHUTDOWN_TIMEOUT` | `30s` | Grace period for active requests during shutdown |

## Docker

The image downloads ExifTool from CPAN, verifies its checksum during the build,
and runs it with Debian's Perl as an unprivileged user.

```sh
docker build -t exiftool .
docker run --rm -p 8080:8080 exiftool
```

### Container releases

Publishing a GitHub Release with a semantic version tag such as `v1.2.3`
builds and pushes a multi-architecture image to GitHub Container Registry:

```text
ghcr.io/appwrite/exiftool:1.2.3
ghcr.io/appwrite/exiftool:1.2
ghcr.io/appwrite/exiftool:1
ghcr.io/appwrite/exiftool:latest
```

Prereleases receive only their full version tag and do not update `latest`.
Published images include build provenance and an SBOM.

## API

Liveness and readiness checks:

```sh
curl -sS http://localhost:8080/livez
curl -sS http://localhost:8080/readyz
```

```json
{
  "status": "ok"
}
```

`/livez` reports that the process is running. `/readyz` reports whether the
service accepts requests and returns 503 as soon as graceful shutdown starts.
`/healthz` is an alias for `/readyz`.

## Telemetry and shutdown

The service writes structured JSON logs to stdout. Each HTTP request receives
an `X-Request-ID`. A caller-supplied ID is kept when it contains only ASCII
letters, digits, `_`, or `-` and is at most 64 characters. Logs never include
request bodies, query strings, or request headers.

Prometheus metrics are exposed at `/metrics`. They cover HTTP request rates and
latency, Go runtime and process statistics, and build information including the
ExifTool version. Keep this endpoint private at the ingress layer.

On SIGINT or SIGTERM, readiness becomes false immediately, and active requests
have `SHUTDOWN_TIMEOUT` to finish. After that, request contexts are cancelled
and connections are closed. Set the orchestrator's termination grace period
longer than `SHUTDOWN_TIMEOUT`.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, tests, and the
project layout.

## License

This service is released under the [MIT License](LICENSE). The Docker image
also includes ExifTool, which is distributed under the same terms as Perl
itself.
