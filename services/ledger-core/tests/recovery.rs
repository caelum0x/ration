//! Proves the WAL is actually the durability source of truth: this test
//! simulates a crash mid-write by appending a deliberately torn frame
//! directly to the WAL file on disk (bypassing the `Wal` API entirely —
//! byte-for-byte what a real process crash leaves behind: a frame
//! header written, but its payload only partially written and never
//! fsynced), then deletes the SQLite index outright and "restarts" by
//! reopening the ledger against nothing but the WAL file. Recovery must
//! truncate the torn tail cleanly and reconstruct every pre-crash entry
//! with no loss and no double-application.

use std::io::Write;
use std::path::{Path, PathBuf};

use ledger_core::ledger::{ApplyOutcome, Ledger, LedgerEntryType};
use ledger_core::money::Money;

fn temp_paths(tag: &str) -> (PathBuf, PathBuf) {
    let dir = std::env::temp_dir();
    let unique = uuid::Uuid::new_v4();
    let wal = dir.join(format!("ration-recovery-{tag}-{unique}.wal"));
    let db = dir.join(format!("ration-recovery-{tag}-{unique}.db"));
    let _ = std::fs::remove_file(&wal);
    let _ = std::fs::remove_file(&db);
    (wal, db)
}

/// Appends a torn frame directly to the WAL file, bypassing the `Wal`
/// API entirely. A real header (length=500, some CRC) followed by only
/// 7 bytes of "payload" and no fsync — exactly what's on disk if the
/// process died partway through writing a frame.
fn simulate_crash_mid_write(wal_path: &Path) {
    let mut f = std::fs::OpenOptions::new()
        .append(true)
        .open(wal_path)
        .expect("WAL file must already exist before simulating a crash onto it");
    let mut header = [0u8; 8];
    header[0..4].copy_from_slice(&500u32.to_le_bytes());
    header[4..8].copy_from_slice(&0xDEAD_BEEFu32.to_le_bytes());
    f.write_all(&header).unwrap();
    f.write_all(b"torndat").unwrap();
    f.flush().unwrap();
}

#[test]
fn crash_mid_write_loses_nothing_and_double_applies_nothing_on_restart() {
    let (wal_path, db_path) = temp_paths("case1");

    // --- Phase 1: normal operation, several fully-durable entries.
    let tenant_id = {
        let (ledger, replayed) = Ledger::open(&wal_path, &db_path).unwrap();
        assert_eq!(replayed, 0, "fresh WAL has nothing to replay");

        let tenant = ledger
            .create_tenant("Acme".into(), "USD".into(), 1_000)
            .unwrap();

        ledger
            .apply_entry(
                tenant.id.clone(),
                LedgerEntryType::Credit,
                Money::new(10_000, "USD").unwrap(),
                "seed".into(),
                "evt-1".into(),
                "".into(),
            )
            .unwrap();
        ledger
            .apply_entry(
                tenant.id.clone(),
                LedgerEntryType::Debit,
                Money::new(2_000, "USD").unwrap(),
                "spend".into(),
                "evt-2".into(),
                "".into(),
            )
            .unwrap();
        ledger
            .apply_entry(
                tenant.id.clone(),
                LedgerEntryType::Hold,
                Money::new(1_500, "USD").unwrap(),
                "hold".into(),
                "evt-3".into(),
                "".into(),
            )
            .unwrap();

        let (balance, _) = ledger.get_balance(&tenant.id).unwrap();
        assert_eq!(balance.available_minor_units, 10_000 - 2_000 - 1_500);
        assert_eq!(balance.held_minor_units, 1_500);

        tenant.id
        // `ledger` is dropped here — its Wal fd and sqlite connection
        // go away, simulating the process disappearing.
    };

    // --- Phase 2: simulate a crash mid-write on top of those 4 good
    // frames (1 TenantCreated + 3 EntryApplied).
    simulate_crash_mid_write(&wal_path);

    // Wipe the SQLite index entirely: restart must reconstruct every
    // bit of state purely from the WAL, proving the WAL — not the
    // index — is the durability source of truth.
    std::fs::remove_file(&db_path).unwrap();

    // --- Phase 3: "restart".
    let (ledger, replayed) = Ledger::open(&wal_path, &db_path)
        .expect("recovery must truncate the torn tail cleanly, not error out");
    assert_eq!(
        replayed, 4,
        "exactly the 4 pre-crash frames must replay; the torn frame must be dropped, not counted"
    );

    let (balance, _) = ledger.get_balance(&tenant_id).unwrap();
    assert_eq!(
        balance.available_minor_units,
        10_000 - 2_000 - 1_500,
        "no entry lost after recovery"
    );
    assert_eq!(
        balance.held_minor_units, 1_500,
        "no entry lost after recovery"
    );

    let (entries, _) = ledger.list_entries(&tenant_id, 100, "").unwrap();
    assert_eq!(
        entries.len(),
        3,
        "no entry lost and none double-applied after recovery"
    );

    // --- Phase 4: the WAL must still be appendable after recovery
    // truncated the torn tail — prove it by applying one more entry.
    let outcome = ledger
        .apply_entry(
            tenant_id.clone(),
            LedgerEntryType::Release,
            Money::new(500, "USD").unwrap(),
            "partial-release".into(),
            "evt-4".into(),
            "".into(),
        )
        .unwrap();
    match outcome {
        ApplyOutcome::Applied { balance, .. } => {
            assert_eq!(balance.held_minor_units, 1_000);
            assert_eq!(balance.available_minor_units, 10_000 - 2_000 - 1_500 + 500);
        }
        other => panic!("expected Applied, got {other:?}"),
    }
    drop(ledger);

    // --- Phase 5: restart again (no crash this time) to prove that
    // replaying an *already fully-indexed* WAL does not double-apply
    // anything either: the balance must be byte-for-byte identical.
    let (ledger2, replayed2) = Ledger::open(&wal_path, &db_path).unwrap();
    assert_eq!(
        replayed2, 5,
        "all 5 good frames (including the post-recovery append) replay"
    );

    let (balance2, _) = ledger2.get_balance(&tenant_id).unwrap();
    assert_eq!(balance2.available_minor_units, 10_000 - 2_000 - 1_500 + 500);
    assert_eq!(balance2.held_minor_units, 1_000);

    let (entries2, _) = ledger2.list_entries(&tenant_id, 100, "").unwrap();
    assert_eq!(
        entries2.len(),
        4,
        "still exactly 4 entries — nothing double-applied across two restarts"
    );
}

#[test]
fn recover_on_a_wal_with_no_torn_tail_is_a_pure_pass_through() {
    let (wal_path, db_path) = temp_paths("case2-clean");

    let tenant_id = {
        let (ledger, _) = Ledger::open(&wal_path, &db_path).unwrap();
        let tenant = ledger
            .create_tenant("Beta".into(), "USD".into(), 0)
            .unwrap();
        ledger
            .apply_entry(
                tenant.id.clone(),
                LedgerEntryType::Credit,
                Money::new(300, "USD").unwrap(),
                "seed".into(),
                "evt-1".into(),
                "".into(),
            )
            .unwrap();
        tenant.id
    };

    // No simulated crash this time — a perfectly clean shutdown/restart.
    let (ledger, replayed) = Ledger::open(&wal_path, &db_path).unwrap();
    assert_eq!(replayed, 2);
    let (balance, _) = ledger.get_balance(&tenant_id).unwrap();
    assert_eq!(balance.available_minor_units, 300);
}
