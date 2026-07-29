package packs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tabuhana/bombers-server/internal/admin"
	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/media"
)

// A media store that records instead of storing. The whole point of the checks
// under test is that a refused request never reaches object storage, so "what
// did it touch" is the assertion, not "what did it return".
type recordingStore struct {
	puts    []string
	removed []string
}

func (s *recordingStore) PutObject(_ context.Context, key string, _ []byte, _ string) error {
	s.puts = append(s.puts, key)
	return nil
}

func (s *recordingStore) GetObject(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, media.ErrObjectNotFound
}

func (s *recordingStore) RemovePrefix(_ context.Context, prefix string) error {
	s.removed = append(s.removed, prefix)
	return nil
}

func (s *recordingStore) Put(_ context.Context, userID, kind string, _ []byte, _ string) error {
	s.puts = append(s.puts, userID+"/"+kind)
	return nil
}

func (s *recordingStore) Get(_ context.Context, _, _ string) (io.ReadCloser, error) {
	return nil, media.ErrObjectNotFound
}

func (s *recordingStore) Remove(_ context.Context, userID, kind string) error {
	s.removed = append(s.removed, userID+"/"+kind)
	return nil
}

func (s *recordingStore) Ping(_ context.Context) error { return nil }

// A stand-in for the pool, exactly as admin's own test uses: RequireAdmin
// decides from one row, so the row is all this needs to control.
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

// errorCode pulls the code out of the shared error envelope, so a test asserts
// the contract the client branches on rather than a substring of a message.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the error envelope: %q", rec.Body.String())
	}
	return body.Error
}

// Publishing a pack is the operator's most powerful reachable-over-HTTP action:
// it replaces what every client installs. Each publish route must be behind the
// admin gate — mounting one outside it (or under the wrong method/pattern) is
// the mistake this pins down.
func TestPublishRoutesAreAdminGated(t *testing.T) {
	routes := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{"publish", http.MethodPost, "/packs", `{"id":"midnight","name":"Midnight"}`},
		{"upload an asset", http.MethodPut, "/packs/midnight/assets/sounds/dm.mp3", "sound bytes"},
		{"unpublish", http.MethodDelete, "/packs/midnight", ""},
	}

	gates := []struct {
		name       string
		db         fakeDB
		wantStatus int
		wantPass   bool
	}{
		{name: "an admin gets through", db: fakeDB{isAdmin: true}, wantStatus: http.StatusNoContent, wantPass: true},
		{name: "a normal user is forbidden", db: fakeDB{}, wantStatus: http.StatusForbidden},
		{name: "a deleted user is forbidden", db: fakeDB{noRows: true}, wantStatus: http.StatusForbidden},
		{name: "a db failure fails CLOSED", db: fakeDB{err: errors.New("db is down")}, wantStatus: http.StatusInternalServerError},
	}

	for _, route := range routes {
		for _, gate := range gates {
			t.Run(route.name+": "+gate.name, func(t *testing.T) {
				h := NewHandler(nil, &recordingStore{})
				passed := false

				r := chi.NewRouter()
				// The read side stays open to any authenticated user — it shares
				// the /packs/{id}/assets/* pattern, so registering it here proves
				// the two don't collide.
				r.Get("/packs", h.List)
				r.Get("/packs/{id}/assets/*", h.Asset)
				r.Group(func(r chi.Router) {
					r.Use(admin.RequireAdmin(gate.db))
					// Stands in for the handler: past the gate is all this test
					// asks about, and the handlers below need a database.
					r.Use(func(http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							passed = true
							w.WriteHeader(http.StatusNoContent)
						})
					})
					r.Post("/packs", h.Publish)
					r.Put("/packs/{id}/assets/*", h.PublishAsset)
					r.Delete("/packs/{id}", h.Unpublish)
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

// An asset upload writes to object storage under a key built from the pack id
// and the asset path. Both halves come off the wire, so both are validated
// before anything is written — a traversal through a published pack would be
// the worst kind of bug.
func TestPublishAssetRefusesUnsafeKeys(t *testing.T) {
	tests := []struct {
		name string
		id   string
		path string
		want string
	}{
		{"climbs out of the pack folder", "midnight", "../../etc/passwd", errInvalidAssetPath},
		{"climbs out mid-path", "midnight", "sounds/../../secret", errInvalidAssetPath},
		{"dot segment", "midnight", "sounds/./dm.mp3", errInvalidAssetPath},
		{"windows separator", "midnight", `sounds\dm.mp3`, errInvalidAssetPath},
		{"empty segment", "midnight", "sounds//dm.mp3", errInvalidAssetPath},
		{"no path at all", "midnight", "", errInvalidAssetPath},
		{"pack id climbs out", "..", "dm.mp3", errInvalidPackID},
		{"pack id carries a separator", "a/b", "dm.mp3", errInvalidPackID},
		{"pack id is empty", "", "dm.mp3", errInvalidPackID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			storage := &recordingStore{}
			// A nil pool is deliberate: reaching the database would mean the
			// refusal came too late to be a guarantee.
			h := NewHandler(nil, storage)

			req := httptest.NewRequest(http.MethodPut, "/packs/x/assets/x", strings.NewReader("bytes"))
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", tc.id)
			rctx.URLParams.Add("*", tc.path)
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

			rec := httptest.NewRecorder()
			h.PublishAsset(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if got := errorCode(t, rec); got != tc.want {
				t.Errorf("error = %q, want %q", got, tc.want)
			}
			if len(storage.puts) != 0 || len(storage.removed) != 0 {
				t.Errorf("a refused upload touched storage: put %v, removed %v", storage.puts, storage.removed)
			}
		})
	}
}

// pack.json is stored verbatim, so what it claims about itself is checked
// before it is stored — every case here fails without a database being needed.
func TestPublishRefusesMalformedPacks(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"not JSON", "sounds/dm.mp3", http.StatusBadRequest, errInvalidPackJSON},
		{"JSON, but not an object", `["midnight"]`, http.StatusBadRequest, errInvalidPackJSON},
		{"no id or name", `{}`, http.StatusBadRequest, errInvalidPackManifest},
		{"id but no name", `{"id":"midnight"}`, http.StatusBadRequest, errInvalidPackManifest},
		{"a whitespace name", `{"id":"midnight","name":"  "}`, http.StatusBadRequest, errInvalidPackManifest},
		{
			"a theme family over the cap",
			`{"id":"huge","name":"Huge","themes":[` + strings.TrimSuffix(strings.Repeat(`{},`, MaxThemesPerPack+1), ",") + `]}`,
			http.StatusBadRequest,
			errTooManyThemes,
		},
		{"an id that escapes packs/", `{"id":"../users","name":"Midnight"}`, http.StatusBadRequest, errInvalidPackID},
		{"over the bundle cap", `{"id":"midnight","name":"` + strings.Repeat("a", BundleLimit) + `"}`, http.StatusRequestEntityTooLarge, errBundleTooLarge},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			storage := &recordingStore{}
			h := NewHandler(nil, storage)

			req := httptest.NewRequest(http.MethodPost, "/packs", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.Publish(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := errorCode(t, rec); got != tc.wantCode {
				t.Errorf("error = %q, want %q", got, tc.wantCode)
			}
			if len(storage.removed) != 0 {
				t.Errorf("a refused publish cleared assets: %v", storage.removed)
			}
		})
	}
}

func TestValidPackID(t *testing.T) {
	for _, ok := range []string{"midnight", "sample-pack", "pack_2", "v1.2", "A"} {
		if !ValidPackID(ok) {
			t.Errorf("%q should be allowed", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "packs/../x", "with space", strings.Repeat("a", 65)} {
		if ValidPackID(bad) {
			t.Errorf("%q should be REFUSED", bad)
		}
	}
}

func TestAssetContentType(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 24))

	if got := assetContentType("audio/mpeg", []byte("id3")); got != "audio/mpeg" {
		t.Errorf("declared type should win: got %q", got)
	}
	// Parameters are noise for the metadata row.
	if got := assetContentType("text/css; charset=utf-8", nil); got != "text/css" {
		t.Errorf("got %q, want text/css", got)
	}
	// Nothing useful declared → fall back to the bytes, which is the only thing
	// that can't lie about itself.
	if got := assetContentType("", png); got != "image/png" {
		t.Errorf("got %q, want image/png", got)
	}
	if got := assetContentType("application/octet-stream", png); got != "image/png" {
		t.Errorf("a placeholder type should not beat the bytes: got %q", got)
	}
}
