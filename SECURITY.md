# Security Policy

## Supported version

Only the latest published OTrans server version receives security fixes.

## Deployment boundary

OTrans does not terminate TLS. Never expose its raw HTTP port directly to the public Internet. Use an HTTPS reverse proxy or a trusted VPN, keep the upstream port private, and preserve WebSocket Upgrade headers.

Transfer metadata and blobs are stored unencrypted on the NAS. Protect the fnOS host, application data directory, backups, and administrator token accordingly.

## Reporting a vulnerability

Please do not open a public issue for a vulnerability that is not yet fixed. Use GitHub's private vulnerability reporting for the `oyster-projects/otrans-server` repository. Include the affected version, deployment topology, reproduction steps, impact, and any suggested remediation.

Do not include real administrator tokens, device tokens, transfer contents, or personal server addresses in a report.
