package compaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// LedgerStatus represents a constraint lifecycle state in L1 ContextLedger.
type LedgerStatus string

const (
	StatusActive     LedgerStatus = "active"
	StatusSuperseded LedgerStatus = "superseded"
	StatusResolved   LedgerStatus = "resolved"
)

// Constraint represents a typed invariant or architectural decision in L1 ContextLedger.
type Constraint struct {
	ID           string       `json:"id"`
	Category     string       `json:"category"`
	Statement    string       `json:"statement"`
	Status       LedgerStatus `json:"status"`
	Supersedes   []string     `json:"supersedes,omitempty"`
	SupersededBy string       `json:"superseded_by,omitempty"`
	Provenance   []string     `json:"provenance"`
	Turn         int          `json:"turn,omitempty"`
}

// LedgerEvent describes an incremental state transition emitted by LLM structured compaction.
type LedgerEvent struct {
	Action       string   `json:"action"` // add | supersede | resolve
	ID           string   `json:"id"`
	Category     string   `json:"category,omitempty"`
	Statement    string   `json:"statement,omitempty"`
	SupersedesID string   `json:"supersedes_id,omitempty"`
	Provenance   []string `json:"provenance,omitempty"`
	Turn         int      `json:"turn,omitempty"`
}

// LedgerDiff records an applied mutation on ContextLedger for audit logging.
type LedgerDiff struct {
	Action       string   `json:"action"`
	ID           string   `json:"id"`
	Category     string   `json:"category"`
	Statement    string   `json:"statement,omitempty"`
	SupersedesID string   `json:"supersedes_id,omitempty"`
	Provenance   []string `json:"provenance,omitempty"`
	Turn         int      `json:"turn,omitempty"`
}

// ContextLedger maintains structured low-frequency invariants with provenance pointers.
type ContextLedger struct {
	Version     int                    `json:"version"`
	LastEpoch   *int                   `json:"last_epoch,omitempty"`
	Constraints map[string]*Constraint `json:"constraints"`
	Order       []string               `json:"order,omitempty"`
}

// NewContextLedger initializes an empty structured constraint ledger.
func NewContextLedger() *ContextLedger {
	return &ContextLedger{
		Constraints: make(map[string]*Constraint),
	}
}

// Clone returns a deep copy of the ContextLedger.
func (l *ContextLedger) Clone() *ContextLedger {
	if l == nil {
		return NewContextLedger()
	}
	var lastEp *int
	if l.LastEpoch != nil {
		ep := *l.LastEpoch
		lastEp = &ep
	}
	cp := &ContextLedger{
		Version:     l.Version,
		LastEpoch:   lastEp,
		Constraints: make(map[string]*Constraint, len(l.Constraints)),
		Order:       slices.Clone(l.Order),
	}
	for id, c := range l.Constraints {
		if c == nil {
			continue
		}
		cc := *c
		cc.Supersedes = slices.Clone(c.Supersedes)
		cc.Provenance = slices.Clone(c.Provenance)
		cp.Constraints[id] = &cc
	}
	return cp
}

// CanUpdateInEpoch reports whether the ledger has not yet been updated in epochID.
func (l *ContextLedger) CanUpdateInEpoch(epochID int) bool {
	if l == nil || l.LastEpoch == nil {
		return true
	}
	return epochID > *l.LastEpoch
}

// MarkEpochUpdated records the Epoch ID of the latest ContextLedger mutation.
func (l *ContextLedger) MarkEpochUpdated(epochID int) {
	if l == nil {
		return
	}
	ep := epochID
	l.LastEpoch = &ep
}

// UnmarshalContextLedger restores a persisted ContextLedger from JSON bytes.
func UnmarshalContextLedger(data []byte) (*ContextLedger, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return NewContextLedger(), nil
	}
	var l ContextLedger
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, err
	}
	if l.Constraints == nil {
		l.Constraints = make(map[string]*Constraint)
	}
	return &l, nil
}

// Apply adds, supersedes, or resolves a constraint and tracks provenance entry IDs.
func (l *ContextLedger) Apply(action, id, category, statement, supersedesID string, provenance []string, turn int) {
	_, _ = l.ApplyWithDiff(action, id, category, statement, supersedesID, provenance, turn)
}

// ApplyWithDiff applies a single state transition and returns the resulting audit diff when applied.
func (l *ContextLedger) ApplyWithDiff(action, id, category, statement, supersedesID string, provenance []string, turn int) (*LedgerDiff, bool) {
	if l == nil {
		return nil, false
	}
	if l.Constraints == nil {
		l.Constraints = make(map[string]*Constraint)
	}
	id = strings.TrimSpace(id)
	statement = strings.TrimSpace(statement)
	category = strings.TrimSpace(category)
	supersedesID = strings.TrimSpace(supersedesID)
	act := strings.ToLower(strings.TrimSpace(action))

	if id == "" {
		return nil, false
	}
	if category == "" {
		category = "general"
	}

	switch act {
	case "resolve":
		c, ok := l.Constraints[id]
		if !ok {
			return nil, false
		}
		l.Version++
		c.Status = StatusResolved
		if statement != "" {
			c.Statement = statement
		}
		c.Provenance = mergeUniqueStrs(c.Provenance, provenance)
		if turn > c.Turn {
			c.Turn = turn
		}
		return &LedgerDiff{
			Action:     "resolve",
			ID:         id,
			Category:   c.Category,
			Statement:  c.Statement,
			Provenance: slices.Clone(c.Provenance),
			Turn:       c.Turn,
		}, true

	case "supersede":
		if statement == "" {
			return nil, false
		}
		l.Version++
		if supersedesID != "" {
			if old, ok := l.Constraints[supersedesID]; ok {
				old.Status = StatusSuperseded
				old.SupersededBy = id
			}
		}
		c := &Constraint{
			ID:         id,
			Category:   category,
			Statement:  statement,
			Status:     StatusActive,
			Provenance: mergeUniqueStrs(nil, provenance),
			Turn:       turn,
		}
		if supersedesID != "" {
			c.Supersedes = []string{supersedesID}
		}
		if _, exists := l.Constraints[id]; !exists {
			l.Order = append(l.Order, id)
		}
		l.Constraints[id] = c
		return &LedgerDiff{
			Action:       "supersede",
			ID:           id,
			Category:     category,
			Statement:    statement,
			SupersedesID: supersedesID,
			Provenance:   slices.Clone(c.Provenance),
			Turn:         turn,
		}, true

	default: // "add"
		if statement == "" {
			return nil, false
		}
		l.Version++
		if existing, exists := l.Constraints[id]; exists {
			existing.Statement = statement
			if category != "general" || existing.Category == "" {
				existing.Category = category
			}
			existing.Status = StatusActive
			existing.SupersededBy = ""
			existing.Provenance = mergeUniqueStrs(existing.Provenance, provenance)
			if turn > existing.Turn {
				existing.Turn = turn
			}
			return &LedgerDiff{
				Action:     "add",
				ID:         id,
				Category:   existing.Category,
				Statement:  existing.Statement,
				Provenance: slices.Clone(existing.Provenance),
				Turn:       existing.Turn,
			}, true
		}
		l.Order = append(l.Order, id)
		c := &Constraint{
			ID:         id,
			Category:   category,
			Statement:  statement,
			Status:     StatusActive,
			Provenance: mergeUniqueStrs(nil, provenance),
			Turn:       turn,
		}
		l.Constraints[id] = c
		return &LedgerDiff{
			Action:     "add",
			ID:         id,
			Category:   category,
			Statement:  statement,
			Provenance: slices.Clone(c.Provenance),
			Turn:       turn,
		}, true
	}
}

// ApplyEvents applies a batch of structured ledger events in order.
func (l *ContextLedger) ApplyEvents(events []LedgerEvent) {
	_ = l.ApplyEventsWithDiff(events)
}

// ApplyEventsWithDiff applies a batch of structured ledger events and returns all applied audit diffs.
func (l *ContextLedger) ApplyEventsWithDiff(events []LedgerEvent) []LedgerDiff {
	var diffs []LedgerDiff
	for _, ev := range events {
		if d, ok := l.ApplyWithDiff(ev.Action, ev.ID, ev.Category, ev.Statement, ev.SupersedesID, ev.Provenance, ev.Turn); ok && d != nil {
			diffs = append(diffs, *d)
		}
	}
	return diffs
}

// Stats returns ledger version, active count, superseded count, and the first 8 hex chars of
// SHA256(CanonicalRender()) for prompt-cache stability tracking.
func (l *ContextLedger) Stats() (version, activeCnt, supersededCnt int, hash8 string) {
	if l == nil {
		sum := sha256.Sum256(nil)
		return 0, 0, 0, hex.EncodeToString(sum[:4])
	}
	version = l.Version
	for _, c := range l.Constraints {
		if c == nil {
			continue
		}
		switch c.Status {
		case StatusActive:
			activeCnt++
		case StatusSuperseded:
			supersededCnt++
		}
	}
	sum := sha256.Sum256([]byte(l.CanonicalRender()))
	hash8 = hex.EncodeToString(sum[:4])
	return version, activeCnt, supersededCnt, hash8
}

// SortedByStatus returns constraints filtered by status and sorted deterministically by ID.
func (l *ContextLedger) SortedByStatus(status LedgerStatus) []*Constraint {
	if l == nil {
		return nil
	}
	ids := make([]string, 0, len(l.Constraints))
	for id, c := range l.Constraints {
		if c != nil && c.Status == status {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]*Constraint, 0, len(ids))
	for _, id := range ids {
		cc := *l.Constraints[id]
		cc.Provenance = slices.Clone(cc.Provenance)
		cc.Supersedes = slices.Clone(cc.Supersedes)
		out = append(out, &cc)
	}
	return out
}

// ActiveProvenanceEntryIDs returns entry IDs backing currently active constraints.
func (l *ContextLedger) ActiveProvenanceEntryIDs() []string {
	if l == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, c := range l.sortedActive() {
		for _, eid := range c.Provenance {
			if _, ok := seen[eid]; !ok {
				seen[eid] = struct{}{}
				out = append(out, eid)
			}
		}
	}
	return out
}

// SupersededEntryIDs returns entry IDs that belong exclusively to superseded constraints.
func (l *ContextLedger) SupersededEntryIDs() map[string]struct{} {
	sup := make(map[string]struct{})
	if l == nil {
		return sup
	}
	active := make(map[string]struct{})
	for _, eid := range l.ActiveProvenanceEntryIDs() {
		active[eid] = struct{}{}
	}
	for _, c := range l.Constraints {
		if c != nil && c.Status == StatusSuperseded {
			for _, eid := range c.Provenance {
				if _, isActive := active[eid]; !isActive {
					sup[eid] = struct{}{}
				}
			}
		}
	}
	return sup
}

// CanonicalRender produces a deterministic, byte-stable representation sorted by constraint ID
// so unchanged ledgers preserve Prompt Caching prefixes.
func (l *ContextLedger) CanonicalRender() string {
	if l == nil {
		return ""
	}
	active := l.sortedActive()
	if len(active) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[CONTEXT_LEDGER]\n")
	for _, c := range active {
		b.WriteString(fmt.Sprintf("- %s (%s): %s", c.ID, c.Category, c.Statement))
		if len(c.Provenance) > 0 {
			b.WriteString(fmt.Sprintf(" [provenance:%s]", strings.Join(c.Provenance, ",")))
		}
		if len(c.Supersedes) > 0 {
			b.WriteString(fmt.Sprintf(" (supersedes:%s)", strings.Join(c.Supersedes, ",")))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func (l *ContextLedger) sortedActive() []*Constraint {
	ids := make([]string, 0, len(l.Constraints))
	for id, c := range l.Constraints {
		if c != nil && c.Status == StatusActive {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]*Constraint, 0, len(ids))
	for _, id := range ids {
		out = append(out, l.Constraints[id])
	}
	return out
}

func mergeUniqueStrs(base, extra []string) []string {
	out := make([]string, 0, len(base)+len(extra))
	seen := make(map[string]struct{}, len(base)+len(extra))
	for _, v := range base {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	for _, v := range extra {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}
