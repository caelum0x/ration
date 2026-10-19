use std::path::PathBuf;

fn main() {
    // Client stubs for gateway.proto (and common.proto, which it
    // imports) — this crate only ever *calls* a Gateway service, never
    // implements one. Server stubs are generated too, same as
    // services/ledger-core/build.rs does for audit.proto: this crate's
    // own tests stand up a fake Gateway server (implementing the
    // generated server trait) to exercise the client against, so both
    // sides are needed here even though only the client is part of the
    // public API (src/lib.rs re-exports no server types).
    let proto_dir = PathBuf::from("../../../proto");
    let gateway_proto = proto_dir.join("gateway.proto");
    let common_proto = proto_dir.join("common.proto");

    assert!(
        gateway_proto.exists(),
        "expected {} to exist",
        gateway_proto.display()
    );
    assert!(
        common_proto.exists(),
        "expected {} to exist",
        common_proto.display()
    );

    println!("cargo:rerun-if-changed={}", gateway_proto.display());
    println!("cargo:rerun-if-changed={}", common_proto.display());

    tonic_build::configure()
        .build_server(true)
        .build_client(true)
        .compile_protos(&[gateway_proto, common_proto], &[proto_dir])
        .expect("failed to compile gateway.proto/common.proto with tonic-build");
}
