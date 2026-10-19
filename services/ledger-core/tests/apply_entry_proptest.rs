//! Property-based test for `ledger_core::ledger::Ledger::apply_entry`.
//!
//! Invariant under test (docs/SCALE-EXPANSION.md section 5): no sequence
//! of (possibly concurrent) `ApplyEntry` calls can ever push a tenant's
//! available balance below its `reserve_floor`. Two complementary
//! proptest properties exercise this against randomly generated, varied
//! input rather than a handful of fixed cases:
//!
//!  1. `prop_sequential_apply_matches_oracle_and_never_breaches_floor` —
//!     generates a random reserve floor and a random sequence (1-30 ops)
//!     of CREDIT/DEBIT/HOLD/RELEASE entries with random amounts, applies
//!     them one at a time against a real `Ledger`, and after every call
//!     cross-checks the result against an independent shadow-model
//!     oracle (reimplementing the spec from `apply_entry`'s own doc
//!     comment, deliberately kept separate from `ledger.rs` so this test
//!     can't pass by construction) and asserts the live balance never
//!     reads below the floor after any applied DEBIT/HOLD. proptest
//!     shrinks any failing case to a minimal repro automatically.
//!  2. `prop_concurrent_apply_never_breaches_floor` — fires random
//!     batches of entries at the *same* `Arc<Ledger>` from multiple OS
//!     threads simultaneously (the actual "concurrent ApplyEntry calls"
//!     the invariant names), asserts every single completed Applied
//!     DEBIT/HOLD outcome — in whatever order the ledger's internal
//!     mutex happened to serialize them — respected the floor, and then
//!     reconciles the full `ListEntries` log against `GetBalance` to
//!     prove nothing was lost or double-applied once everything settles.

use std::path::PathBuf;
use std::sync::{Arc, Barrier, Mutex};
use std::thread;

use ledger_core::ledger::{ApplyOutcome, Ledger, LedgerEntryType};
use ledger_core::money::Money;
use proptest::prelude::*;

const CURRENCY: &str = "USD";

fn temp_wal_path(tag: &str) -> PathBuf {
    std::env::temp_dir().join(format!(
        "ration-ledger-proptest-{tag}-{}-{}.wal",
        std::process::id(),
        uuid::Uuid::new_v4()
    ))
}

#[derive(Debug, Clone, Copy)]
enum OpKind {
    Credit,
    Debit,
    Hold,
    Release,
}

impl OpKind {
    fn to_entry_type(self) -> LedgerEntryType {
        match self {
            OpKind::Credit => LedgerEntryType::Credit,
            OpKind::Debit => LedgerEntryType::Debit,
            OpKind::Hold => LedgerEntryType::Hold,
            OpKind::Release => LedgerEntryType::Release,
        }
    }
}

fn op_kind_strategy() -> impl Strategy<Value = OpKind> {
    prop_oneof![
        Just(OpKind::Credit),
        Just(OpKind::Debit),
        Just(OpKind::Hold),
        Just(OpKind::Release),
    ]
}

/// Amounts deliberately range well past a typical reserve floor so a
/// healthy fraction of generated DEBIT/HOLD ops actually attempt to
/// breach it (exercising the rejection path), not just comfortably clear
/// it every time.
fn amount_strategy() -> impl Strategy<Value = i64> {
    0i64..150_000
}

fn reserve_floor_strategy() -> impl Strategy<Value = i64> {
    0i64..100_000
}

fn ops_strategy(max_len: usize) -> impl Strategy<Value = Vec<(OpKind, i64)>> {
    prop::collection::vec((op_kind_strategy(), amount_strategy()), 1..max_len)
}

/// Independent shadow model of exactly what `Ledger::apply_entry` should
/// do, per its own doc comment — kept deliberately separate from
/// `ledger.rs`'s implementation so this test cannot pass by construction
/// (a bug duplicated in both places would still be caught by the live
/// reserve-floor assertion below).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct ShadowBalance {
    available: i64,
    held: i64,
}

#[derive(Debug)]
enum ShadowOutcome {
    Applied(ShadowBalance),
    ReserveFloorViolated,
    InsufficientHeld,
}

fn shadow_apply(balance: ShadowBalance, floor: i64, kind: OpKind, amt: i64) -> ShadowOutcome {
    match kind {
        OpKind::Credit => ShadowOutcome::Applied(ShadowBalance {
            available: balance.available + amt,
            held: balance.held,
        }),
        OpKind::Debit => {
            let headroom = balance.available - floor;
            if amt > headroom {
                ShadowOutcome::ReserveFloorViolated
            } else {
                ShadowOutcome::Applied(ShadowBalance {
                    available: balance.available - amt,
                    held: balance.held,
                })
            }
        }
        OpKind::Hold => {
            let headroom = balance.available - floor;
            if amt > headroom {
                ShadowOutcome::ReserveFloorViolated
            } else {
                ShadowOutcome::Applied(ShadowBalance {
                    available: balance.available - amt,
                    held: balance.held + amt,
                })
            }
        }
        OpKind::Release => {
            if amt > balance.held {
                ShadowOutcome::InsufficientHeld
            } else {
                ShadowOutcome::Applied(ShadowBalance {
                    available: balance.available + amt,
                    held: balance.held - amt,
                })
            }
        }
    }
}

/// Sums a slice of applied ledger entries into a `ShadowBalance` by
/// simple arithmetic accumulation — deliberately order-independent
/// (addition commutes) unlike a strict replay, because
/// `LedgerEntryRecord.created_at_unix` only has one-second resolution:
/// under genuine concurrency many entries legitimately tie on that
/// timestamp, so `ListEntries`' `ORDER BY created_at_unix, id` does not
/// reliably reflect true commit order. Order-independent summation is
/// still a fully valid proof that nothing was lost or double-applied —
/// if it were, the sum would not reconcile with `GetBalance`.
fn sum_entries(entries: &[ledger_core::ledger::LedgerEntryRecord]) -> ShadowBalance {
    let mut balance = ShadowBalance {
        available: 0,
        held: 0,
    };
    for e in entries {
        balance = match e.entry_type {
            LedgerEntryType::Credit => ShadowBalance {
                available: balance.available + e.amount_minor_units,
                held: balance.held,
            },
            LedgerEntryType::Debit => ShadowBalance {
                available: balance.available - e.amount_minor_units,
                held: balance.held,
            },
            LedgerEntryType::Hold => ShadowBalance {
                available: balance.available - e.amount_minor_units,
                held: balance.held + e.amount_minor_units,
            },
            LedgerEntryType::Release => ShadowBalance {
                available: balance.available + e.amount_minor_units,
                held: balance.held - e.amount_minor_units,
            },
            LedgerEntryType::Unspecified => balance,
        };
    }
    balance
}

proptest! {
    #![proptest_config(ProptestConfig::with_cases(48))]

    #[test]
    fn prop_sequential_apply_matches_oracle_and_never_breaches_floor(
        reserve_floor in reserve_floor_strategy(),
        ops in ops_strategy(30),
    ) {
        let wal_path = temp_wal_path("seq");
        let (ledger, _) = Ledger::open_with_memory_db(&wal_path).expect("open ledger");
        let tenant = ledger
            .create_tenant("Acme".into(), CURRENCY.into(), reserve_floor)
            .expect("create tenant");

        let mut shadow = ShadowBalance { available: 0, held: 0 };

        for (idx, (kind, amt)) in ops.iter().enumerate() {
            let expected = shadow_apply(shadow, reserve_floor, *kind, *amt);

            let result = ledger.apply_entry(
                tenant.id.clone(),
                kind.to_entry_type(),
                Money::new(*amt, CURRENCY).unwrap(),
                "proptest".into(),
                format!("proptest-op-{idx}-{}", uuid::Uuid::new_v4()),
                "".into(),
            );

            match (&expected, result) {
                (ShadowOutcome::Applied(next), Ok(ApplyOutcome::Applied { balance, .. })) => {
                    prop_assert_eq!(balance.available_minor_units, next.available);
                    prop_assert_eq!(balance.held_minor_units, next.held);
                    shadow = *next;
                }
                (
                    ShadowOutcome::ReserveFloorViolated,
                    Ok(ApplyOutcome::ReserveFloorViolated { balance, .. }),
                ) => {
                    // A rejected DEBIT/HOLD must leave the balance
                    // completely unchanged — the WAL must never have
                    // been written to for this call.
                    prop_assert_eq!(balance.available_minor_units, shadow.available);
                    prop_assert_eq!(balance.held_minor_units, shadow.held);
                }
                (ShadowOutcome::InsufficientHeld, Err(_)) => {
                    // amount unchanged; shadow stays put.
                }
                (expected_kind, actual) => {
                    prop_assert!(
                        false,
                        "op {idx} ({kind:?}, amt={amt}) diverged from oracle: expected {expected_kind:?}, got {actual:?}"
                    );
                }
            }

            // The invariant itself: after any DEBIT/HOLD that the ledger
            // actually applied (not one it rejected as a floor
            // violation), the tenant's live balance must never read
            // below its reserve floor. A tenant can start out under its
            // own floor (e.g. floor=100, balance=0 right after
            // creation) — that is a pre-existing condition, not a
            // breach caused by this apply_entry call, so the check is
            // scoped to the Applied case specifically.
            if matches!(kind, OpKind::Debit | OpKind::Hold)
                && matches!(expected, ShadowOutcome::Applied(_))
            {
                let (live, _currency) = ledger.get_balance(&tenant.id).unwrap();
                prop_assert!(
                    live.available_minor_units >= reserve_floor,
                    "reserve floor breached after op {idx}: available={} floor={}",
                    live.available_minor_units, reserve_floor
                );
            }
        }

        let _ = std::fs::remove_file(&wal_path);
    }
}

proptest! {
    #![proptest_config(ProptestConfig::with_cases(24))]

    #[test]
    fn prop_concurrent_apply_never_breaches_floor(
        reserve_floor in reserve_floor_strategy(),
        initial_credit in 0i64..200_000,
        thread_ops in prop::collection::vec(ops_strategy(10), 2..6),
    ) {
        let wal_path = temp_wal_path("concurrent");
        let (ledger, _) = Ledger::open_with_memory_db(&wal_path).expect("open ledger");
        let tenant = ledger
            .create_tenant("Acme".into(), CURRENCY.into(), reserve_floor)
            .expect("create tenant");

        // Seed some available balance so DEBIT/HOLD have something to
        // actually contend over, rather than every concurrent op
        // trivially bouncing off a zero balance.
        if initial_credit > 0 {
            ledger
                .apply_entry(
                    tenant.id.clone(),
                    LedgerEntryType::Credit,
                    Money::new(initial_credit, CURRENCY).unwrap(),
                    "seed".into(),
                    format!("seed-{}", uuid::Uuid::new_v4()),
                    "".into(),
                )
                .unwrap();
        }

        let ledger = Arc::new(ledger);
        let tenant_id = tenant.id.clone();
        let n_threads = thread_ops.len();
        let barrier = Arc::new(Barrier::new(n_threads));
        let violations: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));

        let handles: Vec<_> = thread_ops
            .into_iter()
            .enumerate()
            .map(|(t_idx, ops)| {
                let ledger = Arc::clone(&ledger);
                let tenant_id = tenant_id.clone();
                let barrier = Arc::clone(&barrier);
                let violations = Arc::clone(&violations);
                thread::spawn(move || {
                    // Line every thread up so the calls genuinely overlap
                    // instead of running one after another by accident.
                    barrier.wait();
                    for (op_idx, (kind, amt)) in ops.into_iter().enumerate() {
                        let result = ledger.apply_entry(
                            tenant_id.clone(),
                            kind.to_entry_type(),
                            Money::new(amt, CURRENCY).unwrap(),
                            "proptest-concurrent".into(),
                            format!("proptest-concurrent-{t_idx}-{op_idx}-{}", uuid::Uuid::new_v4()),
                            "".into(),
                        );
                        if let Ok(ApplyOutcome::Applied { balance, .. }) = result {
                            if matches!(kind, OpKind::Debit | OpKind::Hold)
                                && balance.available_minor_units < reserve_floor
                            {
                                violations.lock().unwrap().push(format!(
                                    "thread {t_idx} op {op_idx}: available {} < floor {}",
                                    balance.available_minor_units, reserve_floor
                                ));
                            }
                        }
                    }
                })
            })
            .collect();

        for h in handles {
            h.join().expect("worker thread panicked");
        }

        let violations = violations.lock().unwrap().clone();
        prop_assert!(
            violations.is_empty(),
            "reserve floor breaches under concurrency: {violations:?}"
        );

        // Reconciliation: the WAL/SQLite-indexed entry log must exactly
        // explain the final balance — i.e. nothing any thread's
        // apply_entry call reported as Applied was silently lost or
        // double-counted once everything is serialized through the
        // ledger's internal mutex.
        let (final_balance, _currency) = ledger.get_balance(&tenant_id).unwrap();
        let (entries, _) = ledger.list_entries(&tenant_id, 500, "").unwrap();
        let summed = sum_entries(&entries);
        prop_assert_eq!(
            final_balance.available_minor_units, summed.available,
            "GetBalance available does not reconcile with the sum of ListEntries — an entry was lost or double-applied"
        );
        prop_assert_eq!(
            final_balance.held_minor_units, summed.held,
            "GetBalance held does not reconcile with the sum of ListEntries — an entry was lost or double-applied"
        );

        let _ = std::fs::remove_file(&wal_path);
    }
}
