# infra/dev-ca — dev-only internal CA

Generates a throwaway certificate authority and one cert+key pair per
Ration service, so mTLS between internal services (`pkg/mtls` on the Go
side, `services/ledger-core/src/mtls.rs` on the Rust side) can be
exercised locally and in the demo without any real PKI.

**Dev-only — do not use any of this in a real deployment.** The CA
private key lands on disk in plaintext right next to everything it
signs, certs live for a year with no rotation story, and there is no
revocation. A real deployment should issue short-lived service
identities from a managed CA instead — AWS Private CA / ACM PCA,
HashiCorp Vault's PKI secrets engine, or a service mesh's built-in mTLS
(Istio/Linkerd) — none of which this script tries to be.

## Usage

```sh
infra/dev-ca/generate-dev-ca.sh            # writes to infra/dev-ca/out/
infra/dev-ca/generate-dev-ca.sh /some/dir  # or a custom output dir
```

Output (`out/` is gitignored — regenerate it, don't commit it):

| File | What |
|---|---|
| `ca.pem` / `ca.key` | The dev CA's own self-signed cert/key. |
| `<service>-cert.pem` / `<service>-key.pem` | One identity per service (`ledger-core`, `gateway`, `audit-log`, `agent`, `cli`, `tui`), signed by `ca.pem`. Each cert carries `serverAuth` **and** `clientAuth` EKUs and SANs for `<service>`, `localhost`, `127.0.0.1`, since every internal service here both serves its own RPCs and dials others. |
| `dev-ca.env` | `export FOO_MTLS_ENABLED=true` / `_CERT_FILE` / `_KEY_FILE` / `_CA_FILE` for every service, matching the env var convention `pkg/mtls.LoadFromEnv(prefix)` (Go) and ledger-core's Rust loader read. |

To actually require mTLS for a local run:

```sh
set -a
source infra/dev-ca/out/dev-ca.env
set +a
make build
./bin/ledger-core &          # now requires + verifies a client cert
./bin/gateway &               # dials ledger-core with its own cert,
                               # and requires one from its own callers
```

Leave the env vars unset (the default) and everything runs exactly as
it did before this existed — plaintext gRPC, no certs required. Every
service in this repo treats mTLS as opt-in, matching the
`AIRWALLEX_MODE=mock|live` pattern already used for the Airwallex
integration.

## Why one identity reused as both server and client cert

Ration's internal services form a small, symmetric mesh (gateway dials
ledger-core, ledger-core best-effort-mirrors into audit-log, the
TUI/CLI dial gateway directly) rather than a strict client/server
hierarchy, so issuing a single cert per service with both EKU bits set
is simpler to operate than maintaining separate server and client
identities per service — at dev-CA trust-boundary granularity (see
`docs/THREAT-MODEL.md`) this distinction doesn't buy anything a real
deployment's managed CA wouldn't already handle through separate
short-lived certs per workload identity anyway.
