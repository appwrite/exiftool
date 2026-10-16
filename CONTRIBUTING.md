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

They need neither ExifTool nor network access: a fake stripper covers raw and
multipart uploads, request limits, error responses, deadlines and cancellation,
bounded uploads and concurrency, temporary file cleanup, and shutdown. To also
run the tests against the pinned ExifTool release:

```sh
make test-integration
```

This downloads and verifies ExifTool if it is missing. The integration suite
tags JPEG, PNG, TIFF, GIF, WebP, AVIF, and HEIC files with a location, camera,
and capture time, strips them, and checks that every private tag is gone while
the orientation is kept. A JPEG also checks that its ICC profile is kept byte
for byte. The non-JPEG and non-PNG fixtures in `internal/exiftool/testdata`
are 32x24 gradients made with ImageMagick 7.1
(`magick -size 32x24 gradient:red-blue photo.<format>`). It also covers files ExifTool cannot rewrite, recovery after a timeout
or an exited process, concurrent requests, and that a failed startup leaves no
processes behind. It fails rather than silently skipping when ExifTool is
unavailable.

## Upgrading ExifTool

Production releases are published on
[CPAN](https://metacpan.org/dist/Image-ExifTool). To upgrade, update the
version and SHA-256 checksum in both `Makefile` and `Dockerfile`. CPAN lists
checksums in the author's
[CHECKSUMS](https://cpan.metacpan.org/authors/id/E/EX/EXIFTOOL/CHECKSUMS) file.

## Layout

```text
cmd/exiftool-server/   HTTP server, lifecycle, and telemetry
internal/exiftool/     ExifTool process pool and metadata removal
```
