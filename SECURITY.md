# Security Policy

## Supported Versions

kruntimes has not published a stable release. Security fixes are made on the
`main` branch and will be included in the next available release.

| Version | Supported |
|---------|-----------|
| `main` | Yes |
| Older commits and unreleased forks | No |

## Reporting a Vulnerability

Do not open a public issue for a suspected vulnerability.

Use GitHub's private vulnerability reporting for this repository:

1. Open the repository's **Security** tab.
2. Select **Report a vulnerability**.
3. Include affected versions, impact, reproduction steps, and any suggested
   mitigation.

The maintainers will acknowledge a complete report within seven days and will
coordinate validation, remediation, and disclosure with the reporter.

If private vulnerability reporting is temporarily unavailable, contact a
maintainer listed in [MAINTAINERS.md](MAINTAINERS.md) before sharing details
publicly.

## Security Scope

The built-in Bash and Python Runtime implementations execute multiple Runs
inside shared Runtime Pods. They are intended for trusted workloads and are not
a sandbox for hostile code. Reports that demonstrate an escape from documented
trust boundaries, unauthorized Kubernetes access, cross-namespace access, or
artifact disclosure are in scope.
