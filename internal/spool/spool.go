// Package spool stages results on disk until Muster has acknowledged them.
//
// The spool directory is the only place Chartroom writes. A result is
// written to a temporary file, synced and renamed into place, so a file in
// the spool is either complete or absent. Results can always be worked out
// again from the item, so a file that cannot be read back is removed and
// counted, never guessed at.
package spool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/muster"
	"github.com/ClearedStack/chartroom/internal/strictjson"
)

// DefaultDir is the spool directory the specification names.
const DefaultDir = "/var/spool/cs"

// MaxRecordBytes is the largest staged file the spool will read back.
const MaxRecordBytes = 1 << 20

// The three results a record can hold. They are three of Muster's calls.
const (
	OutcomeComplete     = "complete"
	OutcomeFail         = "fail"
	OutcomeNeedsSubtool = "needs_subtool"
)

// ErrUnusable means the spool directory cannot be used. The path is never
// put in the error.
var ErrUnusable = errors.New("the spool directory is not usable")

// Record is one staged result, waiting for Muster's acknowledgement.
//
// A complete record holds what the complete call sends: the state the item
// is now in and the result, which is the catalog entry. A fail or
// needs_subtool record holds the public error code, and the reason and
// field for Chartroom's own log; Muster is sent the code and no more.
type Record struct {
	Contract string          `json:"contract"`
	Lease    string          `json:"lease"`
	ID       string          `json:"id,omitempty"`
	Lane     string          `json:"lane"`
	Outcome  string          `json:"outcome"`
	State    string          `json:"state,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Code     string          `json:"code,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Field    string          `json:"field,omitempty"`
}

// Encode writes the record in canonical form.
func (r *Record) Encode() []byte { return strictjson.Marshal(r) }

func (r *Record) valid() bool {
	if r.Contract != catalog.ContractVersion || r.Lease == "" {
		return false
	}
	switch r.Outcome {
	case OutcomeComplete:
		return r.State != "" && len(r.Result) > 0 && r.Code == ""
	case OutcomeFail:
		return len(r.Result) == 0 && (r.Code == "E_PARSE" || r.Code == "E_TIMEOUT") && r.Reason != ""
	case OutcomeNeedsSubtool:
		return len(r.Result) == 0 && r.Code == "E_NEEDS_SUBTOOL" && r.Reason != ""
	}
	return false
}

type staged struct {
	rec Record
	seq int
}

// Spool is the set of staged records, on disk and mirrored in memory.
type Spool struct {
	dir     string
	seq     int
	records map[string]*staged
}

// Open reads the spool directory. Left-over temporary files are removed.
// discarded is the number of staged files that could not be read back and
// were removed.
func Open(dir string) (s *Spool, discarded int, err error) {
	names, err := readNames(dir)
	if err != nil {
		return nil, 0, ErrUnusable
	}
	s = &Spool{dir: dir, records: make(map[string]*staged)}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if strings.HasSuffix(name, tmpSuffix) {
			_ = os.Remove(path)
			continue
		}
		if !strings.HasSuffix(name, recSuffix) {
			continue
		}
		rec, ok := readRecord(path)
		if !ok || fileName(rec.Lease) != name {
			_ = os.Remove(path)
			discarded++
			continue
		}
		s.seq++
		s.records[rec.Lease] = &staged{rec: rec, seq: s.seq}
	}
	return s, discarded, nil
}

const (
	recSuffix = ".json"
	tmpSuffix = ".tmp"
)

func readNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func readRecord(path string) (Record, bool) {
	var rec Record
	info, err := os.Stat(path)
	if err != nil || info.Size() > MaxRecordBytes {
		return rec, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return rec, false
	}
	if strictjson.Decode(data, MaxRecordBytes, &rec) != nil || !rec.valid() {
		return rec, false
	}
	rec.Result = Compact(rec.Result)
	return rec, true
}

// Compact strips insignificant white space from raw JSON, so a result is
// the same bytes whether it was just worked out or read back from disk.
func Compact(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

// fileName gives the file a lease's record lives in. The lease identifier
// comes from outside, so it is hashed and never used as a path itself.
func fileName(lease string) string {
	sum := sha256.Sum256([]byte(lease))
	return hex.EncodeToString(sum[:16]) + recSuffix
}

// Put stages a record, replacing any record for the same lease. It returns
// once the record is safely on disk.
func (s *Spool) Put(rec Record) error {
	if !rec.valid() {
		return ErrUnusable
	}
	rec.Result = Compact(rec.Result)
	final := filepath.Join(s.dir, fileName(rec.Lease))
	tmp := final + tmpSuffix
	if err := writeSynced(tmp, rec.Encode()); err != nil {
		_ = os.Remove(tmp)
		return ErrUnusable
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return ErrUnusable
	}
	syncDir(s.dir)
	if old, ok := s.records[rec.Lease]; ok {
		old.rec = rec
		return nil
	}
	s.seq++
	s.records[rec.Lease] = &staged{rec: rec, seq: s.seq}
	return nil
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// Remove drops the record for a lease, once Muster has acknowledged it or
// no longer wants it.
func (s *Spool) Remove(lease string) error {
	if _, ok := s.records[lease]; !ok {
		return nil
	}
	if err := os.Remove(filepath.Join(s.dir, fileName(lease))); err != nil && !os.IsNotExist(err) {
		return ErrUnusable
	}
	syncDir(s.dir)
	delete(s.records, lease)
	return nil
}

// Len is the number of staged records.
func (s *Spool) Len() int { return len(s.records) }

// Pending returns the staged records in the order they should be sent:
// urgent, then normal, then backfill, and oldest first within each.
func (s *Spool) Pending() []Record {
	list := make([]*staged, 0, len(s.records))
	for _, st := range s.records {
		list = append(list, st)
	}
	sort.Slice(list, func(i, j int) bool {
		ri, rj := muster.LaneRank(list[i].rec.Lane), muster.LaneRank(list[j].rec.Lane)
		if ri != rj {
			return ri < rj
		}
		return list[i].seq < list[j].seq
	})
	out := make([]Record, len(list))
	for i, st := range list {
		out[i] = st.rec
	}
	return out
}
