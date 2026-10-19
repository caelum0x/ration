//! `MarkDepositReceived` is the only path by which an expected deposit
//! turns into spendable headroom. Before this test existed, marking a
//! deposit received updated the deposit's status but never touched the
//! tenant's balance — every reserve-floor and bridge-envelope
//! computation elsewhere in this crate would see a balance stuck at
//! zero forever, since `RecordDeposit`/`ApplyEntry` are never wired
//! together anywhere else in the system.

use std::path::PathBuf;

use ledger_core::deposit::DepositStatus;
use ledger_core::ledger::Ledger;
use ledger_core::money::Money;

fn temp_wal_path(tag: &str) -> PathBuf {
    let dir = std::env::temp_dir();
    dir.join(format!(
        "ration-deposit-credit-{tag}-{}-{}.wal",
        std::process::id(),
        uuid::Uuid::new_v4()
    ))
}

fn new_ledger(tag: &str) -> Ledger {
    let (ledger, _) = Ledger::open_with_memory_db(&temp_wal_path(tag)).unwrap();
    ledger
}

#[test]
fn marking_a_deposit_received_credits_the_tenants_available_balance() {
    let ledger = new_ledger("credits-balance");
    let tenant = ledger
        .create_tenant("Acme".into(), "USD".into(), 200)
        .unwrap();

    let deposit = ledger
        .record_deposit(
            tenant.id.clone(),
            Money::new(5_000, "USD").unwrap(),
            1_000,
            "ext-1".into(),
        )
        .unwrap();

    let (balance_before, _) = ledger.get_balance(&tenant.id).unwrap();
    assert_eq!(balance_before.available_minor_units, 0);

    let updated = ledger
        .mark_deposit_received(deposit.id.clone(), 2_000, "txn-1".into())
        .unwrap();
    assert_eq!(updated.status, DepositStatus::Received);

    let (balance_after, _) = ledger.get_balance(&tenant.id).unwrap();
    assert_eq!(
        balance_after.available_minor_units, 5_000,
        "available balance must rise by the deposit's full expected amount once received"
    );
}

#[test]
fn marking_the_same_deposit_received_twice_does_not_double_credit() {
    let ledger = new_ledger("no-double-credit");
    let tenant = ledger
        .create_tenant("Acme".into(), "USD".into(), 0)
        .unwrap();

    let deposit = ledger
        .record_deposit(
            tenant.id.clone(),
            Money::new(1_000, "USD").unwrap(),
            500,
            "ext-1".into(),
        )
        .unwrap();

    ledger
        .mark_deposit_received(deposit.id.clone(), 600, "txn-1".into())
        .unwrap();

    // A retried webhook delivery or duplicate RPC call for the same
    // deposit must be a no-op against the balance, not a second credit.
    ledger
        .mark_deposit_received(deposit.id.clone(), 600, "txn-1-retry".into())
        .unwrap();

    let (balance, _) = ledger.get_balance(&tenant.id).unwrap();
    assert_eq!(balance.available_minor_units, 1_000);
}

#[test]
fn a_credited_deposit_makes_the_reserve_floor_envelope_math_non_zero() {
    // The end-to-end point of this fix: without it, every tenant's
    // headroom is permanently stuck at `0 - reserve_floor` (always
    // negative), so the agent's bridge envelope can never grant
    // anything, no matter how the late deposit itself is sized.
    let ledger = new_ledger("unblocks-headroom");
    let tenant = ledger
        .create_tenant("Acme".into(), "USD".into(), 1_000)
        .unwrap();

    let deposit = ledger
        .record_deposit(
            tenant.id.clone(),
            Money::new(10_000, "USD").unwrap(),
            500,
            "ext-1".into(),
        )
        .unwrap();
    ledger
        .mark_deposit_received(deposit.id.clone(), 600, "txn-1".into())
        .unwrap();

    let (allowed, headroom) = ledger.check_reserve_floor(&tenant.id, 0).unwrap();
    assert!(allowed);
    assert_eq!(
        headroom, 9_000,
        "headroom = available (10,000) - reserve_floor (1,000)"
    );
}
