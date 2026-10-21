// Package store implements audit-log's append-only, hash-chained
// ledger. It is deliberately the simplest thing that can honestly claim
// "append-only and tamper-evident": every record is durably appended to
// a plain file (fsync'd before the call that produced it is
// acknowledged) and chained to the previous record by
// sha256(prev_hash || canonical record bytes), so deleting or editing
// any earlier line breaks every hash after it. VerifyChain re-derives
// the whole chain from the file and reports the first break.
//
// This is intentionally a *separate* durability mechanism from
// ledger-core's own WAL (see services/wal, services/ledger-core): a bug
// or compromise in ledger-core (or in whichever service calls
// RecordAgentDecision/ApplyEntry) must not be able to also quietly
// rewrite what already landed here, because this process is the only
// thing that ever writes to this file, and it only ever appends.
//
// Scale note: the in-memory index below (recordsByTenant, a plain
// slice) is rebuilt from the append file on every startup and is not
// going to hold up at real audit-log volumes (every ApplyEntry and
// RecordAgentDecision call, system-wide, forever). At that point the
// fix is the same one ledger-core already took for its own WAL: keep
// this file as the durability/tamper-evidence source of truth, and add
// a real queryable index (SQLite, or a managed append-only store like
// S3 Object Lock + a Postgres index) behind ListRecords/VerifyChain
// without changing this package's on-disk format or its guarantees.
package store

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
)

// SourceKind mirrors ration.audit.v1.SourceKind without importing the
// generated pb package, so this package has zero proto/grpc
// dependencies and can be unit tested (and reused, in principle) on its
// own.
type SourceKind int32

const (
	SourceKindUnspecified SourceKind = 0
	SourceKindLedgerEntry SourceKind = 1
	SourceKindAgentDecision SourceKind = 2
)

// Record is one entry in the chain, exactly as persisted (field order
// here is also the field order in the canonical hash input — see hashOf
// — so do not reorder these without treating it as a chain format
// change).
type Record struct {
	Sequence       int64      `json:"sequence"`
	SourceService  string     `json:"source_service"`
	Kind           SourceKind `json:"kind"`
	TenantID       string     `json:"tenant_id"`
	RefID          string     `json:"ref_id"`
	PayloadJSON    string     `json:"payload_json"`
	OccurredAtUnix int64      `json:"occurred_at_unix"`
	RecordedAtUnix int64      `json:"recorded_at_unix"`
	PrevHash       string     `json:"prev_hash"`
	Hash           string     `json:"hash"`
}

// AppendInput is everything a caller supplies; Store fills in
// Sequence, RecordedAtUnix, PrevHash and Hash.
type AppendInput struct {
	SourceService  string
	Kind           SourceKind
	TenantID       string
	RefID          string
	PayloadJSON    string
	OccurredAtUnix int64
}

// idempotencyKey identifies a logical record for Append's
// re-append-returns-the-original behavior (see proto/audit.proto's
// doc comment on AuditLog.Append).
func idempotencyKey(sourceService string, kind SourceKind, refID string) string {
	return sourceService + "|" + strconv.Itoa(int(kind)) + "|" + refID
}

// Store is an append-only, hash-chained log backed by a single file.
// Safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	file *os.File

	records     []Record       // in-memory index, oldest first (see package doc's scale note)
	byIdempKey  map[string]int // idempotencyKey -> index into records
	lastHash    string
	nextSeq     int64
}

// Open opens (creating if necessary) the append-only file at path and
// replays it to rebuild the in-memory index, the idempotency map and
// the running hash chain. Replay fails loudly (returns an error) if the
// file is corrupt or the chain doesn't verify — audit-log would rather
// refuse to start than silently serve a broken trail.
func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	s := &Store{
		file:       f,
		byIdempKey: make(map[string]int),
	}

	if err := s.replay(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying file handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
}

func (s *Store) replay() error {
	if _, err := s.file.Seek(0, 0); err != nil {
		return fmt.Errorf("store: seek to start for replay: %w", err)
	}
	scanner := bufio.NewScanner(s.file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return fmt.Errorf("store: replay: corrupt record at line %d: %w", lineNo, err)
		}
		wantHash := hashOf(rec)
		if wantHash != rec.Hash {
			return fmt.Errorf("store: replay: chain broken at sequence %d: stored hash %q, recomputed %q", rec.Sequence, rec.Hash, wantHash)
		}
		if rec.PrevHash != s.lastHash {
			return fmt.Errorf("store: replay: chain broken at sequence %d: prev_hash %q does not match previous record's hash %q", rec.Sequence, rec.PrevHash, s.lastHash)
		}
		if rec.Sequence != s.nextSeq+1 {
			return fmt.Errorf("store: replay: non-contiguous sequence at line %d: got %d, want %d", lineNo, rec.Sequence, s.nextSeq+1)
		}

		s.records = append(s.records, rec)
		s.byIdempKey[idempotencyKey(rec.SourceService, rec.Kind, rec.RefID)] = len(s.records) - 1
		s.lastHash = rec.Hash
		s.nextSeq = rec.Sequence
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("store: replay: reading %s: %w", s.file.Name(), err)
	}
	if _, err := s.file.Seek(0, 2); err != nil {
		return fmt.Errorf("store: seek to end after replay: %w", err)
	}
	return nil
}

// hashOf computes a record's chain hash from every field except Hash
// itself, in a fixed field order (see the Record doc comment).
func hashOf(rec Record) string {
	canonical := fmt.Sprintf("%d|%s|%d|%s|%s|%s|%d|%d|%s",
		rec.Sequence, rec.SourceService, rec.Kind, rec.TenantID, rec.RefID,
		rec.PayloadJSON, rec.OccurredAtUnix, rec.RecordedAtUnix, rec.PrevHash)
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// Append writes one record, or — if (source_service, kind, ref_id)
// matches a record already appended — returns that original record
// unchanged. recordedAtUnix is passed in by the caller (rather than
// read from time.Now() here) so tests can control it deterministically;
// production callers pass time.Now().Unix().
func (s *Store) Append(in AppendInput, recordedAtUnix int64) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := idempotencyKey(in.SourceService, in.Kind, in.RefID)
	if idx, ok := s.byIdempKey[key]; ok {
		return s.records[idx], nil
	}

	rec := Record{
		Sequence:       s.nextSeq + 1,
		SourceService:  in.SourceService,
		Kind:           in.Kind,
		TenantID:       in.TenantID,
		RefID:          in.RefID,
		PayloadJSON:    in.PayloadJSON,
		OccurredAtUnix: in.OccurredAtUnix,
		RecordedAtUnix: recordedAtUnix,
		PrevHash:       s.lastHash,
	}
	rec.Hash = hashOf(rec)

	line, err := json.Marshal(rec)
	if err != nil {
		return Record{}, fmt.Errorf("store: marshal record: %w", err)
	}
	line = append(line, '\n')
	if _, err := s.file.Write(line); err != nil {
		return Record{}, fmt.Errorf("store: append to %s: %w", s.file.Name(), err)
	}
	if err := s.file.Sync(); err != nil {
		return Record{}, fmt.Errorf("store: fsync %s: %w", s.file.Name(), err)
	}

	s.records = append(s.records, rec)
	s.byIdempKey[key] = len(s.records) - 1
	s.lastHash = rec.Hash
	s.nextSeq = rec.Sequence
	return rec, nil
}

// List returns up to pageSize records for tenantID (all tenants if
// empty) starting after afterSeq, oldest first, plus the sequence to
// resume from on the next call (0 when there is nothing more).
func (s *Store) List(tenantID string, afterSeq int64, pageSize int) ([]Record, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if pageSize <= 0 {
		pageSize = 100
	}
	// Fetch one more than requested so we can tell whether there's a
	// next page without a second pass over s.records.
	var matched []Record
	for _, rec := range s.records {
		if rec.Sequence <= afterSeq {
			continue
		}
		if tenantID != "" && rec.TenantID != tenantID {
			continue
		}
		matched = append(matched, rec)
		if len(matched) == pageSize+1 {
			break
		}
	}
	if len(matched) > pageSize {
		return matched[:pageSize], matched[pageSize-1].Sequence
	}
	return matched, 0
}

// VerifyChainResult is VerifyChain's outcome.
type VerifyChainResult struct {
	Intact              bool
	RecordsChecked      int64
	FirstBrokenSequence int64
	Detail              string
}

// VerifyChain re-derives the hash chain from the in-memory index (which
// was itself only ever populated by successful replay or Append, both
// of which already checked each record as they went — see replay and
// Append) and reports the first mismatch, if any. Exposed as its own
// RPC (rather than only checked implicitly) so an operator or an
// incident-review script can ask "is the trail still intact?" without
// restarting the process.
func (s *Store) VerifyChain() VerifyChainResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	prevHash := ""
	for _, rec := range s.records {
		if rec.PrevHash != prevHash {
			return VerifyChainResult{
				Intact:              false,
				RecordsChecked:      rec.Sequence - 1,
				FirstBrokenSequence: rec.Sequence,
				Detail:              fmt.Sprintf("sequence %d: prev_hash %q does not match preceding record's hash %q", rec.Sequence, rec.PrevHash, prevHash),
			}
		}
		if hashOf(rec) != rec.Hash {
			return VerifyChainResult{
				Intact:              false,
				RecordsChecked:      rec.Sequence - 1,
				FirstBrokenSequence: rec.Sequence,
				Detail:              fmt.Sprintf("sequence %d: stored hash does not match recomputed hash", rec.Sequence),
			}
		}
		prevHash = rec.Hash
	}
	return VerifyChainResult{Intact: true, RecordsChecked: int64(len(s.records))}
}

// VerifyFile re-derives the hash chain directly from the append-only
// file at path, independently of any running Store/audit-log process.
// This is the "independent verifier" docs/THREAT-MODEL.md's "Known
// gaps" section 9 asks for: VerifyChain (above) is only trustworthy if
// the audit-log binary serving that RPC is itself uncompromised — a
// compromised audit-log could simply lie about the result. VerifyFile
// opens path read-only (O_RDONLY, never O_RDWR — this must never
// require write access or risk mutating the file it's auditing) and
// performs the exact same per-record checks replay() does, but reports
// every failure as a VerifyChainResult (Intact=false, Detail
// populated) rather than a Go error, so a caller (see
// cmd/verify-chain) can always print a verdict instead of only "it
// crashed." The one exception is a file the OS itself refuses to open
// or read — that's reported as the returned error, since there is no
// chain to report a verdict about at all in that case.
func VerifyFile(path string) (VerifyChainResult, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return VerifyChainResult{}, fmt.Errorf("store: open %s read-only: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	prevHash := ""
	wantSeq := int64(1)
	var checked int64
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return VerifyChainResult{
				Intact:              false,
				RecordsChecked:      checked,
				FirstBrokenSequence: wantSeq,
				Detail:              fmt.Sprintf("line %d: corrupt record: %v", lineNo, err),
			}, nil
		}
		if rec.PrevHash != prevHash {
			return VerifyChainResult{
				Intact:              false,
				RecordsChecked:      checked,
				FirstBrokenSequence: rec.Sequence,
				Detail:              fmt.Sprintf("sequence %d: prev_hash %q does not match preceding record's hash %q", rec.Sequence, rec.PrevHash, prevHash),
			}, nil
		}
		if rec.Sequence != wantSeq {
			return VerifyChainResult{
				Intact:              false,
				RecordsChecked:      checked,
				FirstBrokenSequence: rec.Sequence,
				Detail:              fmt.Sprintf("line %d: non-contiguous sequence: got %d, want %d", lineNo, rec.Sequence, wantSeq),
			}, nil
		}
		if hashOf(rec) != rec.Hash {
			return VerifyChainResult{
				Intact:              false,
				RecordsChecked:      checked,
				FirstBrokenSequence: rec.Sequence,
				Detail:              fmt.Sprintf("sequence %d: stored hash does not match recomputed hash", rec.Sequence),
			}, nil
		}
		prevHash = rec.Hash
		wantSeq = rec.Sequence + 1
		checked++
	}
	if err := scanner.Err(); err != nil {
		return VerifyChainResult{}, fmt.Errorf("store: reading %s: %w", path, err)
	}
	return VerifyChainResult{Intact: true, RecordsChecked: checked}, nil
}
