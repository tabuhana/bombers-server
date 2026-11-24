package patchnotes

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tabuhana/bombers-server/internal/admin"
	"github.com/tabuhana/bombers-server/internal/auth"
)

func TestParseVersion(t *testing.T) {
	ok := []struct {
		in         string
		ma, mi, pa int
	}{
		{"0.0.0", 0, 0, 0},
		{"0.1.30", 0, 1, 30},
		{"1.2.3", 1, 2, 3},
		{"1000000.0.0", 1000000, 0, 0},
	}
	for _, c := range ok {
		ma, mi, pa, good := parseVersion(c.in)
		if !good || ma != c.ma || mi != c.mi || pa != c.pa {
			t.Errorf("parseVersion(%q) = %d.%d.%d %v", c.in, ma, mi, pa, good)
		}
	}

	bad := []string{
		"", "1", "1.2", "1.2.3.4", "01.2.3", "1.02.3", "-1.2.3", "+1.2.3",
		"1.2.3-beta", "1.2.3+b", "1..3", " 1.2.3", "1.2.3 ", "1000001.0.0",
		"a.b.c", "1.2.03", "1.2.", ".1.2",
		strconv.FormatUint(math.MaxUint64, 10) + ".0.0",
		"99999999999999999999999.0.0",
	}
	for _, in := range bad {
		if _, _, _, good := parseVersion(in); good {
			t.Errorf("parseVersion(%q) accepted, want refused", in)
		}
	}
}

func TestParseNoteAcceptsAndTrims(t *testing.T) {
	parts, err := parseNote([]byte(`{"parts":[{"title":"  Hi  ","body":"\n text \t"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Title != "Hi" || parts[0].Body != "text" {
		t.Errorf("parts = %+v", parts)
	}

	// 200 multi-byte runes is within the limit: it is characters, not bytes.
	long := strings.Repeat("é", 200)
	if _, err := parseNote([]byte(`{"parts":[{"title":"` + long + `","body":"b"}]}`)); err != nil {
		t.Errorf("200-rune multi-byte title refused: %v", err)
	}
	// Exactly at the other limits.
	if _, err := parseNote([]byte(`{"parts":[{"title":"t","body":"` + strings.Repeat("b", 4000) + `"}]}`)); err != nil {
		t.Errorf("4000-rune body refused: %v", err)
	}
	many := strings.Repeat(`{"title":"t","body":"b"},`, 19) + `{"title":"t","body":"b"}`
	if _, err := parseNote([]byte(`{"parts":[` + many + `]}`)); err != nil {
		t.Errorf("20 parts refused: %v", err)
	}
}

func TestParseNoteRefusals(t *testing.T) {
	one := `{"title":"t","body":"b"}`
	tests := []struct {
		name string
		raw  []byte
	}{
		{"zero parts", []byte(`{"parts":[]}`)},
		{"21 parts", []byte(`{"parts":[` + strings.Repeat(one+",", 20) + one + `]}`)},
		{"blank title", []byte(`{"parts":[{"title":"","body":"b"}]}`)},
		{"whitespace title", []byte(`{"parts":[{"title":" \t ","body":"b"}]}`)},
		{"blank body", []byte(`{"parts":[{"title":"t","body":""}]}`)},
		{"whitespace body", []byte(`{"parts":[{"title":"t","body":"  \n"}]}`)},
		{"201-rune title", []byte(`{"parts":[{"title":"` + strings.Repeat("a", 201) + `","body":"b"}]}`)},
		{"201 multi-byte runes", []byte(`{"parts":[{"title":"` + strings.Repeat("é", 201) + `","body":"b"}]}`)},
		{"4001-rune body", []byte(`{"parts":[{"title":"t","body":"` + strings.Repeat("b", 4001) + `"}]}`)},
		{"unknown part field", []byte(`{"parts":[{"title":"t","text":"b"}]}`)},
		{"unknown top field", []byte(`{"parts":[` + one + `],"extra":1}`)},
		{"missing parts", []byte(`{}`)},
		{"null parts", []byte(`{"parts":null}`)},
		{"null body", []byte(`null`)},
		{"parts not an array", []byte(`{"parts":"x"}`)},
		{"parts an object", []byte(`{"parts":` + one + `}`)},
		{"part not an object", []byte(`{"parts":["x"]}`)},
		{"title not a string", []byte(`{"parts":[{"title":5,"body":"b"}]}`)},
		{"NUL in body", []byte(`{"parts":[{"title":"t","body":"a\u0000b"}]}`)},
		{"NUL in title", []byte(`{"parts":[{"title":"a\u0000","body":"b"}]}`)},
		{"invalid UTF-8", []byte("{\"parts\":[{\"title\":\"t\",\"body\":\"a\xffb\"}]}")},
		{"lone surrogate escape in body", []byte(`{"parts":[{"title":"t","body":"a\udc00b"}]}`)},
		{"lone surrogate escape in title", []byte(`{"parts":[{"title":"\ud800","body":"b"}]}`)},
		{"replacement character", []byte("{\"parts\":[{\"title\":\"t\",\"body\":\"a�b\"}]}")},
		{"trailing data", []byte(`{"parts":[` + one + `]} {}`)},
		{"trailing garbage", []byte(`{"parts":[` + one + `]}x`)},
		{"empty", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseNote(tc.raw); !errors.Is(err, errBadShape) {
				t.Errorf("err = %v, want errBadShape", err)
			}
		})
	}
}

// The handlers judge the version and the note before touching the database, so
// a nil pool is never reached on a refusal.
func TestHandlerRefusalsNeverTouchTheDatabase(t *testing.T) {
	h := NewHandler(nil)
	r := chi.NewRouter()
	r.Get("/patch-notes/{version}", h.Get)
	r.Put("/patch-notes/{version}", h.Put)
	r.Delete("/patch-notes/{version}", h.Delete)

	tests := []struct {
		name, method, target, body string
		status                     int
		code                       string
	}{
		{"get malformed", http.MethodGet, "/patch-notes/1.2", "", 400, errInvalidVer},
		{"delete malformed", http.MethodDelete, "/patch-notes/01.2.3", "", 400, errInvalidVer},
		{"put malformed version", http.MethodPut, "/patch-notes/1.2.3-beta", `{"parts":[{"title":"t","body":"b"}]}`, 400, errInvalidVer},
		{"put bad note", http.MethodPut, "/patch-notes/1.2.3", `{"parts":[]}`, 400, errInvalidNote},
		{"put typo field", http.MethodPut, "/patch-notes/1.2.3", `{"parts":[{"title":"t","text":"b"}]}`, 400, errInvalidNote},
		{"put oversized", http.MethodPut, "/patch-notes/1.2.3", `{"parts":[{"title":"t","body":"` + strings.Repeat("b", 300<<10) + `"}]}`, 413, errNoteTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body)))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error != tc.code {
				t.Errorf("error = %q (%v), want %q", body.Error, err, tc.code)
			}
		})
	}
}

func TestSQLPins(t *testing.T) {
	if !strings.Contains(listSQL, "ORDER BY major DESC, minor DESC, patch DESC") {
		t.Error("list must order by the version numbers")
	}
	for name, q := range map[string]string{"list": listSQL, "get": getSQL} {
		if strings.Contains(q, "created_at") || strings.Contains(q, "updated_at") {
			t.Errorf("%s selects a timestamp; none goes on the wire", name)
		}
	}
	_, update, found := strings.Cut(upsertSQL, "DO UPDATE SET")
	if !found {
		t.Fatal("upsert has no DO UPDATE")
	}
	if strings.Contains(update, "created_at") {
		t.Error("a replace must keep created_at")
	}
	if !strings.Contains(update, "updated_at = now()") {
		t.Error("a replace must bump updated_at")
	}
	if !strings.Contains(upsertSQL, "xmax = 0") {
		t.Error("upsert must report whether it inserted")
	}
}

// Numeric order, not text order: 0.1.10 above 0.1.9. This is the ordering the
// list SQL asks Postgres for, spelled out in Go.
func TestNumericOrderIsNotTextOrder(t *testing.T) {
	versions := []string{"0.1.9", "0.1.10", "0.1.2", "0.10.0", "0.2.0"}
	sort.Slice(versions, func(i, j int) bool {
		a1, a2, a3, _ := parseVersion(versions[i])
		b1, b2, b3, _ := parseVersion(versions[j])
		if a1 != b1 {
			return a1 > b1
		}
		if a2 != b2 {
			return a2 > b2
		}
		return a3 > b3
	})
	if got := strings.Join(versions, " "); got != "0.10.0 0.2.0 0.1.10 0.1.9 0.1.2" {
		t.Errorf("order = %s", got)
	}
}

func keysOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestWireShape(t *testing.T) {
	// The list handler writes exactly this envelope; an empty list must be [].
	empty, _ := json.Marshal(map[string]any{"patch_notes": []Entry{}})
	if string(empty) != `{"patch_notes":[]}` {
		t.Errorf("empty list = %s", empty)
	}

	entry, _ := json.Marshal(Entry{Version: "0.1.30", Parts: []Part{{Title: "t", Body: "b"}}})
	if got := strings.Join(keysOf(t, entry), ","); got != "parts,version" {
		t.Errorf("entry keys = %s", got)
	}
	var e struct {
		Parts []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(entry, &e); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(keysOf(t, e.Parts[0]), ","); got != "body,title" {
		t.Errorf("part keys = %s", got)
	}
}

// Stand-in for the pool, as the apps test uses: RequireAdmin decides from one row.
type fakeDB struct {
	isAdmin bool
	err     error
	noRows  bool
}

type fakeRow struct {
	isAdmin bool
	err     error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) > 0 {
		if p, ok := dest[0].(*bool); ok {
			*p = r.isAdmin
		}
	}
	return nil
}

func (d fakeDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	if d.noRows {
		return fakeRow{err: pgx.ErrNoRows}
	}
	return fakeRow{isAdmin: d.isAdmin, err: d.err}
}

func (d fakeDB) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func TestWriteRoutesAreAdminGated(t *testing.T) {
	routes := []struct {
		name, method, target, body string
	}{
		{"put", http.MethodPut, "/patch-notes/0.1.30", `{"parts":[{"title":"t","body":"b"}]}`},
		{"delete", http.MethodDelete, "/patch-notes/0.1.30", ""},
	}
	gates := []struct {
		name       string
		db         fakeDB
		wantStatus int
		wantPass   bool
	}{
		{"an admin gets through", fakeDB{isAdmin: true}, http.StatusNoContent, true},
		{"a normal user is forbidden", fakeDB{}, http.StatusForbidden, false},
		{"a deleted user is forbidden", fakeDB{noRows: true}, http.StatusForbidden, false},
		{"a db failure fails CLOSED", fakeDB{err: errors.New("db is down")}, http.StatusInternalServerError, false},
	}

	for _, route := range routes {
		for _, gate := range gates {
			t.Run(route.name+": "+gate.name, func(t *testing.T) {
				h := NewHandler(nil)
				passed := false

				r := chi.NewRouter()
				// The reads stay open to any authenticated user; registering
				// them beside the writes proves the patterns don't collide.
				r.Get("/patch-notes", h.List)
				r.Get("/patch-notes/{version}", h.Get)
				r.Group(func(r chi.Router) {
					r.Use(admin.RequireAdmin(gate.db))
					// Stands in for the handler: past the gate is all this asks.
					r.Use(func(http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							passed = true
							w.WriteHeader(http.StatusNoContent)
						})
					})
					r.Put("/patch-notes/{version}", h.Put)
					r.Delete("/patch-notes/{version}", h.Delete)
				})

				req := httptest.NewRequest(route.method, route.target, strings.NewReader(route.body))
				req = req.WithContext(auth.WithUserID(req.Context(), "user-1"))
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)

				if rec.Code != gate.wantStatus {
					t.Errorf("status = %d, want %d (body %q)", rec.Code, gate.wantStatus, rec.Body.String())
				}
				if passed != gate.wantPass {
					t.Errorf("reached the handler = %v, want %v", passed, gate.wantPass)
				}
			})
		}
	}
}
