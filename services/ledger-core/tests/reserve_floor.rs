//! The reserve floor is the platform's own capital and must never be
//! breached by a DEBIT or HOLD — headroom = available - reserve_floor,
//! and no DEBIT/HOLD may exceed it. CREDIT and RELEASE are never
//! floor-checked (they only ever move money toward safety).

use std::path::PathBuf;

use ledger_core::ledger::{ApplyOutcome, Ledger, LedgerEntryType};
use ledger_core::money::Money;

fn temp_wal_path(tag: &str) -> PathBuf {
    let dir = std::env::temp_dir();
    dir.join(format!(
        "ration-reserve-floor-{tag}-{}-{}.wal",
        std::process::id(),
        uuid::Uuid::new_v4()
    ))
}

fn new_ledger(tag: &str) -> Ledger {
    let (ledger, _) = Ledger::open_with_memory_db(&temp_wal_path(tag)).unwrap();
    ledger
}

#[test]
fn debit_within_headroom_succeeds() {
    let ledger = new_ledger("debit-ok");
    let tenant = ledger.create_tenant("Acme".into(), "USD".into(), 200).unwrap();
    ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Credit, Money::new(1_000, "USD").unwrap(), "seed".into(), "k1".into(), "".into())
        .unwrap();

    // headroom = 1000 - 200 = 800; debiting exactly 800 must be allowed.
    let outcome = ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Debit, Money::new(800, "USD").unwrap(), "spend".into(), "k2".into(), "".into())
        .unwrap();

    match outcome {
        ApplyOutcome::Applied { balance, .. } => assert_eq!(balance.available_minor_units, 200),
        other => panic!("expected Applied, got {other:?}"),
    }
}

#[test]
fn debit_exceeding_headroom_by_one_minor_unit_is_rejected() {
    let ledger = new_ledger("debit-reject-off-by-one");
    let tenant = ledger.create_tenant("Acme".into(), "USD".into(), 200).unwrap();
    ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Credit, Money::new(1_000, "USD").unwrap(), "seed".into(), "k1".into(), "".into())
        .unwrap();

    // headroom = 800; requesting 801 must be rejected, balance untouched.
    let outcome = ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Debit, Money::new(801, "USD").unwrap(), "spend".into(), "k2".into(), "".into())
        .unwrap();

    match outcome {
        ApplyOutcome::ReserveFloorViolated { balance, currency } => {
            assert_eq!(balance.available_minor_units, 1_000, "balance must be unchanged on rejection");
            assert_eq!(currency, "USD");
        }
        other => panic!("expected ReserveFloorViolated, got {other:?}"),
    }

    let (balance, _) = ledger.get_balance(&tenant.id).unwrap();
    assert_eq!(balance.available_minor_units, 1_000);

    // The rejected attempt must not even be recorded as a ledger entry:
    // a *new* idempotency_key attempting the same breach is independently rejected again.
    let (entries, _) = ledger.list_entries(&tenant.id, 100, "").unwrap();
    assert_eq!(entries.len(), 1, "only the original credit should be recorded");
}

#[test]
fn hold_is_floor_checked_exactly_like_debit() {
    let ledger = new_ledger("hold-floor");
    let tenant = ledger.create_tenant("Acme".into(), "USD".into(), 300).unwrap();
    ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Credit, Money::new(1_000, "USD").unwrap(), "seed".into(), "k1".into(), "".into())
        .unwrap();

    // headroom = 700; holding 700 is fine, holding 701 is not.
    let ok = ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Hold, Money::new(700, "USD").unwrap(), "hold".into(), "k2".into(), "".into())
        .unwrap();
    match ok {
        ApplyOutcome::Applied { balance, .. } => {
            assert_eq!(balance.available_minor_units, 300);
            assert_eq!(balance.held_minor_units, 700);
        }
        other => panic!("expected Applied, got {other:?}"),
    }

    let tenant2 = ledger.create_tenant("Beta".into(), "USD".into(), 300).unwrap();
    ledger
        .apply_entry(tenant2.id.clone(), LedgerEntryType::Credit, Money::new(1_000, "USD").unwrap(), "seed".into(), "k3".into(), "".into())
        .unwrap();
    let rejected = ledger
        .apply_entry(tenant2.id.clone(), LedgerEntryType::Hold, Money::new(701, "USD").unwrap(), "hold".into(), "k4".into(), "".into())
        .unwrap();
    assert!(matches!(rejected, ApplyOutcome::ReserveFloorViolated { .. }));
}

#[test]
fn credit_is_never_floor_checked_even_with_a_deeply_negative_headroom() {
    // Reserve floor far above the tenant's balance — if CREDIT were
    // (incorrectly) floor-checked, this would be rejected.
    let ledger = new_ledger("credit-unchecked");
    let tenant = ledger.create_tenant("Acme".into(), "USD".into(), 1_000_000).unwrap();

    let credit = ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Credit, Money::new(50, "USD").unwrap(), "seed".into(), "k1".into(), "".into())
        .unwrap();
    assert!(matches!(credit, ApplyOutcome::Applied { .. }));
}

#[test]
fn release_is_never_floor_checked_even_when_available_sits_at_zero() {
    // Zero reserve floor, but available balance is driven all the way
    // to zero by a HOLD first — RELEASE moving money back from held to
    // available must still succeed unconditionally.
    let ledger = new_ledger("release-unchecked");
    let tenant = ledger.create_tenant("Gamma".into(), "USD".into(), 0).unwrap();
    ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Credit, Money::new(100, "USD").unwrap(), "seed".into(), "k1".into(), "".into())
        .unwrap();
    ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Hold, Money::new(100, "USD").unwrap(), "hold".into(), "k2".into(), "".into())
        .unwrap();

    let release = ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Release, Money::new(100, "USD").unwrap(), "release".into(), "k3".into(), "".into())
        .unwrap();
    match release {
        ApplyOutcome::Applied { balance, .. } => {
            assert_eq!(balance.available_minor_units, 100);
            assert_eq!(balance.held_minor_units, 0);
        }
        other => panic!("expected Applied, got {other:?}"),
    }
}

#[test]
fn release_more_than_held_is_a_hard_error_not_a_soft_violation() {
    let ledger = new_ledger("release-overdraw");
    let tenant = ledger.create_tenant("Acme".into(), "USD".into(), 0).unwrap();
    ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Credit, Money::new(100, "USD").unwrap(), "seed".into(), "k1".into(), "".into())
        .unwrap();
    ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Hold, Money::new(40, "USD").unwrap(), "hold".into(), "k2".into(), "".into())
        .unwrap();

    let err = ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Release, Money::new(41, "USD").unwrap(), "release".into(), "k3".into(), "".into())
        .unwrap_err();

    assert!(matches!(
        err,
        ledger_core::error::LedgerError::InsufficientHeld { requested: 41, available: 40 }
    ));
}

#[test]
fn check_reserve_floor_rpc_math_matches_apply_entry() {
    let ledger = new_ledger("check-reserve-floor");
    let tenant = ledger.create_tenant("Acme".into(), "USD".into(), 200).unwrap();
    ledger
        .apply_entry(tenant.id.clone(), LedgerEntryType::Credit, Money::new(1_000, "USD").unwrap(), "seed".into(), "k1".into(), "".into())
        .unwrap();

    let (allowed, headroom) = ledger.check_reserve_floor(&tenant.id, 800).unwrap();
    assert!(allowed);
    assert_eq!(headroom, 800);

    let (allowed, headroom) = ledger.check_reserve_floor(&tenant.id, 801).unwrap();
    assert!(!allowed);
    assert_eq!(headroom, 800);
}
