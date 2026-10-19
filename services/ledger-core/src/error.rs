//! Typed domain errors for ledger-core, with an explicit mapping onto
//! `tonic::Status` so gRPC handlers never need to hand-roll status codes.

use thiserror::Error;

use crate::money::MoneyError;
use crate::wal_ffi::WalError;

#[derive(Debug, Error)]
pub enum LedgerError {
    #[error("tenant not found: {0}")]
    TenantNotFound(String),

    #[error("deposit not found: {0}")]
    DepositNotFound(String),

    #[error("agent decision not found: {0}")]
    AgentDecisionNotFound(String),

    #[error("reserve floor violated: requested {requested} exceeds headroom {headroom}")]
    ReserveFloorViolated { requested: i64, headroom: i64 },

    #[error("insufficient held balance: tried to release {requested}, only {available} held")]
    InsufficientHeld { requested: i64, available: i64 },

    #[error(transparent)]
    Money(#[from] MoneyError),

    #[error("invalid argument: {0}")]
    InvalidArgument(String),

    #[error("storage error: {0}")]
    Storage(String),

    #[error("write-ahead log error: {0}")]
    Wal(String),

    #[error("idempotency_key is required")]
    IdempotencyKeyRequired,
}

impl From<rusqlite::Error> for LedgerError {
    fn from(e: rusqlite::Error) -> Self {
        LedgerError::Storage(e.to_string())
    }
}

impl From<WalError> for LedgerError {
    fn from(e: WalError) -> Self {
        LedgerError::Wal(e.to_string())
    }
}

impl From<LedgerError> for tonic::Status {
    fn from(e: LedgerError) -> Self {
        match e {
            LedgerError::TenantNotFound(_)
            | LedgerError::DepositNotFound(_)
            | LedgerError::AgentDecisionNotFound(_) => tonic::Status::not_found(e.to_string()),

            LedgerError::ReserveFloorViolated { .. } | LedgerError::InsufficientHeld { .. } => {
                tonic::Status::failed_precondition(e.to_string())
            }

            LedgerError::Money(_)
            | LedgerError::InvalidArgument(_)
            | LedgerError::IdempotencyKeyRequired => tonic::Status::invalid_argument(e.to_string()),

            LedgerError::Storage(_) | LedgerError::Wal(_) => tonic::Status::internal(e.to_string()),
        }
    }
}
