use std::path::PathBuf;

fn main() {
    // --- Compile the C write-ahead log (services/wal) directly from its
    // sources. We deliberately do NOT depend on services/wal's own
    // Makefile/libwal.a output: compiling wal.c and crc32.c ourselves via
    // the `cc` crate keeps this crate self-contained and buildable with
    // nothing but `cargo build`.
    let wal_dir = PathBuf::from("../wal");
    let wal_c = wal_dir.join("wal.c");
    let crc32_c = wal_dir.join("crc32.c");

    cc::Build::new()
        .file(&wal_c)
        .file(&crc32_c)
        .include(&wal_dir)
        .flag_if_supported("-std=c11")
        .warnings(true)
        .compile("wal");

    println!("cargo:rerun-if-changed={}", wal_c.display());
    println!("cargo:rerun-if-changed={}", crc32_c.display());
    println!("cargo:rerun-if-changed={}", wal_dir.join("wal.h").display());
    println!(
        "cargo:rerun-if-changed={}",
        wal_dir.join("frame.h").display()
    );
    println!(
        "cargo:rerun-if-changed={}",
        wal_dir.join("crc32.h").display()
    );

    // --- Compile proto/*.proto with tonic-build. ledger-core only serves
    // the LedgerCore service, but we compile every proto in the shared
    // contracts directory (as common.proto and gateway.proto are part of
    // the same package family and common.proto is imported by the
    // others) so a single source of generated Rust types stays in sync
    // with the Go stubs in pkg/pb.
    let proto_dir = PathBuf::from("../../proto");
    let mut proto_files: Vec<PathBuf> = std::fs::read_dir(&proto_dir)
        .expect("failed to read proto/ directory — expected it next to services/")
        .filter_map(|entry| entry.ok())
        .map(|entry| entry.path())
        .filter(|path| path.extension().and_then(|e| e.to_str()) == Some("proto"))
        .collect();
    proto_files.sort();

    for proto_file in &proto_files {
        println!("cargo:rerun-if-changed={}", proto_file.display());
    }

    // audit.proto is compiled in its own pass with client stubs on top
    // of the usual server stubs: ledger-core is a *client* of AuditLog
    // in production (see docs/SCALE-EXPANSION.md section 8,
    // src/audit_mirror.rs) and never runs an AuditLog server, but
    // src/audit_mirror.rs's own test suite stands up a fake AuditLog
    // server (implementing the generated server trait) to exercise
    // the client against — so both sides are generated here, unlike
    // every other proto file below, which only ever needs server
    // stubs for the service(s) ledger-core actually implements.
    let audit_proto = proto_dir.join("audit.proto");
    let (audit_protos, server_only_protos): (Vec<PathBuf>, Vec<PathBuf>) =
        proto_files.into_iter().partition(|p| p == &audit_proto);
    assert!(
        !audit_protos.is_empty(),
        "expected {} to exist alongside the other proto/*.proto files",
        audit_proto.display()
    );

    tonic_build::configure()
        .build_server(true)
        .build_client(false)
        .compile_protos(&server_only_protos, std::slice::from_ref(&proto_dir))
        .expect("failed to compile proto/*.proto (server-only stubs) with tonic-build");

    tonic_build::configure()
        .build_server(true)
        .build_client(true)
        .compile_protos(&audit_protos, &[proto_dir])
        .expect("failed to compile proto/audit.proto (client+server stubs) with tonic-build");
}
