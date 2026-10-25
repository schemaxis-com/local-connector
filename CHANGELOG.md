# Changelog

What changed in each release of the SchemAxis Local Connector, newest first.
Every section below is published as the notes of the release with the same
version.

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
