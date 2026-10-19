//! ledger-core: the authoritative Rust ledger for Ration.
//!
//! Owns all tenant/deposit/balance/ledger-entry state. Every money-moving
//! operation is written to the crash-safe C write-ahead log
//! (`services/wal`, wrapped safely in [`wal_ffi`]) before the in-process
//! SQLite index is updated. The WAL is the durability source of truth;
//! SQLite is a rebuildable, queryable index over it.

pub mod agent_decision;
pub mod deposit;
pub mod error;
pub mod ledger;
pub mod money;
pub mod tenant;
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
    }
}
