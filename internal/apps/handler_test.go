package apps

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

func (s *recordingStore) ListObjects(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	for _, k := range s.puts {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
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

// An app id may carry a namespace and therefore a colon — an app published
// under the id of the node it replaces is `core:weather`. A client
// percent-encodes that into the path, and chi routes on the RAW path whenever
// it differs from the decoded one, handing back `core%3Aweather`, which matches
// no row.
//
// The node store paid for this once, and the symptom was misleading: the
// catalogue worked (no id in the URL) while installing failed, so it read as a
// publishing problem rather than a reading one.
func TestAppIDIsDecoded(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"an encoded colon", "core%3Aweather", "core:weather"},
		{"an unencoded colon", "core:weather", "core:weather"},
		{"a plain id", "clock", "clock"},
		// A broken escape is a lookup that should MISS, not a request that
		// should fall over.
		{"a malformed escape survives", "core%zzweather", "core%zzweather"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", tc.raw)
			r := httptest.NewRequest(http.MethodGet, "/apps/x/bundle", nil)
			r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

			if got := appID(r); got != tc.want {
				t.Errorf("appID(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// Publishing an app is the operator's most powerful reachable-over-HTTP action:
// it replaces what every client installs. Each publish route must be behind the
// admin gate — mounting one outside it (or under the wrong method/pattern) is
// the mistake this pins down.
//
// `nodes` has no test like this, which is why one is here: the three routes it
// covers were copied, and a copy is exactly where a gate goes missing.
func TestPublishRoutesAreAdminGated(t *testing.T) {
	routes := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{"publish", http.MethodPost, "/apps", `{"manifest":{"id":"clock","name":"Clock"},"files":{"screen.jsx":"x"}}`},
		{"upload an asset", http.MethodPut, "/apps/clock/assets/icon.svg", "svg bytes"},
		{"unpublish", http.MethodDelete, "/apps/clock", ""},
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
				// the /apps/{id}/assets/* pattern, so registering it here proves
				// the two don't collide.
				r.Get("/apps", h.List)
				r.Get("/apps/{id}/bundle", h.Download)
				r.Get("/apps/{id}/assets/*", h.Asset)
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
					r.Post("/apps", h.Publish)
					r.Put("/apps/{id}/assets/*", h.PublishAsset)
					r.Delete("/apps/{id}", h.Unpublish)
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

// An asset upload writes to object storage under a key built from the app id
// and the asset path. Both halves come off the wire, so both are validated
// before anything is written — a traversal through a published bundle would be
// the worst kind of bug.
func TestPublishAssetRefusesUnsafeKeys(t *testing.T) {
	tests := []struct {
		name string
		id   string
		path string
		want string
	}{
		{"climbs out of the app folder", "clock", "../../etc/passwd", errInvalidAssetPath},
		{"climbs out mid-path", "clock", "art/../../secret", errInvalidAssetPath},
		{"dot segment", "clock", "art/./icon.svg", errInvalidAssetPath},
		{"windows separator", "clock", `art\icon.svg`, errInvalidAssetPath},
		{"empty segment", "clock", "art//icon.svg", errInvalidAssetPath},
		{"no path at all", "clock", "", errInvalidAssetPath},
		{"app id climbs out", "..", "icon.svg", errInvalidID},
		{"app id carries a separator", "a/b", "icon.svg", errInvalidID},
		{"app id is hidden", ".ssh", "icon.svg", errInvalidID},
		{"app id is empty", "", "icon.svg", errInvalidID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			storage := &recordingStore{}
			// A nil pool is deliberate: reaching the database would mean the
			// refusal came too late to be a guarantee.
			h := NewHandler(nil, storage)

			req := httptest.NewRequest(http.MethodPut, "/apps/x/assets/x", strings.NewReader("bytes"))
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

// THE READ SIDE IS OPAQUE: an unknown app, an id that is not one and a path
// that is not one are the same answer, because a reader has no business
// learning which. (The WRITE side says which, because the operator is the one
// who can fix it.)
func TestAssetReadIsOpaque(t *testing.T) {
	for _, tc := range []struct{ name, id, path string }{
		{"an id that is a path", "a/b", "icon.svg"},
		{"a path that climbs out", "clock", "../../etc/passwd"},
		{"no path at all", "clock", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(nil, &recordingStore{})
			req := httptest.NewRequest(http.MethodGet, "/apps/x/assets/x", nil)
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", tc.id)
			rctx.URLParams.Add("*", tc.path)
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

			rec := httptest.NewRecorder()
			h.Asset(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
			if got := errorCode(t, rec); got != errAppNotFound {
				t.Errorf("error = %q, want %q", got, errAppNotFound)
			}
		})
	}
}

// A bundle is checked before it is stored, so every case here fails without a
// database — and a refused publish must not have cleared the assets of
// whatever is published under that id already.
func TestPublishRefusesMalformedBundles(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"not JSON", "screen.jsx", http.StatusBadRequest, ""},
		{"no manifest", `{}`, http.StatusBadRequest, ""},
		{"no files", `{"manifest":{"id":"clock","name":"Clock"}}`, http.StatusBadRequest, ""},
		{"an id that escapes apps/", `{"manifest":{"id":"../users","name":"Clock"},"files":{"a":"b"}}`, http.StatusBadRequest, ""},
		{
			"over the bundle cap",
			`{"manifest":{"id":"clock","name":"` + strings.Repeat("a", BundleLimit) + `"},"files":{"a":"b"}}`,
			http.StatusRequestEntityTooLarge,
			errBundleTooLarge,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			storage := &recordingStore{}
			h := NewHandler(nil, storage)

			req := httptest.NewRequest(http.MethodPost, "/apps", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.Publish(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			// A malformed bundle answers with the REASON rather than a code, so
			// only the cap has a fixed string to pin. The rest must simply be
			// the error envelope and not empty.
			got := errorCode(t, rec)
			if tc.wantCode != "" && got != tc.wantCode {
				t.Errorf("error = %q, want %q", got, tc.wantCode)
			}
			if got == "" {
				t.Error("a refusal said nothing")
			}
			if len(storage.removed) != 0 {
				t.Errorf("a refused publish cleared assets: %v", storage.removed)
			}
		})
	}
}

// AN ASSET BELONGS TO A PUBLISHED APP, and an upload with no object store
// behind it must not record a row pointing at nothing. Both refusals come
// before any write.
func TestPublishAssetNeedsSomewhereToPutIt(t *testing.T) {
	h := NewHandler(nil, nil) // no media store at all

	req := httptest.NewRequest(http.MethodPut, "/apps/clock/assets/icon.svg", strings.NewReader("bytes"))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "clock")
	rctx.URLParams.Add("*", "icon.svg")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	rec := httptest.NewRecorder()
	h.PublishAsset(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if got := errorCode(t, rec); got != errNoMediaStore {
		t.Errorf("error = %q, want %q", got, errNoMediaStore)
	}
}

// An empty body is not a file. It is refused before the database is asked
// whether the app exists, so a nil pool proves the order.
func TestPublishAssetRefusesAnEmptyFile(t *testing.T) {
	storage := &recordingStore{}
	h := NewHandler(nil, storage)

	req := httptest.NewRequest(http.MethodPut, "/apps/clock/assets/icon.svg", strings.NewReader(""))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "clock")
	rctx.URLParams.Add("*", "icon.svg")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	rec := httptest.NewRecorder()
	h.PublishAsset(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if got := errorCode(t, rec); got != errInvalidBody {
		t.Errorf("error = %q, want %q", got, errInvalidBody)
	}
	if len(storage.puts) != 0 {
		t.Errorf("an empty upload reached storage: %v", storage.puts)
	}
}

// An asset past the 8 MiB cap is refused by the reader, not after it has been
// buffered and measured.
func TestPublishAssetRefusesAnOversizeFile(t *testing.T) {
	storage := &recordingStore{}
	h := NewHandler(nil, storage)

	body := strings.NewReader(strings.Repeat("a", AssetLimit+1))
	req := httptest.NewRequest(http.MethodPut, "/apps/clock/assets/icon.svg", body)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "clock")
	rctx.URLParams.Add("*", "icon.svg")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	rec := httptest.NewRecorder()
	h.PublishAsset(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if got := errorCode(t, rec); got != errAssetTooLarge {
		t.Errorf("error = %q, want %q", got, errAssetTooLarge)
	}
	if len(storage.puts) != 0 {
		t.Errorf("an oversize upload reached storage: %v", storage.puts)
	}
}
