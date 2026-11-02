//! `ApplyEntry` must be idempotent on `idempotency_key`: calling it twice
//! with the same key must apply the money movement exactly once, and
//! the second call must hand back the entry that already happened
//! rather than creating a new one or re-mutating the balance.

use std::path::PathBuf;

use ledger_core::ledger::{ApplyOutcome, Ledger, LedgerEntryType};
use ledger_core::money::Money;

fn temp_wal_path(tag: &str) -> PathBuf {
    let dir = std::env::temp_dir();
    dir.join(format!(
        "ration-idempotency-{tag}-{}-{}.wal",
        std::process::id(),
        uuid::Uuid::new_v4()
    ))
}

fn new_ledger(tag: &str) -> Ledger {
    let (ledger, _) = Ledger::open_with_memory_db(&temp_wal_path(tag)).unwrap();
    ledger
}

#[test]
fn duplicate_apply_entry_call_is_a_no_op_the_second_time() {
    let ledger = new_ledger("dup-call");
    let tenant = ledger
        .create_tenant("Acme".into(), "USD".into(), 0)
        .unwrap();

    let first = ledger
        .apply_entry(
            tenant.id.clone(),
            LedgerEntryType::Credit,
            Money::new(5_000, "USD").unwrap(),
            "payout".into(),
            "idem-key-1".into(),
            "".into(),
        )
        .unwrap();

    let first_entry = match first {
        ApplyOutcome::Applied { entry, balance } => {
            assert_eq!(balance.available_minor_units, 5_000);
            entry
        }
        other => panic!("expected Applied, got {other:?}"),
    };

    // Replay the exact same call (same idempotency_key). Balance must
    // not move a second time, and the returned entry must be the one
    // that already happened (same id).
    let second = ledger
        .apply_entry(
            tenant.id.clone(),
            LedgerEntryType::Credit,
            Money::new(5_000, "USD").unwrap(),
            "payout".into(),
            "idem-key-1".into(),
            "".into(),
        )
        .unwrap();

    match second {
        ApplyOutcome::Applied { entry, balance } => {
            assert_eq!(
                entry.id, first_entry.id,
                "replay must return the original entry"
            );
            assert_eq!(
                balance.available_minor_units, 5_000,
                "balance must not double-apply"
            );
        }
        other => panic!("expected Applied, got {other:?}"),
    }

    let (balance, _) = ledger.get_balance(&tenant.id).unwrap();
    assert_eq!(balance.available_minor_units, 5_000);
}

#[test]
fn replay_ignores_different_parameters_and_returns_the_original_decision() {
    // Even if a caller (bug, retry-with-mutated-payload) sends different
    // amount/type on the replay, the idempotency_key dedup must win: the
    // original decision is what happened, full stop.
    let ledger = new_ledger("dup-call-different-params");
    let tenant = ledger
        .create_tenant("Acme".into(), "USD".into(), 0)
        .unwrap();

    ledger
        .apply_entry(
            tenant.id.clone(),
            LedgerEntryType::Credit,
            Money::new(1_000, "USD").unwrap(),
            "payout".into(),
            "idem-key-2".into(),
            "".into(),
        )
        .unwrap();

    let replay = ledger
        .apply_entry(
            tenant.id.clone(),
            LedgerEntryType::Debit, // different type than the original
            Money::new(9_999, "USD").unwrap(), // different amount
            "unrelated".into(),
            "idem-key-2".into(), // same key
            "".into(),
        )
        .unwrap();

    match replay {
        ApplyOutcome::Applied { entry, balance } => {
            assert_eq!(
                entry.amount_minor_units, 1_000,
                "must be the ORIGINAL amount, not 9999"
            );
            assert_eq!(balance.available_minor_units, 1_000);
        }
        other => panic!("expected Applied (idempotent replay), got {other:?}"),
    }
}

#[test]
fn missing_idempotency_key_is_rejected() {
    let ledger = new_ledger("missing-key");
    let tenant = ledger
        .create_tenant("Acme".into(), "USD".into(), 0)
        .unwrap();

    let err = ledger
        .apply_entry(
            tenant.id,
            LedgerEntryType::Credit,
            Money::new(100, "USD").unwrap(),
            "payout".into(),
            "".into(),
            "".into(),
        )
        .unwrap_err();

    assert!(matches!(
        err,
        ledger_core::error::LedgerError::IdempotencyKeyRequired
    ));
}

#[test]
fn replaying_another_tenants_idempotency_key_is_rejected_not_silently_applied() {
    // idempotency_key is UNIQUE across the whole ledger_entries table
    // (every tenant shares one key space, by schema), not scoped to
    // (tenant_id, idempotency_key). Before this was guarded, supplying a
    // key some OTHER tenant already used returned `Applied` with the
    // OTHER tenant's entry (leaking its amount/reference/memo to this
    // caller) and THIS tenant's own unchanged balance — reporting
    // success for a write that never actually happened for this tenant,
    // with no error anywhere. Round 36.
    let ledger = new_ledger("cross-tenant-key-reuse");
    let tenant_a = ledger
        .create_tenant("Acme".into(), "USD".into(), 0)
        .unwrap();
    let tenant_b = ledger
        .create_tenant("Widgets Inc".into(), "USD".into(), 0)
        .unwrap();

    ledger
        .apply_entry(
            tenant_a.id.clone(),
            LedgerEntryType::Credit,
            Money::new(5_000, "USD").unwrap(),
            "tenant-a-payout".into(),
            "shared-key".into(),
            "".into(),
        )
        .unwrap();

    let err = ledger
        .apply_entry(
            tenant_b.id.clone(),
            LedgerEntryType::Credit,
            Money::new(7_500, "USD").unwrap(),
            "tenant-b-payout".into(),
            "shared-key".into(), // same key, different tenant
            "".into(),
        )
        .unwrap_err();

    assert!(
        matches!(
            err,
            ledger_core::error::LedgerError::IdempotencyKeyTenantMismatch { .. }
        ),
        "expected IdempotencyKeyTenantMismatch, got {err:?}"
    );

    // Tenant B's balance must be untouched — the rejected call must not
    // have silently no-op'd as a reported success.
    let (balance_b, _) = ledger.get_balance(&tenant_b.id).unwrap();
    assert_eq!(balance_b.available_minor_units, 0);

    // Tenant A's own balance/entry are unaffected by tenant B's attempt.
    let (balance_a, _) = ledger.get_balance(&tenant_a.id).unwrap();
    assert_eq!(balance_a.available_minor_units, 5_000);
}

#[test]
fn distinct_idempotency_keys_both_apply() {
    let ledger = new_ledger("distinct-keys");
    let tenant = ledger
        .create_tenant("Acme".into(), "USD".into(), 0)
        .unwrap();

    ledger
        .apply_entry(
            tenant.id.clone(),
            LedgerEntryType::Credit,
            Money::new(1_000, "USD").unwrap(),
            "payout-1".into(),
            "idem-a".into(),
            "".into(),
        )
        .unwrap();
    ledger
        .apply_entry(
            tenant.id.clone(),
            LedgerEntryType::Credit,
            Money::new(1_000, "USD").unwrap(),
            "payout-2".into(),
            "idem-b".into(),
            "".into(),
        )
        .unwrap();

    let (balance, _) = ledger.get_balance(&tenant.id).unwrap();
    assert_eq!(
        balance.available_minor_units, 2_000,
        "two distinct keys must both apply"
    );
}
