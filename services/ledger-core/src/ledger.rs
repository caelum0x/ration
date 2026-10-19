//! Core domain: the ledger itself.
//!
//! `apply_entry` is the only way money moves. It is idempotent on
//! `idempotency_key` and enforces the tenant's reserve floor for DEBIT
//! and HOLD entries BEFORE writing anything to the write-ahead log —
//! the WAL is the durability source of truth, SQLite underneath is a
//! rebuildable, queryable index over it. Every state-changing operation
//! in this module follows the same two-phase pattern: (1) validate and
//! decide fully in memory, (2) append the decided event to the WAL and
//! fsync, (3) only then apply it to the SQLite index — so a crash
//! between steps 2 and 3 is recovered by replaying the WAL on boot
//! (`Ledger::open`), and a crash before step 2 simply never happened as
//! far as any observer is concerned.

use std::path::Path;
use std::sync::Mutex;
use std::time::{SystemTime, UNIX_EPOCH};

use rusqlite::Connection;
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::agent_decision::{AgentAction, AgentDecisionRecord};
use crate::deposit::{DepositRecord, DepositStatus};
use crate::error::LedgerError;
use crate::money::Money;
use crate::tenant::TenantRecord;
use crate::wal_ffi::{self, Wal};
use crate::{agent_decision, deposit, tenant};

/// Mirrors `ration.common.v1.LedgerEntryType`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[repr(i32)]
pub enum LedgerEntryType {
    Unspecified = 0,
    Credit = 1,
    Debit = 2,
    Hold = 3,
    Release = 4,
}

impl LedgerEntryType {
    pub fn from_i32(v: i32) -> Self {
        match v {
            1 => LedgerEntryType::Credit,
            2 => LedgerEntryType::Debit,
            3 => LedgerEntryType::Hold,
            4 => LedgerEntryType::Release,
            _ => LedgerEntryType::Unspecified,
        }
    }

    pub fn as_i32(self) -> i32 {
        self as i32
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct LedgerEntryRecord {
    pub id: String,
    pub tenant_id: String,
    pub entry_type: LedgerEntryType,
    pub amount_minor_units: i64,
    pub currency: String,
    /// Tenant's `available_minor_units` immediately after this entry.
    pub balance_after_minor_units: i64,
    pub reference: String,
    pub memo: String,
    pub idempotency_key: String,
    pub created_at_unix: i64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct BalanceRecord {
    pub available_minor_units: i64,
    pub held_minor_units: i64,
}

/// Every event that durably changes ledger-core's state, in the exact
/// post-decision form it should be replayed in. Replaying an event never
/// re-runs business logic (reserve-floor checks, validation) — the
/// decision was already made and durably logged the first time; replay
/// only has to make the SQLite index match what the WAL says happened.
#[derive(Debug, Clone, Serialize, Deserialize)]
enum WalEvent {
    TenantCreated(TenantRecord),
    DepositRecorded(DepositRecord),
    /// A deposit clearing AND the resulting CREDIT to the tenant's
    /// available balance, as a single atomic WAL frame. These must never
    /// be two separate frames: if the process crashed between them, a
    /// deposit could be marked received without ever crediting the
    /// ledger (or vice versa), silently breaking the headroom math every
    /// other decision in this file depends on.
    DepositReceivedWithCredit {
        deposit_id: String,
        received_at_unix: i64,
        external_ref: String,
        entry: LedgerEntryRecord,
        held_after_minor_units: i64,
    },
    EntryApplied {
        entry: LedgerEntryRecord,
        held_after_minor_units: i64,
    },
    AgentDecisionRecorded(AgentDecisionRecord),
}

pub fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("system clock before unix epoch")
        .as_secs() as i64
}

struct LedgerInner {
    wal: Wal,
    db: Connection,
}

/// The ledger: a WAL-backed, SQLite-indexed store of tenants, deposits,
/// balances, ledger entries and agent decisions.
///
/// All mutating operations lock `inner` for their full duration (decide,
/// WAL-append+fsync, SQLite-apply) so the WAL and the SQLite index can
/// never observe interleaved, half-applied state from two concurrent
/// calls.
pub struct Ledger {
    inner: Mutex<LedgerInner>,
}

impl Ledger {
    /// Opens the ledger at `wal_path`/`db_path`. Runs WAL recovery
    /// first: this both truncates any torn tail frame left by a crash
    /// AND replays every valid frame (in order) into the SQLite index
    /// via `wal_ffi::recover`'s callback, so the index is guaranteed
    /// consistent with the durable log before any RPC is accepted and
    /// before the WAL is even opened for further appends. Returns the
    /// number of frames replayed.
    pub fn open(wal_path: &Path, db_path: &Path) -> Result<(Self, u64), LedgerError> {
        if let Some(parent) = db_path.parent() {
            if !parent.as_os_str().is_empty() {
                std::fs::create_dir_all(parent)
                    .map_err(|e| LedgerError::Storage(format!("creating db dir: {e}")))?;
            }
        }
        let db = Connection::open(db_path)?;
        init_schema(&db)?;
        let replayed = replay_wal_into_index(wal_path, &db)?;

        let wal = Wal::open(wal_path)?;
        let ledger = Ledger {
            inner: Mutex::new(LedgerInner { wal, db }),
        };
        Ok((ledger, replayed))
    }

    /// In-memory variant used by tests that don't need a real SQLite
    /// file on disk.
    pub fn open_with_memory_db(wal_path: &Path) -> Result<(Self, u64), LedgerError> {
        let db = Connection::open_in_memory()?;
        init_schema(&db)?;
        let replayed = replay_wal_into_index(wal_path, &db)?;

        let wal = Wal::open(wal_path)?;
        let ledger = Ledger {
            inner: Mutex::new(LedgerInner { wal, db }),
        };
        Ok((ledger, replayed))
    }

    pub fn create_tenant(
        &self,
        name: String,
        currency: String,
        reserve_floor_minor_units: i64,
    ) -> Result<TenantRecord, LedgerError> {
        // Validates the currency code shape; the reserve floor amount
        // itself is allowed to be zero (a platform with no protected
        // capital) but not negative.
        let _ = Money::new(reserve_floor_minor_units, currency.clone())?;
        if reserve_floor_minor_units < 0 {
            return Err(LedgerError::InvalidArgument(
                "reserve_floor_minor_units must be non-negative".into(),
            ));
        }
        if name.trim().is_empty() {
            return Err(LedgerError::InvalidArgument("name must not be empty".into()));
        }

        let record = TenantRecord {
            id: Uuid::new_v4().to_string(),
            name,
            currency: currency.to_uppercase(),
            reserve_floor_minor_units,
            created_at_unix: now_unix(),
        };

        let mut inner = self.inner.lock().expect("ledger mutex poisoned");
        let event = WalEvent::TenantCreated(record.clone());
        append_and_commit(&mut inner, &event, |db| {
            tenant::insert(db, &record)?;
            init_balance_row(db, &record.id, &record.currency)?;
            Ok(())
        })?;
        Ok(record)
    }

    pub fn get_tenant(&self, id: &str) -> Result<TenantRecord, LedgerError> {
        let inner = self.inner.lock().expect("ledger mutex poisoned");
        tenant::get(&inner.db, id)?.ok_or_else(|| LedgerError::TenantNotFound(id.to_string()))
    }

    pub fn list_tenants(
        &self,
        page_size: i32,
        page_token: &str,
    ) -> Result<(Vec<TenantRecord>, String), LedgerError> {
        let limit = normalize_page_size(page_size);
        let offset = decode_page_token(page_token)?;
        let inner = self.inner.lock().expect("ledger mutex poisoned");
        let rows = tenant::list(&inner.db, limit + 1, offset)?;
        Ok(paginate(rows, limit, offset))
    }

    pub fn record_deposit(
        &self,
        tenant_id: String,
        amount: Money,
        expected_at_unix: i64,
        external_ref: String,
    ) -> Result<DepositRecord, LedgerError> {
        let mut inner = self.inner.lock().expect("ledger mutex poisoned");
        let t = tenant::get(&inner.db, &tenant_id)?
            .ok_or_else(|| LedgerError::TenantNotFound(tenant_id.clone()))?;
        if t.currency != amount.currency() {
            return Err(LedgerError::Money(crate::money::MoneyError::CurrencyMismatch {
                a: t.currency,
                b: amount.currency().to_string(),
            }));
        }

        let record = DepositRecord {
            id: Uuid::new_v4().to_string(),
            tenant_id,
            expected_minor_units: amount.minor_units(),
            currency: amount.currency().to_string(),
            expected_at_unix,
            received_at_unix: 0,
            status: DepositStatus::Expected,
            external_ref,
        };

        let event = WalEvent::DepositRecorded(record.clone());
        append_and_commit(&mut inner, &event, |db| deposit::insert(db, &record))?;
        Ok(record)
    }

    /// Marks a deposit received AND credits the tenant's available
    /// balance for its full expected amount, atomically (see
    /// `WalEvent::DepositReceivedWithCredit`). This is the only path by
    /// which an expected deposit actually becomes spendable headroom —
    /// without it, `RecordDeposit` alone never moves money, and every
    /// downstream reserve-floor/bridge-envelope computation would be
    /// working off a balance that can never rise above zero.
    ///
    /// Idempotent at the request level, not just WAL-replay level: if
    /// the deposit is already `Received` (e.g. a retried webhook
    /// delivery, or a duplicate call), this returns the existing record
    /// without appending a new event or crediting the ledger again.
    pub fn mark_deposit_received(
        &self,
        deposit_id: String,
        received_at_unix: i64,
        external_ref: String,
    ) -> Result<DepositRecord, LedgerError> {
        let mut inner = self.inner.lock().expect("ledger mutex poisoned");
        let existing = deposit::get(&inner.db, &deposit_id)?
            .ok_or_else(|| LedgerError::DepositNotFound(deposit_id.clone()))?;

        if existing.status == DepositStatus::Received {
            return Ok(existing);
        }

        let current_balance = read_balance(&inner.db, &existing.tenant_id)?;
        let new_available = current_balance
            .available_minor_units
            .checked_add(existing.expected_minor_units)
            .ok_or(crate::money::MoneyError::Overflow)?;

        let entry = LedgerEntryRecord {
            id: Uuid::new_v4().to_string(),
            tenant_id: existing.tenant_id.clone(),
            entry_type: LedgerEntryType::Credit,
            amount_minor_units: existing.expected_minor_units,
            currency: existing.currency.clone(),
            balance_after_minor_units: new_available,
            reference: format!("deposit:{deposit_id}"),
            memo: "deposit received".to_string(),
            idempotency_key: format!("deposit-received-credit:{deposit_id}"),
            created_at_unix: now_unix(),
        };

        let event = WalEvent::DepositReceivedWithCredit {
            deposit_id: deposit_id.clone(),
            received_at_unix,
            external_ref: external_ref.clone(),
            entry: entry.clone(),
            held_after_minor_units: current_balance.held_minor_units,
        };
        let tenant_id = existing.tenant_id.clone();
        let held_after = current_balance.held_minor_units;
        append_and_commit(&mut inner, &event, |db| {
            deposit::mark_received(db, &deposit_id, received_at_unix, &external_ref)?;
            let inserted = insert_ledger_entry_if_new(db, &entry)?;
            if inserted {
                write_balance(db, &tenant_id, entry.balance_after_minor_units, held_after)?;
            }
            Ok(())
        })?;

        Ok(DepositRecord {
            received_at_unix,
            status: DepositStatus::Received,
            external_ref,
            ..existing
        })
    }

    pub fn list_deposits(&self, tenant_id: &str) -> Result<Vec<DepositRecord>, LedgerError> {
        let inner = self.inner.lock().expect("ledger mutex poisoned");
        Ok(deposit::list_for_tenant(&inner.db, tenant_id)?)
    }

    pub fn list_late_deposits(&self, as_of_unix: i64) -> Result<Vec<DepositRecord>, LedgerError> {
        let inner = self.inner.lock().expect("ledger mutex poisoned");
        Ok(deposit::list_late(&inner.db, as_of_unix)?)
    }

    pub fn get_balance(&self, tenant_id: &str) -> Result<(BalanceRecord, String), LedgerError> {
        let inner = self.inner.lock().expect("ledger mutex poisoned");
        let t = tenant::get(&inner.db, tenant_id)?
            .ok_or_else(|| LedgerError::TenantNotFound(tenant_id.to_string()))?;
        let balance = read_balance(&inner.db, tenant_id)?;
        Ok((balance, t.currency))
    }

    pub fn check_reserve_floor(
        &self,
        tenant_id: &str,
        proposed_debit_minor_units: i64,
    ) -> Result<(bool, i64), LedgerError> {
        let inner = self.inner.lock().expect("ledger mutex poisoned");
        let t = tenant::get(&inner.db, tenant_id)?
            .ok_or_else(|| LedgerError::TenantNotFound(tenant_id.to_string()))?;
        let balance = read_balance(&inner.db, tenant_id)?;
        let headroom = balance.available_minor_units - t.reserve_floor_minor_units;
        let allowed = proposed_debit_minor_units >= 0 && proposed_debit_minor_units <= headroom;
        Ok((allowed, headroom))
    }

    /// The only way money moves. Idempotent on `idempotency_key`:
    /// replaying the exact same key returns the entry that already
    /// happened plus the current balance, without writing anything new.
    /// For DEBIT/HOLD, the reserve floor is checked BEFORE anything is
    /// written to the WAL; a violation returns `Ok` with
    /// `ApplyOutcome::ReserveFloorViolated` (not an error) carrying the
    /// unchanged current balance, mirroring
    /// `ApplyEntryResponse.reserve_floor_violated` in the proto.
    pub fn apply_entry(
        &self,
        tenant_id: String,
        entry_type: LedgerEntryType,
        amount: Money,
        reference: String,
        idempotency_key: String,
        memo: String,
    ) -> Result<ApplyOutcome, LedgerError> {
        if idempotency_key.trim().is_empty() {
            return Err(LedgerError::IdempotencyKeyRequired);
        }
        if amount.minor_units() < 0 {
            return Err(LedgerError::InvalidArgument(
                "amount.minor_units must be non-negative".into(),
            ));
        }

        let mut inner = self.inner.lock().expect("ledger mutex poisoned");

        if let Some(existing) = find_entry_by_idempotency_key(&inner.db, &idempotency_key)? {
            let balance = read_balance(&inner.db, &tenant_id)?;
            return Ok(ApplyOutcome::Applied {
                entry: existing,
                balance,
            });
        }

        let t = tenant::get(&inner.db, &tenant_id)?
            .ok_or_else(|| LedgerError::TenantNotFound(tenant_id.clone()))?;
        if t.currency != amount.currency() {
            return Err(LedgerError::Money(crate::money::MoneyError::CurrencyMismatch {
                a: t.currency,
                b: amount.currency().to_string(),
            }));
        }

        let current = read_balance(&inner.db, &tenant_id)?;
        let amt = amount.minor_units();

        let (new_available, new_held) = match entry_type {
            LedgerEntryType::Credit => {
                let available = current
                    .available_minor_units
                    .checked_add(amt)
                    .ok_or(crate::money::MoneyError::Overflow)?;
                (available, current.held_minor_units)
            }
            LedgerEntryType::Debit => {
                let headroom = current.available_minor_units - t.reserve_floor_minor_units;
                if amt > headroom {
                    return Ok(ApplyOutcome::ReserveFloorViolated {
                        balance: current,
                        currency: t.currency.clone(),
                    });
                }
                let available = current
                    .available_minor_units
                    .checked_sub(amt)
                    .ok_or(crate::money::MoneyError::Overflow)?;
                (available, current.held_minor_units)
            }
            LedgerEntryType::Hold => {
                let headroom = current.available_minor_units - t.reserve_floor_minor_units;
                if amt > headroom {
                    return Ok(ApplyOutcome::ReserveFloorViolated {
                        balance: current,
                        currency: t.currency.clone(),
                    });
                }
                let available = current
                    .available_minor_units
                    .checked_sub(amt)
                    .ok_or(crate::money::MoneyError::Overflow)?;
                let held = current
                    .held_minor_units
                    .checked_add(amt)
                    .ok_or(crate::money::MoneyError::Overflow)?;
                (available, held)
            }
            LedgerEntryType::Release => {
                if amt > current.held_minor_units {
                    return Err(LedgerError::InsufficientHeld {
                        requested: amt,
                        available: current.held_minor_units,
                    });
                }
                let available = current
                    .available_minor_units
                    .checked_add(amt)
                    .ok_or(crate::money::MoneyError::Overflow)?;
                let held = current
                    .held_minor_units
                    .checked_sub(amt)
                    .ok_or(crate::money::MoneyError::Overflow)?;
                (available, held)
            }
            LedgerEntryType::Unspecified => {
                return Err(LedgerError::InvalidArgument(
                    "entry_type must be specified".into(),
                ))
            }
        };

        let entry = LedgerEntryRecord {
            id: Uuid::new_v4().to_string(),
            tenant_id: tenant_id.clone(),
            entry_type,
            amount_minor_units: amt,
            currency: amount.currency().to_string(),
            balance_after_minor_units: new_available,
            reference,
            memo,
            idempotency_key,
            created_at_unix: now_unix(),
        };

        let event = WalEvent::EntryApplied {
            entry: entry.clone(),
            held_after_minor_units: new_held,
        };
        append_and_commit(&mut inner, &event, |db| {
            insert_ledger_entry(db, &entry)?;
            write_balance(db, &tenant_id, new_available, new_held)
        })?;

        Ok(ApplyOutcome::Applied {
            entry,
            balance: BalanceRecord {
                available_minor_units: new_available,
                held_minor_units: new_held,
            },
        })
    }

    pub fn list_entries(
        &self,
        tenant_id: &str,
        page_size: i32,
        page_token: &str,
    ) -> Result<(Vec<LedgerEntryRecord>, String), LedgerError> {
        let limit = normalize_page_size(page_size);
        let offset = decode_page_token(page_token)?;
        let inner = self.inner.lock().expect("ledger mutex poisoned");
        let rows = list_ledger_entries(&inner.db, tenant_id, limit + 1, offset)?;
        Ok(paginate(rows, limit, offset))
    }

    pub fn record_agent_decision(
        &self,
        tenant_id: String,
        trigger: String,
        action: AgentAction,
        rationale: String,
        policy_snapshot_json: String,
    ) -> Result<AgentDecisionRecord, LedgerError> {
        let mut inner = self.inner.lock().expect("ledger mutex poisoned");
        tenant::get(&inner.db, &tenant_id)?
            .ok_or_else(|| LedgerError::TenantNotFound(tenant_id.clone()))?;

        let record = AgentDecisionRecord {
            id: Uuid::new_v4().to_string(),
            tenant_id,
            trigger,
            action,
            rationale,
            policy_snapshot_json,
            created_at_unix: now_unix(),
        };

        let event = WalEvent::AgentDecisionRecorded(record.clone());
        append_and_commit(&mut inner, &event, |db| agent_decision::insert(db, &record))?;
        Ok(record)
    }

    pub fn list_agent_decisions(
        &self,
        tenant_id: &str,
        limit: i32,
    ) -> Result<Vec<AgentDecisionRecord>, LedgerError> {
        let limit = if limit <= 0 { 50 } else { limit as i64 };
        let inner = self.inner.lock().expect("ledger mutex poisoned");
        Ok(agent_decision::list_for_tenant(&inner.db, tenant_id, limit)?)
    }
}

#[derive(Debug, Clone, PartialEq)]
pub enum ApplyOutcome {
    Applied {
        entry: LedgerEntryRecord,
        balance: BalanceRecord,
    },
    ReserveFloorViolated {
        balance: BalanceRecord,
        currency: String,
    },
}

/// Appends `event` to the WAL (fsyncing before returning), then runs
/// `apply` against the SQLite index inside its own transaction. If the
/// process crashes after the WAL append but before (or during) the
/// SQLite commit, the next `Ledger::open` replays the event from the
/// WAL and re-applies it via [`apply_replayed_event`] — so this ordering
/// is exactly what makes recovery lossless and crash-safe.
fn append_and_commit(
    inner: &mut LedgerInner,
    event: &WalEvent,
    apply: impl FnOnce(&Connection) -> rusqlite::Result<()>,
) -> Result<(), LedgerError> {
    let bytes = serde_json::to_vec(event)
        .map_err(|e| LedgerError::Storage(format!("encoding WAL event: {e}")))?;
    inner.wal.append(&bytes)?;

    let tx = inner.db.unchecked_transaction()?;
    apply(&tx)?;
    tx.commit()?;
    Ok(())
}

fn apply_replayed_event(db: &Connection, event: WalEvent) -> Result<(), LedgerError> {
    let tx = db.unchecked_transaction()?;
    match event {
        WalEvent::TenantCreated(t) => {
            tenant::insert(&tx, &t)?;
            init_balance_row(&tx, &t.id, &t.currency)?;
        }
        WalEvent::DepositRecorded(d) => {
            deposit::insert(&tx, &d)?;
        }
        WalEvent::DepositReceivedWithCredit {
            deposit_id,
            received_at_unix,
            external_ref,
            entry,
            held_after_minor_units,
        } => {
            deposit::mark_received(&tx, &deposit_id, received_at_unix, &external_ref)?;
            // Same idempotent-on-replay pattern as EntryApplied: only
            // write the balance if this entry row wasn't already
            // indexed.
            let inserted = insert_ledger_entry_if_new(&tx, &entry)?;
            if inserted {
                write_balance(&tx, &entry.tenant_id, entry.balance_after_minor_units, held_after_minor_units)?;
            }
        }
        WalEvent::EntryApplied {
            entry,
            held_after_minor_units,
        } => {
            // Idempotent: INSERT OR IGNORE on idempotency_key means a
            // frame we've already indexed (e.g. replaying from the
            // start on every boot) does not get double-applied to the
            // balance either — we only update the balance if this entry
            // row did not already exist.
            let inserted = insert_ledger_entry_if_new(&tx, &entry)?;
            if inserted {
                write_balance(&tx, &entry.tenant_id, entry.balance_after_minor_units, held_after_minor_units)?;
            }
        }
        WalEvent::AgentDecisionRecorded(d) => {
            agent_decision::insert(&tx, &d)?;
        }
    }
    tx.commit()?;
    Ok(())
}

/// Runs WAL recovery against `wal_path`: truncates any torn tail frame
/// left by a crash and replays every valid frame, in order, into `db`.
/// Returns the number of frames replayed.
///
/// Any error surfaced by the replay itself (a corrupt WAL event
/// payload, or a SQLite error while applying it) is captured and
/// returned directly, taking priority over the generic
/// `WalError::Invalid` ("callback aborted") that `wal_ffi::recover`
/// would otherwise report for the same abort.
fn replay_wal_into_index(wal_path: &Path, db: &Connection) -> Result<u64, LedgerError> {
    let mut first_error: Option<LedgerError> = None;

    let result = wal_ffi::recover(wal_path, |bytes| {
        if first_error.is_some() {
            return false;
        }
        match serde_json::from_slice::<WalEvent>(bytes) {
            Ok(event) => match apply_replayed_event(db, event) {
                Ok(()) => true,
                Err(e) => {
                    first_error = Some(e);
                    false
                }
            },
            Err(e) => {
                first_error = Some(LedgerError::Storage(format!(
                    "corrupt WAL event payload: {e}"
                )));
                false
            }
        }
    });

    if let Some(err) = first_error {
        return Err(err);
    }
    Ok(result?.frames_recovered)
}

fn init_schema(conn: &Connection) -> rusqlite::Result<()> {
    conn.execute_batch("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;")?;
    tenant::init_schema(conn)?;
    deposit::init_schema(conn)?;
    agent_decision::init_schema(conn)?;
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS balances (
            tenant_id TEXT PRIMARY KEY REFERENCES tenants(id),
            available_minor_units INTEGER NOT NULL,
            held_minor_units INTEGER NOT NULL,
            currency TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS ledger_entries (
            id TEXT PRIMARY KEY,
            tenant_id TEXT NOT NULL REFERENCES tenants(id),
            entry_type INTEGER NOT NULL,
            amount_minor_units INTEGER NOT NULL,
            currency TEXT NOT NULL,
            balance_after_minor_units INTEGER NOT NULL,
            reference TEXT NOT NULL DEFAULT '',
            memo TEXT NOT NULL DEFAULT '',
            idempotency_key TEXT NOT NULL UNIQUE,
            created_at_unix INTEGER NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_ledger_entries_tenant
            ON ledger_entries(tenant_id, created_at_unix ASC);",
    )
}

fn init_balance_row(conn: &Connection, tenant_id: &str, currency: &str) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT OR IGNORE INTO balances (tenant_id, available_minor_units, held_minor_units, currency)
         VALUES (?1, 0, 0, ?2)",
        rusqlite::params![tenant_id, currency],
    )?;
    Ok(())
}

fn read_balance(conn: &Connection, tenant_id: &str) -> Result<BalanceRecord, LedgerError> {
    conn.query_row(
        "SELECT available_minor_units, held_minor_units FROM balances WHERE tenant_id = ?1",
        rusqlite::params![tenant_id],
        |row| {
            Ok(BalanceRecord {
                available_minor_units: row.get(0)?,
                held_minor_units: row.get(1)?,
            })
        },
    )
    .map_err(|e| match e {
        rusqlite::Error::QueryReturnedNoRows => LedgerError::TenantNotFound(tenant_id.to_string()),
        other => LedgerError::from(other),
    })
}

fn write_balance(
    conn: &Connection,
    tenant_id: &str,
    available_minor_units: i64,
    held_minor_units: i64,
) -> rusqlite::Result<()> {
    conn.execute(
        "UPDATE balances SET available_minor_units = ?2, held_minor_units = ?3 WHERE tenant_id = ?1",
        rusqlite::params![tenant_id, available_minor_units, held_minor_units],
    )?;
    Ok(())
}

fn insert_ledger_entry(conn: &Connection, e: &LedgerEntryRecord) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO ledger_entries
            (id, tenant_id, entry_type, amount_minor_units, currency, balance_after_minor_units,
             reference, memo, idempotency_key, created_at_unix)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10)",
        rusqlite::params![
            e.id,
            e.tenant_id,
            e.entry_type.as_i32(),
            e.amount_minor_units,
            e.currency,
            e.balance_after_minor_units,
            e.reference,
            e.memo,
            e.idempotency_key,
            e.created_at_unix,
        ],
    )?;
    Ok(())
}

/// Like [`insert_ledger_entry`] but tolerant of the row already existing
/// (keyed on the unique `idempotency_key`) — used during WAL replay,
/// where the same frame may be replayed more than once across restarts.
/// Returns whether a new row was actually inserted.
fn insert_ledger_entry_if_new(conn: &Connection, e: &LedgerEntryRecord) -> rusqlite::Result<bool> {
    let changed = conn.execute(
        "INSERT OR IGNORE INTO ledger_entries
            (id, tenant_id, entry_type, amount_minor_units, currency, balance_after_minor_units,
             reference, memo, idempotency_key, created_at_unix)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10)",
        rusqlite::params![
            e.id,
            e.tenant_id,
            e.entry_type.as_i32(),
            e.amount_minor_units,
            e.currency,
            e.balance_after_minor_units,
            e.reference,
            e.memo,
            e.idempotency_key,
            e.created_at_unix,
        ],
    )?;
    Ok(changed > 0)
}

fn find_entry_by_idempotency_key(
    conn: &Connection,
    idempotency_key: &str,
) -> rusqlite::Result<Option<LedgerEntryRecord>> {
    conn.query_row(
        "SELECT id, tenant_id, entry_type, amount_minor_units, currency, balance_after_minor_units,
                reference, memo, idempotency_key, created_at_unix
         FROM ledger_entries WHERE idempotency_key = ?1",
        rusqlite::params![idempotency_key],
        row_to_entry,
    )
    .map(Some)
    .or_else(|e| match e {
        rusqlite::Error::QueryReturnedNoRows => Ok(None),
        other => Err(other),
    })
}

fn list_ledger_entries(
    conn: &Connection,
    tenant_id: &str,
    limit: i64,
    offset: i64,
) -> rusqlite::Result<Vec<LedgerEntryRecord>> {
    let mut stmt = conn.prepare(
        "SELECT id, tenant_id, entry_type, amount_minor_units, currency, balance_after_minor_units,
                reference, memo, idempotency_key, created_at_unix
         FROM ledger_entries WHERE tenant_id = ?1
         ORDER BY created_at_unix ASC, id ASC LIMIT ?2 OFFSET ?3",
    )?;
    let rows = stmt.query_map(rusqlite::params![tenant_id, limit, offset], row_to_entry)?;
    rows.collect()
}

fn row_to_entry(row: &rusqlite::Row<'_>) -> rusqlite::Result<LedgerEntryRecord> {
    Ok(LedgerEntryRecord {
        id: row.get(0)?,
        tenant_id: row.get(1)?,
        entry_type: LedgerEntryType::from_i32(row.get(2)?),
        amount_minor_units: row.get(3)?,
        currency: row.get(4)?,
        balance_after_minor_units: row.get(5)?,
        reference: row.get(6)?,
        memo: row.get(7)?,
        idempotency_key: row.get(8)?,
        created_at_unix: row.get(9)?,
    })
}

fn normalize_page_size(page_size: i32) -> i64 {
    const DEFAULT: i64 = 50;
    const MAX: i64 = 500;
    if page_size <= 0 {
        DEFAULT
    } else {
        (page_size as i64).min(MAX)
    }
}

fn decode_page_token(token: &str) -> Result<i64, LedgerError> {
    if token.is_empty() {
        return Ok(0);
    }
    token
        .parse::<i64>()
        .map_err(|_| LedgerError::InvalidArgument(format!("invalid page_token {token:?}")))
}

/// Took `limit + 1` rows from the DB; if we got more than `limit`, there
/// is a next page, so we drop the lookahead row and return the next
/// offset as the token.
fn paginate<T>(mut rows: Vec<T>, limit: i64, offset: i64) -> (Vec<T>, String) {
    if rows.len() as i64 > limit {
        rows.truncate(limit as usize);
        (rows, (offset + limit).to_string())
    } else {
        (rows, String::new())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    fn temp_wal_path(name: &str) -> PathBuf {
        let dir = std::env::temp_dir();
        dir.join(format!(
            "ration-ledger-test-{name}-{}-{}.wal",
            std::process::id(),
            Uuid::new_v4()
        ))
    }

    fn new_ledger() -> Ledger {
        let path = temp_wal_path("unit");
        let (ledger, _) = Ledger::open_with_memory_db(&path).unwrap();
        ledger
    }

    #[test]
    fn create_tenant_and_get_balance_starts_at_zero() {
        let ledger = new_ledger();
        let t = ledger
            .create_tenant("Acme".into(), "USD".into(), 1000)
            .unwrap();
        let (balance, currency) = ledger.get_balance(&t.id).unwrap();
        assert_eq!(balance.available_minor_units, 0);
        assert_eq!(balance.held_minor_units, 0);
        assert_eq!(currency, "USD");
    }

    #[test]
    fn credit_then_debit_within_floor_succeeds() {
        let ledger = new_ledger();
        let t = ledger.create_tenant("Acme".into(), "USD".into(), 500).unwrap();

        let credit = ledger
            .apply_entry(
                t.id.clone(),
                LedgerEntryType::Credit,
                Money::new(1000, "USD").unwrap(),
                "deposit".into(),
                "key-1".into(),
                "".into(),
            )
            .unwrap();
        assert!(matches!(credit, ApplyOutcome::Applied { .. }));

        let debit = ledger
            .apply_entry(
                t.id.clone(),
                LedgerEntryType::Debit,
                Money::new(400, "USD").unwrap(),
                "spend".into(),
                "key-2".into(),
                "".into(),
            )
            .unwrap();
        match debit {
            ApplyOutcome::Applied { balance, .. } => {
                assert_eq!(balance.available_minor_units, 600);
            }
            other => panic!("expected Applied, got {other:?}"),
        }
    }

    #[test]
    fn debit_breaching_reserve_floor_is_rejected_without_mutating_balance() {
        let ledger = new_ledger();
        let t = ledger.create_tenant("Acme".into(), "USD".into(), 500).unwrap();
        ledger
            .apply_entry(
                t.id.clone(),
                LedgerEntryType::Credit,
                Money::new(1000, "USD").unwrap(),
                "deposit".into(),
                "key-1".into(),
                "".into(),
            )
            .unwrap();

        // headroom = 1000 - 500 = 500; requesting 600 must be rejected.
        let outcome = ledger
            .apply_entry(
                t.id.clone(),
                LedgerEntryType::Debit,
                Money::new(600, "USD").unwrap(),
                "spend".into(),
                "key-2".into(),
                "".into(),
            )
            .unwrap();
        match outcome {
            ApplyOutcome::ReserveFloorViolated { balance, currency } => {
                assert_eq!(balance.available_minor_units, 1000);
                assert_eq!(currency, "USD");
            }
            other => panic!("expected ReserveFloorViolated, got {other:?}"),
        }

        let (balance, _) = ledger.get_balance(&t.id).unwrap();
        assert_eq!(balance.available_minor_units, 1000, "balance must be unchanged");
    }
}
