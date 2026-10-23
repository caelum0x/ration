//! ledger-core: the authoritative Rust ledger for Ration.
//!
//! Owns all tenant/deposit/balance/ledger-entry state. Every money-moving
//! operation is written to the crash-safe C write-ahead log
//! (`services/wal`, wrapped safely in [`wal_ffi`]) before the in-process
//! SQLite index is updated. The WAL is the durability source of truth;
//! SQLite is a rebuildable, queryable index over it.

pub mod agent_decision;
pub mod audit_mirror;
pub mod authclient;
pub mod deposit;
pub mod error;
pub mod ledger;
pub mod metrics;
pub mod money;
pub mod mtls;
pub mod telemetry;
pub mod tenant;
pub mod tenantauth;
pub mod wal_crypto;
pub mod wal_ffi;

pub mod grpc;

/// Generated protobuf/gRPC types, nested by proto package
/// (`ration.<package>.v1`) to match the cross-package imports that
/// `common.proto` is used from (`ledger.proto`, `gateway.proto`).
pub mod pb {
    pub mod ration {
        pub mod common {
            pub mod v1 {
                tonic::include_proto!("ration.common.v1");
            }
        }
        pub mod ledger {
            pub mod v1 {
                tonic::include_proto!("ration.ledger.v1");
            }
        }
        pub mod gateway {
            pub mod v1 {
                tonic::include_proto!("ration.gateway.v1");
            }
        }
        // audit.proto and auth.proto are compiled separately (client
        // stubs too — see build.rs) from the rest of this family, but
        // nest under the same `ration::<package>::v1` convention for
        // consistency.
        pub mod audit {
            pub mod v1 {
                tonic::include_proto!("ration.audit.v1");
            }
        }
        // See src/authclient.rs: ledger-core dials Auth directly to
        // verify every tenant-scoped caller's own credential
        // (docs/THREAT-MODEL.md's "Known gaps" item 1's "the biggest
        // one" sub-gap), the same way gateway's internal/tenantauth
        // already does on the Go side.
        pub mod auth {
            pub mod v1 {
                tonic::include_proto!("ration.auth.v1");
            }
        }
    }
}
