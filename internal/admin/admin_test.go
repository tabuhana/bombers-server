package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tabuhana/bombers-server/internal/auth"
)

// A stand-in for the pool: RequireAdmin's whole job is deciding from one row,
// so the row is all a test needs to control.
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

// The gate is the security boundary for every operator action reachable over
// HTTP, so each way through it gets asserted rather than assumed.
func TestRequireAdmin(t *testing.T) {
	reached := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

	tests := []struct {
		name       string
		db         fakeDB
		withUserID bool
		wantStatus int
		wantPass   bool
	}{
		{name: "an admin passes through", db: fakeDB{isAdmin: true}, withUserID: true, wantStatus: http.StatusNoContent, wantPass: true},
		{name: "a normal user is forbidden", db: fakeDB{isAdmin: false}, withUserID: true, wantStatus: http.StatusForbidden},
		{name: "a deleted user is forbidden, not a 500", db: fakeDB{noRows: true}, withUserID: true, wantStatus: http.StatusForbidden},
		{name: "no authenticated user is unauthorized", db: fakeDB{isAdmin: true}, withUserID: false, wantStatus: http.StatusUnauthorized},
		{name: "a db failure fails CLOSED", db: fakeDB{err: errors.New("db is down")}, withUserID: true, wantStatus: http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			passed := false
			handler := RequireAdmin(tc.db)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				passed = true
				reached(w, r)
			}))

			req := httptest.NewRequest(http.MethodPost, "/nodes", nil)
			if tc.withUserID {
				req = req.WithContext(auth.WithUserID(req.Context(), "user-1"))
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if passed != tc.wantPass {
				t.Errorf("reached the handler = %v, want %v", passed, tc.wantPass)
			}
		})
	}
}

func TestIsAdmin(t *testing.T) {
	ctx := context.Background()

	got, err := IsAdmin(ctx, fakeDB{isAdmin: true}, "user-1")
	if err != nil || !got {
		t.Errorf("admin: got %v, err %v", got, err)
	}

	// A missing user is simply not an admin — never an error the caller has to
	// special-case, and never an accidental "true".
	got, err = IsAdmin(ctx, fakeDB{noRows: true}, "ghost")
	if err != nil || got {
		t.Errorf("missing user: got %v, err %v — want false, nil", got, err)
	}

	if _, err = IsAdmin(ctx, fakeDB{err: errors.New("boom")}, "user-1"); err == nil {
		t.Error("a db error must propagate so the caller can fail closed")
	}
}
