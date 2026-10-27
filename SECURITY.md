# Security policy

## Supported versions

Security fixes land on `main` and in the latest release only. This is a
boilerplate, not a long-term-support product: pin a release tag and follow the
changelog. Pre-release tags (`v1.2.3-rc.1`) are for testing and are not
supported in production.

| Version | Supported |
|---------|-----------|
| latest stable `vX.Y.Z` release | yes |
| `main` | fixes land here first |
| older releases, pre-releases | no |

## Reporting a vulnerability

Report privately through GitHub Security Advisories: open the repository's
**Security** tab, then **Advisories** → **Report a vulnerability**. Do not open
a public issue, discussion, or pull request for a suspected vulnerability.

Please include:

- the affected version, tag, or commit;
- the component and route/permission involved;
- reproduction steps or a proof of concept;
- the impact you believe it has (data exposure, tenant escape, privilege
  escalation, denial of service, supply-chain compromise);
- any suggested fix or mitigation.

Do not include real production credentials, personal data, or third-party
systems in a report. Use synthetic tenants and users.

### What to expect

- Acknowledgement within 3 business days.
- Triage and a severity assessment within 10 business days.
- A fix or documented mitigation for confirmed high/critical issues within 30
  days; lower severities are scheduled with normal maintenance.
- Credit in the advisory unless you ask to remain anonymous. There is no bug
  bounty.

We will coordinate disclosure with you. Please give us a reasonable window to
ship a fix before publishing details.

## Scope

In scope:

- authentication, sessions, refresh-token rotation, MFA, and API keys;
- tenant isolation, row-level security, and authorization/rank checks;
- file upload/download, storage paths, and malware-scanning bypasses;
- outbound webhooks (SSRF, signature verification, secret handling) and the
  inbound receiver;
- secret handling, logging of sensitive data, and the job payload encryption;
- the release supply chain: workflow definitions, image contents, signatures,
  and provenance.

Out of scope:

- automated scanner output with no demonstrated impact;
- the sample secrets and defaults documented for local development
  (`APP_ENV=local|test`); they deliberately fail production validation;
- denial of service from unbounded traffic without an application-level bypass;
- vulnerabilities in a dependency with no exploitable path in this code (report
  them upstream too; `govulncheck` runs in CI);
- misconfiguration of a deployment's own cloud, database, or DNS.

## Security model and supply chain

Design details are in [`docs/security-model.md`](docs/security-model.md) and
[`docs/operations.md`](docs/operations.md). Operational guarantees:

- Release images are built from a tag whose commit is an ancestor of `main`,
  scanned, signed keyless with cosign, and attested with SLSA build provenance
  and per-platform SBOMs (`actions/attest-build-provenance`,
  `actions/attest-sbom`).
- Deployments verify both the cosign signature and the provenance attestation
  before rollout, and roll out by digest.
- The optional `deploy/k8s/components/image-policy` component enforces the same
  keyless identity at admission.
- CI runs `govulncheck`, CodeQL, and OpenSSF Scorecard; GitHub Actions and
  container images are pinned by commit SHA and digest.
