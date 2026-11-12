# Security policy

## Reporting a vulnerability

Report suspected vulnerabilities privately to **maci.stgn@gmail.com**.
Include the affected commit, attacker prerequisites, impact, and a minimal
reproducer. Do not publish exploit payloads, guest data, or credentials in a
public issue. There is no promised response or fix-shipped SLA.

## Supported versions

The project is pre-1.0 and carries no API stability promise
(`CHANGELOG.md:9`). Every release is supported for security fixes on the
current line only; there is no long-term support branch.

| Version | Supported |
|---|---|
| 0.8.x (current) | yes |
| 0.7.x and older | no |

## What counts as a security issue

The threat model in `docs/THREAT_MODEL.md` is the authority on scope. In
short, a report is in scope when it concerns:

- Reaching a privileged, `hostPID` migration Job, or the `katamaran-mgr`
  ClusterRole, through the dashboard API (`docs/THREAT_MODEL.md` B1, B4) or
  through the `Migration` CRD (B9).
- Reading or altering guest memory or disk images in flight (B7), which
  cross node boundaries unencrypted.
- Controlling the destination QEMU's argument vector, and through it the
  guest, through a crafted source pod log (B6). The binary executed is pinned
  and is not attacker-chosen.
- Reaching the factory gRPC socket or the QMP socket on a node (B5).
- Bypassing the target screening on the dashboard load generators (B1).

Not in scope: performance or availability findings that require the attacker's
own workload, and anything needing physical access to a node.

## Deployment posture the threat model assumes

These are operator choices, not project defaults, and they are what the
threat model's severity ratings are stated against:

- The dashboard has no authentication of its own
  (`internal/dashboard/authorization.go:23`). The shipped manifests publish
  it as the cluster-wide ClusterIP `katamaran-dashboard-metrics`
  (`deploy/metrics-services.yaml:36`), so every pod in the cluster can reach
  it; restricting it to `kubectl port-forward` by a cluster admin is an
  operator choice the default install does not make.
- `KATAMARAN_MIGRATION_IMAGE` is expected to be set to a digest-pinned image
  the operator trusts (`README.md:776`).
- The `katamaran-adopted` runtime class is optional and experimental
  (`AGENTS.md`).

That ClusterIP Service is the default install, not an extra step, so the
dashboard is on an internet-adjacent boundary unless an operator removes the
Service. The B1 ratings in the threat model are stated against that default.
