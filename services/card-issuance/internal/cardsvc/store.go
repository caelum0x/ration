package cardsvc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	"google.golang.org/protobuf/encoding/protojson"
)

// persistedCards is the on-disk shape of card-issuance's card store: a
// single JSON file (no WAL, no SQLite — card-issuance has neither,
// unlike ledger-core) holding every card this process has ever issued,
// keyed implicitly by each Card's own Id field. protojson (not plain
// encoding/json) is used per-card so proto field names/enum strings
// round-trip the same way they do over the wire, and so a future
// proto field addition doesn't silently get dropped by a hand-rolled
// struct tag.
type persistedCards struct {
	Cards []json.RawMessage `json:"cards"`
}

// loadCardsFile reads path and decodes it into a map keyed by Card.Id.
// A missing file is not an error (first run on a fresh data dir) and
// returns an empty map.
func loadCardsFile(path string) (map[string]*cardissuancepb.Card, error) {
	out := make(map[string]*cardissuancepb.Card)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("cardsvc: read %s: %w", path, err)
	}

	var persisted persistedCards
	if err := json.Unmarshal(data, &persisted); err != nil {
		return nil, fmt.Errorf("cardsvc: decode %s: %w", path, err)
	}

	for i, raw := range persisted.Cards {
		card := &cardissuancepb.Card{}
		if err := protojson.Unmarshal(raw, card); err != nil {
			return nil, fmt.Errorf("cardsvc: decode card %d in %s: %w", i, path, err)
		}
		if card.GetId() == "" {
			return nil, fmt.Errorf("cardsvc: card %d in %s has an empty id", i, path)
		}
		out[card.GetId()] = card
	}
	return out, nil
}

// saveCardsFile atomically writes cards to path: encode everything into
// a temp file in the same directory, fsync it, then rename over the
// real path. A crash or kill mid-write leaves the previous, still-valid
// file in place rather than a truncated or corrupt one — the same
// write-then-fsync-then-rename discipline services/ledger-core's WAL
// uses for its own durability, scaled down to this service's much
// simpler single-file, single-writer needs (card-issuance has no
// concurrent-writer story beyond its own in-process mutex, which the
// caller already holds for the snapshot it hands this function).
func saveCardsFile(path string, cards map[string]*cardissuancepb.Card) error {
	ids := make([]string, 0, len(cards))
	for id := range cards {
		ids = append(ids, id)
	}
	sort.Strings(ids) // stable, diffable file across saves

	persisted := persistedCards{Cards: make([]json.RawMessage, 0, len(ids))}
	for _, id := range ids {
		raw, err := protojson.Marshal(cards[id])
		if err != nil {
			return fmt.Errorf("cardsvc: encode card %s: %w", id, err)
		}
		persisted.Cards = append(persisted.Cards, raw)
	}

	data, err := json.MarshalIndent(&persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("cardsvc: encode %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cardsvc: mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".cards-*.json.tmp")
	if err != nil {
		return fmt.Errorf("cardsvc: create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("cardsvc: write %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("cardsvc: fsync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cardsvc: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("cardsvc: rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}
