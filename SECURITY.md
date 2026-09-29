# Security policy

## Reporting a vulnerability

No private disclosure channel is configured for this project. Until one is,
there is no address to send a report to and no response time to expect. This
file does not name one, because inventing a contact is worse than recording
the gap: a reader who trusts a listed address will wait on it.

Setting up a channel is a decision for the project owner, not for a
contributor editing this file. Until it exists, treat the absence as a known
gap, not as permission to publish a vulnerability in a public issue.

## Supported versions

The project is pre-1.0 and carries no API stability promise
(`CHANGELOG.md:9`). Every release is supported for security fixes on the
current line only; there is no long-term support branch.

| Version | Supported |
|---|---|
| 0.6.x (current) | yes |
| 0.5.x and older | no |

## What counts as a security issue

The threat model in `docs/THREAT_MODEL.md` is the authority on scope. In
short, a report is in scope when it concerns:

- Reaching a privileged, `hostPID` migration Job, or the `katamaran-mgr`
  ClusterRole, through the dashboard API (`docs/THREAT_MODEL.md` B1, B4) or
  through the `Migration` CRD (B9).
- Reading or altering guest memory or disk images in flight (B7), which
  cross node boundaries unencrypted.
- Controlling the destination QEMU process through a crafted source pod log
  (B6).
- Reaching the factory gRPC socket or the QMP socket on a node (B5).
- Bypassing the target screening on the dashboard load generators (B1).

Not in scope: performance or availability findings that require the attacker's
own workload, and anything needing physical access to a node.

## Deployment posture the threat model assumes

These are operator choices, not project defaults, and they are what the
threat model's severity ratings are stated against:

- The dashboard is expected to be reached over `kubectl port-forward` by a
  cluster admin. It has no authentication of its own
  (`internal/dashboard/authorization.go:23`).
- `KATAMARAN_MIGRATION_IMAGE` is expected to be set to a digest-pinned image
  the operator trusts (`README.md:720`).
- The `katamaran-adopted` runtime class is optional and experimental
  (`AGENTS.md`).

Deploying the dashboard with a ClusterIP Service reachable by every pod in the
cluster puts it on an internet-adjacent boundary, and the ratings in the threat
model for B1 get worse, not the model's accuracy.
