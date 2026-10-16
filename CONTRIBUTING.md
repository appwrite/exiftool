# Contributing

See the [README](README.md#requirements) for prerequisites and instructions to
build and run the service.

## Development setup

If you use [mise](https://mise.jdx.dev/), the repository pins Go and exposes
the common development tasks:

```sh
mise install
mise run ci         # formatting, vet, race-enabled tests, and build
mise run exiftool   # download and verify the pinned ExifTool release
```

GitHub Actions runs `mise run ci` and the ExifTool integration tests. It also
builds the Docker image for both `linux/amd64` and `linux/arm64` on pull
requests and pushes to `main`.

## Tests

Run the unit tests with:

```sh
make test
```

They need neither ExifTool nor network access. To also run the tests against
the pinned ExifTool release:

```sh
make test-integration
```

This downloads and verifies ExifTool if it is missing. The integration suite
fails rather than silently skipping when ExifTool is unavailable.

## Upgrading ExifTool

Production releases are published on
[CPAN](https://metacpan.org/dist/Image-ExifTool). To upgrade, update the
version and SHA-256 checksum in both `Makefile` and `Dockerfile`. CPAN lists
checksums in the author's
[CHECKSUMS](https://cpan.metacpan.org/authors/id/E/EX/EXIFTOOL/CHECKSUMS) file.

## Layout

```text
cmd/exiftool-server/   HTTP server, lifecycle, and telemetry
```
