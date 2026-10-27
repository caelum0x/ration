//! Agent decision domain entity + repository functions over the SQLite
//! index. Every agent decision — hard-rule computation or LLM tool call
//! — is recorded here (via the WAL, see `crate::ledger`) before any
//! money moves, so it forms a durable audit trail.

use rusqlite::{params, Connection};
use serde::{Deserialize, Serialize};

use crate::wal_crypto;

/// Mirrors `ration.common.v1.AgentAction`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[repr(i32)]
pub enum AgentAction {
    Unspecified = 0,
    IssueCard = 1,
    AdjustLimit = 2,
    RationBridge = 3,
    ReleaseHold = 4,
    Decline = 5,
    Notify = 6,
}

impl AgentAction {
    pub fn from_i32(v: i32) -> Self {
        match v {
            1 => AgentAction::IssueCard,
            2 => AgentAction::AdjustLimit,
            3 => AgentAction::RationBridge,
            4 => AgentAction::ReleaseHold,
            5 => AgentAction::Decline,
            6 => AgentAction::Notify,
            _ => AgentAction::Unspecified,
        }
    }

    pub fn as_i32(self) -> i32 {
        self as i32
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct AgentDecisionRecord {
    pub id: String,
    pub tenant_id: String,
    pub trigger: String,
    pub action: AgentAction,
    pub rationale: String,
    pub policy_snapshot_json: String,
    pub created_at_unix: i64,
}

pub fn init_schema(conn: &Connection) -> rusqlite::Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS agent_decisions (
            id TEXT PRIMARY KEY,
            tenant_id TEXT NOT NULL REFERENCES tenants(id),
            trigger TEXT NOT NULL,
            action INTEGER NOT NULL,
            rationale TEXT NOT NULL,
            policy_snapshot_json TEXT NOT NULL,
            created_at_unix INTEGER NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_agent_decisions_tenant
            ON agent_decisions(tenant_id, created_at_unix DESC);",
    )
}

/// `crypto`: see `crate::wal_crypto`'s module doc comment, point 2 —
/// when `Some`, `trigger`/`rationale`/`policy_snapshot_json` (every
/// LLM- or operator-authored free-text field on this record) are each
/// encrypted independently before reaching SQLite; `None` preserves the
/// original plaintext behavior exactly.
pub fn insert(
    conn: &Connection,
    d: &AgentDecisionRecord,
    crypto: Option<&wal_crypto::Key>,
) -> rusqlite::Result<()> {
    let trigger = wal_crypto::encrypt_opt(crypto, &d.trigger)?;
    let rationale = wal_crypto::encrypt_opt(crypto, &d.rationale)?;
    let policy_snapshot_json = wal_crypto::encrypt_opt(crypto, &d.policy_snapshot_json)?;
    conn.execute(
        "INSERT OR IGNORE INTO agent_decisions
            (id, tenant_id, trigger, action, rationale, policy_snapshot_json, created_at_unix)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
        params![
            d.id,
            d.tenant_id,
            trigger,
            d.action.as_i32(),
            rationale,
            policy_snapshot_json,
            d.created_at_unix,
        ],
    )?;
    Ok(())
}

pub fn list_for_tenant(
    conn: &Connection,
    tenant_id: &str,
    limit: i64,
    crypto: Option<&wal_crypto::Key>,
) -> rusqlite::Result<Vec<AgentDecisionRecord>> {
    let mut stmt = conn.prepare(
        "SELECT id, tenant_id, trigger, action, rationale, policy_snapshot_json, created_at_unix
         FROM agent_decisions WHERE tenant_id = ?1
         ORDER BY created_at_unix DESC LIMIT ?2",
    )?;
    let rows = stmt
        .query_map(params![tenant_id, limit], row_to_decision)?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    rows.into_iter()
        .map(|d| decrypt_decision(d, crypto))
        .collect()
}

fn row_to_decision(row: &rusqlite::Row<'_>) -> rusqlite::Result<AgentDecisionRecord> {
    Ok(AgentDecisionRecord {
        id: row.get(0)?,
        tenant_id: row.get(1)?,
        trigger: row.get(2)?,
        action: AgentAction::from_i32(row.get(3)?),
        rationale: row.get(4)?,
        policy_snapshot_json: row.get(5)?,
        created_at_unix: row.get(6)?,
    })
}

/// Decrypts `trigger`/`rationale`/`policy_snapshot_json` (a no-op per
/// field when `crypto` is `None`) and returns a new, otherwise-identical
/// record — every read path goes through this rather than mutating a
/// row in place.
fn decrypt_decision(
    d: AgentDecisionRecord,
    crypto: Option<&wal_crypto::Key>,
) -> rusqlite::Result<AgentDecisionRecord> {
    let trigger = wal_crypto::decrypt_opt(crypto, &d.trigger)?;
    let rationale = wal_crypto::decrypt_opt(crypto, &d.rationale)?;
    let policy_snapshot_json = wal_crypto::decrypt_opt(crypto, &d.policy_snapshot_json)?;
    Ok(AgentDecisionRecord {
        trigger,
        rationale,
        policy_snapshot_json,
        ..d
    })
}
