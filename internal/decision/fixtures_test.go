package decision_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/cserr"
	"github.com/ClearedStack/chartroom/internal/decision"
	"github.com/ClearedStack/chartroom/internal/fixture"
)

// TestFixtures runs every saved example under testdata/apply: a catalog
// and a decision in, and either the exact change and catalog out or the
// exact error line.
func TestFixtures(t *testing.T) {
	for _, dir := range fixture.Dirs(t, "apply") {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			catalogData := fixture.Read(t, filepath.Join(dir, "catalog.json"))
			itemData := fixture.Read(t, filepath.Join(dir, "decision.json"))
			change, after, err := decision.Run(catalogData, catalog.MaxBytes, itemData)

			wantChange := filepath.Join(dir, "want.change.json")
			wantCatalog := filepath.Join(dir, "want.catalog.json")
			wantError := filepath.Join(dir, "want.error.txt")

			if err != nil {
				e, ok := cserr.From(err)
				if !ok {
					t.Fatalf("error is not a public error: %v", err)
				}
				wantCode := map[string]cserr.Code{"parse": cserr.Parse, "subtool": cserr.NeedsSubtool}[strings.SplitN(name, "-", 2)[0]]
				if e.Reason.Code != wantCode {
					t.Errorf("code = %s, but the fixture name says %s", e.Reason.Code, wantCode)
				}
				if !strings.Contains(name, strings.ReplaceAll(e.Reason.Name, "_", "-")) {
					t.Errorf("reason = %s, which the fixture name does not say", e.Reason.Name)
				}
				fixture.Golden(t, wantError, []byte(e.Error()+"\n"))
				fixture.Absent(t, wantChange)
				fixture.Absent(t, wantCatalog)
				return
			}
			if !strings.HasPrefix(name, "ok-") {
				t.Errorf("no error, but the fixture name says there should be one")
			}
			if strings.HasSuffix(name, "-unchanged") == change.Changed {
				t.Errorf("changed = %v, which the fixture name contradicts", change.Changed)
			}
			fixture.Golden(t, wantChange, change.Encode())
			fixture.Golden(t, wantCatalog, after.Encode())
			fixture.Absent(t, wantError)

			// The same input gives the same output, every time.
			for i := 0; i < 3; i++ {
				again, afterAgain, err := decision.Run(catalogData, catalog.MaxBytes, itemData)
				if err != nil || !bytes.Equal(again.Encode(), change.Encode()) || !bytes.Equal(afterAgain.Encode(), after.Encode()) {
					t.Fatalf("run %d gave a different result", i+2)
				}
			}
			// Applying the result again is a no-op.
			repeat, afterRepeat, err := decision.Run(after.Encode(), catalog.MaxBytes, itemData)
			if err != nil {
				t.Fatalf("applying a second time: %v", err)
			}
			if repeat.Changed || !bytes.Equal(afterRepeat.Encode(), after.Encode()) {
				t.Errorf("applying a second time changed the catalog")
			}
		})
	}
}

// TestEveryReasonHasASavedExample walks all of testdata and checks that
// each reason Chartroom can give appears in at least one expected output.
func TestEveryReasonHasASavedExample(t *testing.T) {
	if fixture.Updating() {
		t.Skip("updating golden files")
	}
	var all bytes.Buffer
	err := filepath.WalkDir(fixture.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "want.") {
			return err
		}
		data, err := os.ReadFile(path)
		all.Write(data)
		all.WriteByte('\n')
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range cserr.Reasons() {
		line := string(r.Code) + " " + r.Name
		pair := `"code": "` + string(r.Code) + `",` // then the reason, in a record or a log line
		if !bytes.Contains(all.Bytes(), []byte(line)) &&
			!(bytes.Contains(all.Bytes(), []byte(pair)) && bytes.Contains(all.Bytes(), []byte(`"reason": "`+r.Name+`"`))) &&
			!bytes.Contains(all.Bytes(), []byte(`"code":"`+string(r.Code)+`","reason":"`+r.Name+`"`)) {
			t.Errorf("no saved example shows %s", line)
		}
	}
}

// TestEachActionHasAGoldenFixture checks the four rules each have a saved
// example named after them.
func TestEachActionHasAGoldenFixture(t *testing.T) {
	for action := range decision.Transitions {
		if !fixture.Exists(filepath.Join(fixture.Root, "apply", "ok-"+action, "decision.json")) {
			t.Errorf("no ok-%s fixture", action)
		}
	}
}
