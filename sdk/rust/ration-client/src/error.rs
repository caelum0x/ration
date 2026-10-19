//! Typed errors for `ration-client`, mirroring
//! `services/ledger-core/src/error.rs`'s shape: an explicit enum over the
//! two things that can actually go wrong — a bad endpoint/transport, or a
//! failed RPC — instead of a boxed `dyn Error`.

use thiserror::Error;

#[derive(Debug, Error)]
pub enum ClientError {
    #[error("invalid gateway endpoint or transport failure: {0}")]
    Transport(#[from] tonic::transport::Error),

    #[error("gateway RPC failed: {0}")]
    Rpc(#[from] tonic::Status),
}
