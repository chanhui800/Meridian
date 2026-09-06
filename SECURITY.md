# Security policy

## Supported versions

Security fixes are applied to the latest release on the `main` branch. Upgrade
the Controller and its Agents together when a release notes a compatibility or
protocol change.

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
