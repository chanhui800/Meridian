# Security policy

## Supported versions

Security fixes are developed on `main` and published through GitHub Releases.
Use the latest supported release for production; changes documented on `main`
may not be available in an older release. Upgrade the Controller and its Agents
together when a release notes a compatibility or protocol change.

Agent enrollment and runtime credentials are scoped to authenticated Controller
endpoints. Public release downloads use version-pinned assets and SHA-256
verification without sending Controller credentials to the download host.

## Reporting a vulnerability

Please report suspected vulnerabilities privately through the repository's
GitHub Security Advisories page. Include the affected release, a minimal
reproduction, impact, and any suggested mitigation. Do not open a public issue
for an undisclosed vulnerability.

Never include real Agent tokens, API tokens, JWT secrets, databases, backups,
private keys, host addresses, or other production data in a report. Redact
credentials and use synthetic values in logs and screenshots.

Reports involving authentication, Agent enrollment or updates, TLS certificate
handling, SSRF protections, proxy routing, backups, or cross-site request
forgery are especially useful when they identify the exact endpoint and
deployment mode (Docker or standalone).

## Response process

We will acknowledge a private report, reproduce it in an isolated environment,
assess severity, prepare a fix, and publish release notes after a fix is
available. Please allow reasonable time for remediation before public
disclosure.

## Diagnostic network access

Upstream tests and saved-site diagnostics are administrator features. They
retain support for loopback and private Emby/Jellyfin origins, while diagnostic
connections reject metadata and other special destinations, validate DNS
answers before dialing their numeric addresses, and do not inherit environment
HTTP proxies. HTTP redirects must keep the same authority and protocol; normal
upstream certificate verification remains enabled. Diagnostic requests do not
forward management credentials or expose raw upstream response bodies.

The arbitrary-URL test endpoint also limits concurrent requests and request
rate. These checks protect this diagnostic feature; they do not turn the
administrator's general reverse-proxy configuration into an untrusted sandbox.
Keep management sessions and server egress protected, and use HTTPS for the
production panel. Direct HTTP is a compatibility mode and cannot protect a
session from network interception.

## Code-scanning findings

A successful CodeQL workflow means the analysis ran and uploaded its results.
It does not mean there are no findings. Review the actual data flow and safety
boundary before changing behavior or treating a result as a false positive.
The [finding review](docs/code-scanning-review.md) records the current
diagnostic, media-response, local healthcheck, identity-hashing, redirect, and
transport-dependent cookie boundaries. Changes to those boundaries require a
new review; suppressing an entire rule is not a substitute for verification.
