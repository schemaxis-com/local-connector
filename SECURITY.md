# Security policy

## Reporting a vulnerability

If you believe you have found a security issue in the connector agent, please
report it privately so it can be fixed before it is disclosed.

Email **security@schemaxis.com** with:

- what you found and where in the code,
- how to reproduce it,
- what an attacker could do with it.

You will get an acknowledgement, and we will keep you posted while we work on a
fix. Please give us a reasonable window to release one before disclosing the
issue publicly.

## Scope

This repository is the local connector agent only. It is a read-only SQL proxy
that runs inside a customer's network; the platform it connects to is a separate,
private system. Issues in the platform, its API, or its web application are out
of scope here. If you are unsure which side an issue belongs to, report it
through the channel above and we will route it.

## What the agent is designed to guarantee

These are the properties worth testing against, each enforced in code you can
read in this repository:

- It executes only read queries: statements that modify data or structure, row
  locks, sequence increments, PL/SQL embedded in a query and chained statements
  are refused (`internal/proxy/readonly.go`).
- It only opens outbound connections; nothing can connect in to it over the
  tunnel (`internal/tunnel/client.go`).
- It sends the agent token and query results only over TLS (`wss://`); plain
  `ws://` is accepted on the local machine only, for development
  (`internal/tunnel/url.go`).
- The local web UI binds to loopback by default and is never exposed to the
  network unless the operator explicitly asks for it (`internal/manager/web.go`).
- Oracle credentials and the agent token are encrypted at rest with a key
  generated on the local machine (`internal/manager/crypto.go`).
