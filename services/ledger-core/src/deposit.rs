//! Deposit domain entity + repository functions over the SQLite index.

use rusqlite::{params, Connection, OptionalExtension};
use serde::{Deserialize, Serialize};

use crate::wal_crypto;

/// Mirrors `ration.common.v1.DepositStatus`. Kept as our own enum (not a
/// direct dependency on generated pb types) so the storage layer stays
/// decoupled from the wire format; `grpc.rs` converts between the two.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[repr(i32)]
pub enum DepositStatus {
    Unspecified = 0,
    Expected = 1,
    Received = 2,
    Late = 3,
}

impl DepositStatus {
    pub fn from_i32(v: i32) -> Self {
        match v {
            1 => DepositStatus::Expected,
            2 => DepositStatus::Received,
            3 => DepositStatus::Late,
            _ => DepositStatus::Unspecified,
        }
    }

    pub fn as_i32(self) -> i32 {
        self as i32
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct DepositRecord {
    pub id: String,
    pub tenant_id: String,
    pub expected_minor_units: i64,
    pub currency: String,
    pub expected_at_unix: i64,
    /// 0 means "not yet received".
    pub received_at_unix: i64,
    pub status: DepositStatus,
    pub external_ref: String,
}

pub fn init_schema(conn: &Connection) -> rusqlite::Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS deposits (
            id TEXT PRIMARY KEY,
            tenant_id TEXT NOT NULL REFERENCES tenants(id),
            expected_minor_units INTEGER NOT NULL,
            currency TEXT NOT NULL,
            expected_at_unix INTEGER NOT NULL,
            received_at_unix INTEGER NOT NULL DEFAULT 0,
            status INTEGER NOT NULL,
            external_ref TEXT NOT NULL DEFAULT ''
        );
        CREATE INDEX IF NOT EXISTS idx_deposits_tenant ON deposits(tenant_id);
        CREATE INDEX IF NOT EXISTS idx_deposits_late ON deposits(received_at_unix, expected_at_unix);",
    )
}

/// `crypto`: see `crate::wal_crypto`'s module doc comment, point 2 —
/// when `Some`, `external_ref` is encrypted before it ever reaches
/// SQLite; `None` preserves the original plaintext behavior exactly.
pub fn insert(
    conn: &Connection,
    d: &DepositRecord,
    crypto: Option<&wal_crypto::Key>,
) -> rusqlite::Result<()> {
    let external_ref = wal_crypto::encrypt_opt(crypto, &d.external_ref)?;
    conn.execute(
        "INSERT OR IGNORE INTO deposits
            (id, tenant_id, expected_minor_units, currency, expected_at_unix, received_at_unix, status, external_ref)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)",
        params![
            d.id,
            d.tenant_id,
            d.expected_minor_units,
            d.currency,
            d.expected_at_unix,
            d.received_at_unix,
            d.status.as_i32(),
            external_ref,
        ],
    )?;
    Ok(())
}

pub fn get(
    conn: &Connection,
    id: &str,
    crypto: Option<&wal_crypto::Key>,
) -> rusqlite::Result<Option<DepositRecord>> {
    let row = conn
        .query_row(
            "SELECT id, tenant_id, expected_minor_units, currency, expected_at_unix,
                received_at_unix, status, external_ref
         FROM deposits WHERE id = ?1",
            params![id],
            row_to_deposit,
        )
        .optional()?;
    row.map(|d| decrypt_deposit(d, crypto)).transpose()
}

/// Marks a deposit received. Idempotent: if the deposit is already
/// `Received`, this is a no-op update (same values written again) so
/// replaying the same WAL event twice during recovery is harmless.
pub fn mark_received(
    conn: &Connection,
    deposit_id: &str,
    received_at_unix: i64,
    external_ref: &str,
    crypto: Option<&wal_crypto::Key>,
) -> rusqlite::Result<()> {
    let encrypted_ref = wal_crypto::encrypt_opt(crypto, external_ref)?;
    conn.execute(
        "UPDATE deposits SET received_at_unix = ?2, status = ?3, external_ref = ?4
         WHERE id = ?1",
        params![
            deposit_id,
            received_at_unix,
            DepositStatus::Received.as_i32(),
            encrypted_ref
        ],
    )?;
    Ok(())
}

pub fn list_for_tenant(
    conn: &Connection,
    tenant_id: &str,
    crypto: Option<&wal_crypto::Key>,
) -> rusqlite::Result<Vec<DepositRecord>> {
    let mut stmt = conn.prepare(
        "SELECT id, tenant_id, expected_minor_units, currency, expected_at_unix,
                received_at_unix, status, external_ref
         FROM deposits WHERE tenant_id = ?1 ORDER BY expected_at_unix ASC",
    )?;
    let rows = stmt
        .query_map(params![tenant_id], row_to_deposit)?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    rows.into_iter()
        .map(|d| decrypt_deposit(d, crypto))
        .collect()
}

/// Deposits that are late as of `as_of_unix`: not yet received, and
/// their expected time has already passed. Computed from
/// `received_at_unix`/`expected_at_unix` directly rather than trusting
/// the stored `status` column, since a plain `ListLateDeposits` read
/// should never have to rely on some other call having already flipped
/// the status to `Late`.
pub fn list_late(
    conn: &Connection,
    as_of_unix: i64,
    crypto: Option<&wal_crypto::Key>,
) -> rusqlite::Result<Vec<DepositRecord>> {
    let mut stmt = conn.prepare(
        "SELECT id, tenant_id, expected_minor_units, currency, expected_at_unix,
                received_at_unix, status, external_ref
         FROM deposits
         WHERE received_at_unix = 0 AND expected_at_unix < ?1
         ORDER BY expected_at_unix ASC",
    )?;
    let rows = stmt
        .query_map(params![as_of_unix], row_to_deposit)?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    rows.into_iter()
        .map(|d| decrypt_deposit(d, crypto))
        .collect()
}

fn row_to_deposit(row: &rusqlite::Row<'_>) -> rusqlite::Result<DepositRecord> {
    Ok(DepositRecord {
        id: row.get(0)?,
        tenant_id: row.get(1)?,
        expected_minor_units: row.get(2)?,
        currency: row.get(3)?,
        expected_at_unix: row.get(4)?,
        received_at_unix: row.get(5)?,
        status: DepositStatus::from_i32(row.get(6)?),
        external_ref: row.get(7)?,
    })
}

/// Decrypts `d.external_ref` (a no-op when `crypto` is `None`) and
/// returns a new, otherwise-identical record — every read path
/// (`get`/`list_for_tenant`/`list_late`) goes through this rather than
/// mutating a row in place.
fn decrypt_deposit(
    d: DepositRecord,
    crypto: Option<&wal_crypto::Key>,
) -> rusqlite::Result<DepositRecord> {
    let external_ref = wal_crypto::decrypt_opt(crypto, &d.external_ref)?;
    Ok(DepositRecord { external_ref, ..d })
}
