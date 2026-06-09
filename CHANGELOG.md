# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.2.0] - 2026-06-09

### Added

- Add version information to the message output.

## [1.1.1] - 2026-06-08

### Added

- Bundle README and LICENSE in the release archives.
- Repository, license, and author links in the READMEs.

## [1.1.0] - 2026-06-08

### Added

- `-s` strips the `docker.io/library/` prefix from the image name.

### Fixed

- Read `io.containerd.image.name` so the image tag is output correctly.

## [1.0.0] - 2026-06-08

First public release.

### Added

- Converts container image `.tar` / `.tar.gz` files (created by `docker save`) into the layout MikroTik RouterOS expects: `manifest.json`, `config.json`, and `layer*.tar` at the top level.
- Supports both OCI-compliant images (`oci-layout` + `index.json`) and docker-archive images (`manifest.json`).
- Platform selection with `-p <PLATFORM>` and tag selection with `-t <TAG>` to pick a single image from multi-arch or multi-tag archives.
- Decompresses gzip-compressed layers to plain `tar`; accepts gzip-compressed input and produces gzip-compressed output based on the file extension.
- `-d` dry run reports whether conversion is possible without writing output.
- `-f` overwrites an existing output file; without it, an existing `<OUT_FILE>` is left untouched.
- Skips conversion when the image already meets the RouterOS layout, reporting exit code `1`.
- Defined exit codes: `0` success, `1` conversion not needed, `2` conversion failed, `3` argument error.
