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
