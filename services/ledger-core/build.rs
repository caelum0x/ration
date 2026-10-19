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
    println!("cargo:rerun-if-changed={}", wal_dir.join("frame.h").display());
    println!("cargo:rerun-if-changed={}", wal_dir.join("crc32.h").display());

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

    tonic_build::configure()
        .build_server(true)
        .build_client(false)
        .compile_protos(&proto_files, &[proto_dir])
        .expect("failed to compile proto/*.proto with tonic-build");
}
