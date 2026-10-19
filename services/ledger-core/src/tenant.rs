//! Tenant domain entity + repository functions over the SQLite index.

use rusqlite::{params, Connection, OptionalExtension};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct TenantRecord {
    pub id: String,
    pub name: String,
    pub currency: String,
    pub reserve_floor_minor_units: i64,
    pub created_at_unix: i64,
}

pub fn init_schema(conn: &Connection) -> rusqlite::Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS tenants (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            currency TEXT NOT NULL,
            reserve_floor_minor_units INTEGER NOT NULL,
            created_at_unix INTEGER NOT NULL
        );",
    )
}

/// Inserts a tenant row. Used both on first creation and when replaying
/// a `TenantCreated` WAL event during recovery — in the replay case the
/// row may already exist (we crashed after the WAL write but before/at
/// the SQLite commit was durable... no, SQLite commits are durable too,
/// but replay is idempotent either way), so this uses `INSERT OR
/// IGNORE` keyed on the primary key id.
pub fn insert(conn: &Connection, t: &TenantRecord) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT OR IGNORE INTO tenants (id, name, currency, reserve_floor_minor_units, created_at_unix)
         VALUES (?1, ?2, ?3, ?4, ?5)",
        params![
            t.id,
            t.name,
            t.currency,
            t.reserve_floor_minor_units,
            t.created_at_unix
        ],
    )?;
    Ok(())
}

pub fn get(conn: &Connection, id: &str) -> rusqlite::Result<Option<TenantRecord>> {
    conn.query_row(
        "SELECT id, name, currency, reserve_floor_minor_units, created_at_unix
         FROM tenants WHERE id = ?1",
        params![id],
        row_to_tenant,
    )
    .optional()
}

pub fn list(conn: &Connection, limit: i64, offset: i64) -> rusqlite::Result<Vec<TenantRecord>> {
    let mut stmt = conn.prepare(
        "SELECT id, name, currency, reserve_floor_minor_units, created_at_unix
         FROM tenants ORDER BY created_at_unix ASC, id ASC LIMIT ?1 OFFSET ?2",
    )?;
    let rows = stmt.query_map(params![limit, offset], row_to_tenant)?;
    rows.collect()
}

fn row_to_tenant(row: &rusqlite::Row<'_>) -> rusqlite::Result<TenantRecord> {
    Ok(TenantRecord {
        id: row.get(0)?,
        name: row.get(1)?,
        currency: row.get(2)?,
        reserve_floor_minor_units: row.get(3)?,
        created_at_unix: row.get(4)?,
    })
}
