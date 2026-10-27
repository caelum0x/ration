//! AES-256-GCM envelope encryption for ledger-core's two on-disk
//! stores — closes `docs/THREAT-MODEL.md`'s "Known gaps" #6: "the WAL
//! and SQLite index are fully plaintext on local disk."
//!
//! Two independent use sites share this one module's [`Key`]:
//!
//! 1. **WAL frames** (`crate::ledger`'s `append_and_commit`/
//!    `replay_wal`): every [`crate::ledger::WalEvent`] is serialized to
//!    JSON, then encrypted here, before `crate::wal_ffi::Wal::append`
//!    ever sees the bytes — the C WAL layer (`services/wal`) only ever
//!    sees opaque ciphertext frames. This is the right boundary for
//!    encryption, not a compromise: `wal_ffi`'s own interface is
//!    already `append(&mut self, payload: &[u8])` /
//!    `recover<F: FnMut(&[u8]) -> bool>` — a payload is already an
//!    opaque blob to the WAL's CRC/corruption-detection/truncation
//!    logic, which never interprets its contents. Encrypting here
//!    means every byte this process ever durably writes is ciphertext,
//!    with zero changes to the WAL's frame format, CRC computation, or
//!    recovery/truncation semantics (`services/wal/wal.c`,
//!    `docs/formal`'s TLA+ spec) — all of which continue to operate
//!    exactly as before on what is, from their point of view, still
//!    just a `payload: &[u8]`.
//! 2. **SQLite's sensitive free-text columns** (`crate::deposit`,
//!    `crate::agent_decision`, the `ledger_entries` table in
//!    `crate::ledger`): `external_ref`, `reference`, `memo`,
//!    `trigger`, `rationale` and `policy_snapshot_json` carry
//!    operator- and LLM-authored free text that can describe real
//!    money movements and business context in plain language — the
//!    columns actually worth protecting if the SQLite file itself
//!    leaks. `tenant_id`, amounts, timestamps, status/type enums and
//!    ids are deliberately left unencrypted: they are the WHERE/ORDER
//!    BY/JOIN columns every query in this crate already filters or
//!    sorts on, and AES-GCM ciphertext is neither order-preserving nor
//!    equality-stable in a way SQL could use anyway. `encrypt_opt`/
//!    `decrypt_opt` below are the shared read/write helpers every one
//!    of those columns' repository functions goes through.
//!
//!    `ledger_entries.reference` is the one column in this list a real
//!    query filters on (`crate::ledger`'s bridge-hold-release matching:
//!    a `LIKE 'bridge:<deposit_id>:%'` prefix match plus an exact-match
//!    "already released?" check) — exactly the case the paragraph above
//!    says ciphertext can't support. Rather than leave `reference`
//!    unencrypted as a special case, that one lookup was moved out of
//!    SQL: it now fetches every HOLD/RELEASE row for the tenant (already
//!    filtered server-side on the unencrypted `tenant_id`/`entry_type`
//!    columns, which the existing index covers), decrypts `reference`
//!    for each via `decrypt_opt`, and does the prefix/equality match in
//!    Rust — see `list_outstanding_bridge_holds_for_deposit`. At this
//!    project's real scale (a handful of holds per tenant) this is not a
//!    meaningful cost, and it means `reference` is protected with the
//!    same posture as every other listed column instead of being quietly
//!    exempted.
//!
//! Disabled by default (`Key::from_env` returns `None` when
//! `RATION_LEDGER_ENCRYPTION_KEY` is unset) — the same opt-in posture
//! every other security control in this project uses for a setting
//! that changes an on-disk format (mTLS, by contrast, defaults
//! differently because it never touches stored bytes). Enabling this
//! on an existing plaintext WAL/db is NOT a live migration: start from
//! a fresh `RATION_LEDGER_WAL_PATH`/`RATION_LEDGER_DB_PATH` once a key
//! is configured, exactly as `docs/RUNBOOK.md` should (and now does)
//! document.
//!
//! `RATION_LEDGER_ENCRYPTION_KEY` is a base64-encoded 32-byte key —
//! documented, per this gap's own wording, as "the dev stand-in for a
//! real KMS/Secrets Manager key" (see `infra/terraform/modules/service`
//! for this project's existing Secrets Manager wiring pattern a real
//! deployment would use to provision this value instead of an env
//! var).

use aes_gcm::aead::{Aead, AeadCore, KeyInit, OsRng};
use aes_gcm::{Aes256Gcm, Key as AesKey, Nonce};
use base64::engine::general_purpose::STANDARD as BASE64;
use base64::Engine;

/// Env var carrying the base64-encoded 32-byte AES-256 key. See this
/// module's doc comment for why this is a dev stand-in, not a
/// production KMS integration.
pub const ENV_KEY: &str = "RATION_LEDGER_ENCRYPTION_KEY";

/// Nonce length for AES-GCM, in bytes (96 bits — the standard,
/// recommended nonce size for AES-GCM; using any other size works but
/// is not recommended and this module never does).
const NONCE_LEN: usize = 12;

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("wal_crypto: {ENV_KEY} is not valid base64: {0}")]
    InvalidBase64(#[from] base64::DecodeError),
    #[error("wal_crypto: {ENV_KEY} must decode to exactly 32 bytes (AES-256), got {0}")]
    WrongKeyLength(usize),
    #[error("wal_crypto: encryption failed")]
    Encrypt,
    #[error("wal_crypto: decryption failed (wrong key, or data is not ciphertext this module wrote — did the encryption key change, or is RATION_LEDGER_ENCRYPTION_KEY newly set against a WAL/db written while it was unset?)")]
    Decrypt,
    #[error("wal_crypto: ciphertext is shorter than one nonce ({NONCE_LEN} bytes), cannot be data this module encrypted")]
    Truncated,
}

/// A loaded AES-256 key, ready to encrypt/decrypt. Cheap to clone
/// (wraps an `Aes256Gcm` cipher instance, itself cheap to clone).
#[derive(Clone)]
pub struct Key {
    cipher: Aes256Gcm,
}

impl Key {
    /// Loads a [`Key`] from `RATION_LEDGER_ENCRYPTION_KEY`, or returns
    /// `Ok(None)` when that env var is unset OR set to an empty string —
    /// encryption is simply disabled, not an error, matching every other
    /// opt-in security control in this project (see this module's doc
    /// comment) and, specifically, `main.rs`'s own `auth_grpc_addr`
    /// handling one function away (`if auth_grpc_addr.is_empty() {
    /// AuthVerifier::disabled() }`).
    ///
    /// The empty-string case matters in practice, not just in theory:
    /// `scripts/run-demo.sh`'s `load_env_file` falls back to
    /// `.env.example` for any var not already set in the real
    /// environment or `.env`, and `.env.example` documents this key as
    /// `RATION_LEDGER_ENCRYPTION_KEY=` (present, deliberately blank, the
    /// same convention used for `AUDIT_LOG_ADDR=` and every other
    /// optional value in that file) — which *sets* the var to `""`
    /// rather than leaving it absent. Before this fix, that made
    /// `Self::from_base64("")` run with a zero-byte input and fail with
    /// `Error::WrongKeyLength(0)`, which `main.rs` propagates as a hard
    /// startup error — meaning `scripts/run-demo.sh` from a totally
    /// clean checkout, with no `.env` and no env var ever set by hand,
    /// could not start ledger-core at all. Found and fixed verifying
    /// round 2's SQLite column-encryption work against the real demo
    /// stack, the same way round 1's bugs were found.
    pub fn from_env() -> Result<Option<Self>, Error> {
        match std::env::var(ENV_KEY) {
            Err(_) => Ok(None),
            Ok(encoded) if encoded.is_empty() => Ok(None),
            Ok(encoded) => Self::from_base64(&encoded).map(Some),
        }
    }

    /// Decodes a base64-encoded 32-byte key directly — the testable
    /// core `from_env` wraps.
    pub fn from_base64(encoded: &str) -> Result<Self, Error> {
        let raw = BASE64.decode(encoded)?;
        if raw.len() != 32 {
            return Err(Error::WrongKeyLength(raw.len()));
        }
        let key = AesKey::<Aes256Gcm>::from_slice(&raw);
        Ok(Self {
            cipher: Aes256Gcm::new(key),
        })
    }

    /// Encrypts `plaintext`, returning `nonce || ciphertext || tag`
    /// (aes-gcm appends the 16-byte authentication tag to the
    /// ciphertext automatically) as a single buffer — the exact
    /// encoding [`Key::decrypt`] expects back. A fresh random nonce is
    /// generated per call (`OsRng`, a CSPRNG) — AES-GCM's standard,
    /// safe nonce strategy as long as the same key is never reused
    /// anywhere near 2^32 times, which a single ledger-core process's
    /// realistic WAL-frame/row volume is nowhere close to.
    pub fn encrypt(&self, plaintext: &[u8]) -> Result<Vec<u8>, Error> {
        let nonce = Aes256Gcm::generate_nonce(&mut OsRng);
        let ciphertext = self
            .cipher
            .encrypt(&nonce, plaintext)
            .map_err(|_| Error::Encrypt)?;
        let mut out = Vec::with_capacity(NONCE_LEN + ciphertext.len());
        out.extend_from_slice(nonce.as_slice());
        out.extend_from_slice(&ciphertext);
        Ok(out)
    }

    /// Decrypts a buffer produced by [`Key::encrypt`]. Fails (rather
    /// than returning corrupted plaintext) on any tampering of either
    /// the nonce or ciphertext — AES-GCM's authentication tag makes
    /// this detection, not a best-effort guess.
    pub fn decrypt(&self, data: &[u8]) -> Result<Vec<u8>, Error> {
        if data.len() < NONCE_LEN {
            return Err(Error::Truncated);
        }
        let (nonce_bytes, ciphertext) = data.split_at(NONCE_LEN);
        let nonce = Nonce::from_slice(nonce_bytes);
        self.cipher
            .decrypt(nonce, ciphertext)
            .map_err(|_| Error::Decrypt)
    }

    /// Encrypts `plaintext` and base64-encodes the result — the
    /// encoding used for SQLite's sensitive TEXT columns (see this
    /// module's doc comment, point 2), so an encrypted value still
    /// round-trips cleanly through rusqlite's `TEXT` affinity and
    /// through `serde_json` when a `WalEvent` variant happens to
    /// nest one of these same strings in its own JSON (e.g. a
    /// released bridge-hold's `memo`).
    pub fn encrypt_text(&self, plaintext: &str) -> Result<String, Error> {
        Ok(BASE64.encode(self.encrypt(plaintext.as_bytes())?))
    }

    /// Decodes and decrypts a value produced by [`Key::encrypt_text`].
    pub fn decrypt_text(&self, encoded: &str) -> Result<String, Error> {
        let raw = BASE64.decode(encoded)?;
        let plaintext = self.decrypt(&raw)?;
        String::from_utf8(plaintext).map_err(|_| Error::Decrypt)
    }
}

/// Encrypts `plaintext` with [`Key::encrypt_text`] when `crypto` is
/// `Some`, otherwise returns it unchanged — the single helper every
/// SQLite free-text column write (`crate::deposit`, `crate::
/// agent_decision`, the `ledger_entries` functions in `crate::ledger`)
/// goes through, so column-level encryption is opt-in in exactly the
/// same way WAL encryption already is (see this module's doc comment,
/// point 2): a `None` key is a no-op, never an error.
pub fn encrypt_opt(crypto: Option<&Key>, plaintext: &str) -> Result<String, Error> {
    match crypto {
        Some(key) => key.encrypt_text(plaintext),
        None => Ok(plaintext.to_string()),
    }
}

/// Decrypts `stored` with [`Key::decrypt_text`] when `crypto` is `Some`,
/// otherwise returns it unchanged — the read-side counterpart of
/// [`encrypt_opt`]. `crypto` must be the same key (or `None`) used to
/// write `stored`, matching this crate's existing "not a live migration
/// path" rule for the WAL (see `Ledger::open_with_encryption`'s doc
/// comment) — a mismatch fails loudly via [`Error::Decrypt`] rather than
/// silently returning ciphertext or garbage.
pub fn decrypt_opt(crypto: Option<&Key>, stored: &str) -> Result<String, Error> {
    match crypto {
        Some(key) => key.decrypt_text(stored),
        None => Ok(stored.to_string()),
    }
}

/// Lets `encrypt_opt`/`decrypt_opt` be used directly with `?` inside
/// functions returning `rusqlite::Result<_>` (every repository function
/// in `crate::deposit`/`crate::agent_decision`/`crate::ledger` already
/// does) without each call site needing its own `.map_err(...)` — a
/// crypto failure becomes a real, surfaced SQLite-layer error
/// (`rusqlite::Error::ToSqlConversionFailure`, the variant rusqlite
/// itself defines for "a value couldn't be converted for storage/
/// retrieval"), never a silently-dropped one.
impl From<Error> for rusqlite::Error {
    fn from(e: Error) -> Self {
        rusqlite::Error::ToSqlConversionFailure(Box::new(e))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_key() -> Key {
        // 32 raw zero bytes, base64-encoded — fine for tests, never
        // use a fixed/known key in any real deployment.
        Key::from_base64("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=").unwrap()
    }

    #[test]
    fn from_env_is_none_when_unset() {
        // SAFETY (test-only; this var is unique to this test file and
        // not touched by any other test, but see below for the one test
        // that DOES also touch it — run together, in this same function,
        // precisely to avoid a cross-test race on this process-global
        // var under cargo test's default parallelism): remove, rather
        // than assume absent.
        std::env::remove_var(ENV_KEY);
        assert!(Key::from_env().unwrap().is_none());

        // Regression test, same var, same function (see SAFETY note
        // above): scripts/run-demo.sh's load_env_file falls back to
        // .env.example's `RATION_LEDGER_ENCRYPTION_KEY=` (a set, but
        // empty, value) whenever the var isn't already set in the real
        // environment or .env — this must disable encryption, not fail
        // with WrongKeyLength(0), or a totally clean `scripts/
        // run-demo.sh` run can never start ledger-core at all.
        std::env::set_var(ENV_KEY, "");
        assert!(
            Key::from_env().unwrap().is_none(),
            "an empty (but set) RATION_LEDGER_ENCRYPTION_KEY must disable encryption, not error"
        );
        std::env::remove_var(ENV_KEY);
    }

    #[test]
    fn encrypt_then_decrypt_round_trips() {
        let key = test_key();
        let plaintext = b"bridge funding: payout delayed, granting $300.00";
        let ciphertext = key.encrypt(plaintext).unwrap();
        assert_ne!(ciphertext, plaintext, "ciphertext must not equal plaintext");
        let decrypted = key.decrypt(&ciphertext).unwrap();
        assert_eq!(decrypted, plaintext);
    }

    #[test]
    fn encrypt_text_then_decrypt_text_round_trips() {
        let key = test_key();
        let plaintext = "granted $300.00 of $300.00 requested: reserve floor respected";
        let encoded = key.encrypt_text(plaintext).unwrap();
        assert_ne!(encoded, plaintext);
        let decrypted = key.decrypt_text(&encoded).unwrap();
        assert_eq!(decrypted, plaintext);
    }

    #[test]
    fn two_encryptions_of_the_same_plaintext_differ() {
        // Proves the nonce is actually random per call, not reused —
        // a reused nonce under the same key is a catastrophic AES-GCM
        // key-recovery vulnerability, not just a privacy nicety.
        let key = test_key();
        let a = key.encrypt(b"same plaintext").unwrap();
        let b = key.encrypt(b"same plaintext").unwrap();
        assert_ne!(a, b);
    }

    #[test]
    fn decrypt_rejects_tampered_ciphertext() {
        let key = test_key();
        let mut ciphertext = key.encrypt(b"authentic").unwrap();
        *ciphertext.last_mut().unwrap() ^= 0xFF;
        assert!(key.decrypt(&ciphertext).is_err());
    }

    #[test]
    fn decrypt_rejects_wrong_key() {
        let key_a = test_key();
        let key_b = Key::from_base64("AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=").unwrap();
        let ciphertext = key_a.encrypt(b"secret").unwrap();
        assert!(key_b.decrypt(&ciphertext).is_err());
    }

    #[test]
    fn decrypt_rejects_truncated_input() {
        let key = test_key();
        assert!(key.decrypt(&[1, 2, 3]).is_err());
    }

    #[test]
    fn from_base64_rejects_wrong_length() {
        // "AAAA" base64-decodes to 3 bytes, not 32.
        assert!(matches!(
            Key::from_base64("AAAA"),
            Err(Error::WrongKeyLength(3))
        ));
    }

    #[test]
    fn from_base64_rejects_invalid_base64() {
        assert!(matches!(
            Key::from_base64("not valid base64!!"),
            Err(Error::InvalidBase64(_))
        ));
    }

    #[test]
    fn encrypt_opt_is_passthrough_when_crypto_is_none() {
        let plaintext = "memo: bridge funding granted";
        let stored = encrypt_opt(None, plaintext).unwrap();
        assert_eq!(stored, plaintext, "no key configured means no encryption");
    }

    #[test]
    fn decrypt_opt_is_passthrough_when_crypto_is_none() {
        let stored = "memo: bridge funding granted";
        let plaintext = decrypt_opt(None, stored).unwrap();
        assert_eq!(plaintext, stored);
    }

    #[test]
    fn encrypt_opt_then_decrypt_opt_round_trips_when_crypto_is_some() {
        let key = test_key();
        let plaintext = "rationale: headroom covers the full requested amount";
        let stored = encrypt_opt(Some(&key), plaintext).unwrap();
        assert_ne!(
            stored, plaintext,
            "a column encrypted with a real key must not be stored as plaintext"
        );
        let decrypted = decrypt_opt(Some(&key), &stored).unwrap();
        assert_eq!(decrypted, plaintext);
    }

    #[test]
    fn rusqlite_error_from_crypto_error_is_to_sql_conversion_failure() {
        let key = test_key();
        let err: rusqlite::Error = Error::Decrypt.into();
        match err {
            rusqlite::Error::ToSqlConversionFailure(_) => {}
            other => panic!("expected ToSqlConversionFailure, got {other:?}"),
        }
        // Exercises the real `?`-based conversion path `encrypt_opt`/
        // `decrypt_opt` callers rely on: decrypting a non-ciphertext
        // string under a real key must surface as a real rusqlite
        // error, not panic or silently return garbage.
        assert!(decrypt_opt(Some(&key), "not valid ciphertext").is_err());
    }
}
