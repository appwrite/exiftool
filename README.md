# exiftool

`exiftool` is a small Go HTTP service built around
[ExifTool](https://exiftool.org/), Phil Harvey's library for reading and
writing image, video, and document metadata. The service checks for ExifTool
at startup and refuses to start without it.

It removes metadata such as location, camera, and capture time from images
while keeping the ICC color profile and orientation, so a stripped photo looks
the same as the original. Pixel data is never re-encoded. JPEG, PNG, TIFF, GIF,
WebP, AVIF, and HEIC are tested. A fixed pool of ExifTool processes runs in
`-stay_open` mode, so each request skips Perl's startup and memory does not
grow with the number of callers. Uploads are written straight to a temporary
file and the response is sent from it, so memory does not grow with file size
either.

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
| `MAX_CONCURRENT_STRIPS` | available CPUs | Number of ExifTool processes, and so of files stripped concurrently |
| `STRIP_TIMEOUT` | `30s` | Maximum lifetime of a strip request, including upload and queue waits |
| `MAX_REQUEST_SIZE` | `25MiB` | Maximum `/strip` request body, including multipart overhead. Uploads are kept on disk, so this caps temporary files, not memory |
| `SHUTDOWN_TIMEOUT` | `30s` | Grace period for active requests during shutdown |

Each ExifTool process uses about 50 MiB, so memory depends on
`MAX_CONCURRENT_STRIPS`, not on how many clients call the service or how
large their files are. With eight processes and 16 concurrent 18 MB uploads,
the HTTP server itself peaked at 14 MiB, and 340 requests for 320 KB JPEGs, 34
at a time, completed at about 385 per second with a p95 of about 110 ms.

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
`/healthz` is an alias for `/readyz`. The service only starts listening after
every ExifTool process has started.

Send a file to strip as the raw request body:

```sh
curl -sS -X POST http://localhost:8080/strip \
  -H 'Content-Type: application/octet-stream' \
  --data-binary '@photo.jpg' -o stripped.jpg
```

Or as a multipart `file` field:

```sh
curl -sS -X POST http://localhost:8080/strip \
  -F 'file=@photo.jpg' -o stripped.jpg
```

A successful response is `200` with the stripped file as
`application/octet-stream`. The format is detected from the content, so a
renamed file is still stripped. Errors are JSON with an `error` message:

| Status | Meaning |
| --- | --- |
| `400` | Missing or invalid body |
| `413` | Body is larger than `MAX_REQUEST_SIZE` |
| `422` | ExifTool cannot rewrite the file: the format is not writable or the file is corrupt |
| `499` | The client cancelled the request |
| `500` | ExifTool failed; its process is replaced before the next request uses it |
| `503` | The service is shutting down; retry after `Retry-After` |
| `504` | The request took longer than `STRIP_TIMEOUT` |

Callers that must not lose an upload should keep the original file on any
response other than `200`.

## Telemetry and shutdown

The service writes structured JSON logs to stdout. Each HTTP request receives
an `X-Request-ID`. A caller-supplied ID is kept when it contains only ASCII
letters, digits, `_`, or `-` and is at most 64 characters. Logs never include
request bodies, query strings, or request headers.

Prometheus metrics are exposed at `/metrics`. They cover HTTP request rates and
latency, strip stage latency and outcomes, active ExifTool commands,
cancellations, Go runtime and process statistics, and build information
including the ExifTool version. Keep this endpoint private at the ingress layer.

Every strip request has a deadline. A request that times out or is cancelled
stops its ExifTool process, which is started again the next time it is needed,
so one stuck file never blocks other requests.

On SIGINT or SIGTERM, readiness becomes false immediately, and active requests
have `SHUTDOWN_TIMEOUT` to finish. After that, request contexts are cancelled
and connections are closed, and the ExifTool processes are stopped. Set the
orchestrator's termination grace period longer than `SHUTDOWN_TIMEOUT`.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, tests, and the
project layout.

## License

This service is released under the [MIT License](LICENSE). The Docker image
also includes ExifTool, which is distributed under the same terms as Perl
itself.
