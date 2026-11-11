# Changelog

What changed in each release of the SchemAxis Local Connector, newest first.

## About

- Connects your Oracle database to the SchemAxis cloud over an outbound WSS
  connection. No inbound port is opened on your network.
- Runs only single read queries. Anything else is refused before it reaches
  the database.
- A local web page to add, test and manage connections. Passwords and tokens
  are stored on your machine, encrypted.
- Source published under the Apache License 2.0.
- Builds for Windows, Linux and macOS. Every release comes with
  `checksums.txt` and a signed build provenance attestation, so you can check
  that a download is intact. INSTALL.md shows how.

## [0.2.0] - 2026-11-11

### Added

- Duplicate a connection to add another schema of the same database
  without typing it all again.

### Changed

- UI polish and visual updates.
- An agent token already used by another connection is refused on save.
- Queries can take as long as the platform allows, so a slow database can be
  given more time.
- Stability improvements.

## [0.1.1] - 2026-10-25

### Added

- Builds for Windows, Linux and macOS, each with `checksums.txt` and a
  signed build provenance attestation, so you can check that a download is
  intact. INSTALL.md shows how.
- The agent itself is unchanged since 0.1.0.

## [0.1.0] - 2026-10-25

### Added

- First public release. Source published under the Apache License 2.0.
- Connects your Oracle database to the SchemAxis cloud over an outbound WSS
  connection. No inbound port is opened on your network.
- Runs only single read queries. Anything else is refused before it reaches
  the database.
- A local web page to add and test connections. Passwords and tokens are
  stored locally, encrypted.
- Builds for Windows, Linux and macOS. Every release comes with
  `checksums.txt` and a signed build provenance attestation, so you can check
  that a download is intact. INSTALL.md shows how.
