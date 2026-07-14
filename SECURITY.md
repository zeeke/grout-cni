# Security Policy

## Reporting a vulnerability

If you discover a security vulnerability, please report it responsibly by emailing the maintainers directly rather than opening a public issue.

Include:
- A description of the vulnerability
- Steps to reproduce
- Affected versions

We will acknowledge receipt within 48 hours and aim to provide a fix or mitigation within 7 days for critical issues.

## Scope

grout-cni runs as a CNI binary invoked by kubelet with root privileges. Security-relevant areas include:
- Unix socket communication with grout (local only, no network exposure)
- Network namespace operations
- File lock handling
- Input validation of CNI config and IPAM results
