package profiles

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tabuhana/bombers-server/internal/auth"
)

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

// A crop that can't be drawn as sent is refused before anything is stored. A nil
// pool is deliberate: reaching the database would mean the refusal came too late
// to be a guarantee.
func TestUpdateMineRefusesAnOutOfRangeCrop(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"avatar x under 0", `{"avatar_crop":{"x":-1,"y":50,"scale":1}}`},
		{"avatar x over 100", `{"avatar_crop":{"x":100.5,"y":50,"scale":1}}`},
		{"avatar y over 100", `{"avatar_crop":{"x":50,"y":101,"scale":1}}`},
		{"avatar scale under 1", `{"avatar_crop":{"x":50,"y":50,"scale":0.5}}`},
		{"banner y under 0", `{"banner_crop":{"x":50,"y":-0.5,"scale":2}}`},
		{"banner scale over 4", `{"banner_crop":{"x":50,"y":50,"scale":4.5}}`},
		// A missing field reads as 0, and scale is the one field where 0 isn't a
		// framing anybody chose — it's a picture drawn at no size.
		{"a crop sent without its scale", `{"banner_crop":{"x":50,"y":50}}`},
		// One bad crop sinks the save even when the other is fine.
		{"a good avatar beside a bad banner", `{"avatar_crop":{"x":50,"y":50,"scale":1},"banner_crop":{"x":50,"y":50,"scale":9}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(nil, nil)
			req := httptest.NewRequest(http.MethodPut, "/me/profile", strings.NewReader(tc.body))
			req = req.WithContext(auth.WithUserID(req.Context(), "user-1"))
			rec := httptest.NewRecorder()

			h.UpdateMine(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if got := errorCode(t, rec); got != errInvalidCrop {
				t.Errorf("error = %q, want %q", got, errInvalidCrop)
			}
		})
	}
}

// The edges are legal: a picture pushed into a corner at full zoom is a framing
// somebody chose.
func TestCropsAtTheirBoundsAreAccepted(t *testing.T) {
	req := updateProfileRequest{
		AvatarCrop: &crop{X: 0, Y: 100, Scale: 1},
		BannerCrop: &crop{X: 100, Y: 0, Scale: 4},
	}
	rec, code := req.toRecord("user-1")
	if code != "" {
		t.Fatalf("refused crops at their bounds: %q", code)
	}
	if *rec.AvatarCrop != *req.AvatarCrop || *rec.BannerCrop != *req.BannerCrop {
		t.Errorf("crops changed on the way to the store: %+v, %+v", *rec.AvatarCrop, *rec.BannerCrop)
	}
}

// First-run setup saves a partial draft, and a partial draft must not reset how
// somebody's pictures are framed. So a crop left out — or sent as null — has to
// reach the store as no crop at all: NULL query arguments, for which the upsert
// keeps what's stored. A zero crop in its place would overwrite it.
func TestOmittedCropsReachTheStoreAsNoCrop(t *testing.T) {
	for _, body := range []string{
		`{"display_name":"Sam"}`,
		`{"display_name":"Sam","avatar_crop":null,"banner_crop":null}`,
	} {
		var req updateProfileRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		rec, code := req.toRecord("user-1")
		if code != "" {
			t.Fatalf("%s was refused: %q", body, code)
		}
		if rec.AvatarCrop != nil || rec.BannerCrop != nil {
			t.Errorf("%s: omitted crops arrived as %+v and %+v", body, rec.AvatarCrop, rec.BannerCrop)
		}
		for _, c := range []*crop{rec.AvatarCrop, rec.BannerCrop} {
			if x, y, scale := c.args(); x != nil || y != nil || scale != nil {
				t.Errorf("%s: an omitted crop produced non-NULL query arguments", body)
			}
		}
	}
}

// The database half of the same rule, checked the only way a test without a
// database can. Every other column in the upsert takes EXCLUDED, so tidying the
// crops into that pattern would look harmless — but EXCLUDED holds the insert's
// fallback for a NULL argument, and every partial save would quietly reset the
// framing to the defaults. The update has to read the stored row.
func TestUpsertFallsBackToTheStoredCrops(t *testing.T) {
	for _, col := range []string{
		"avatar_crop_x", "avatar_crop_y", "avatar_crop_scale",
		"banner_crop_x", "banner_crop_y", "banner_crop_scale",
	} {
		if strings.Contains(upsertProfileSQL, "EXCLUDED."+col) {
			t.Errorf("the upsert takes %s from EXCLUDED, which resets it whenever a save omits the crop", col)
		}
		if !strings.Contains(upsertProfileSQL, "profiles."+col+")") {
			t.Errorf("the upsert never falls back to the stored %s", col)
		}
	}
}

// Crops are on every profile response, and a card with no framing stored — the
// empty default, a card never saved — carries the defaults rather than zeros:
// scale 0 would draw the picture at no size. A stored crop goes out as saved,
// without float64 noise from the REAL columns.
func TestProfileResponsesAlwaysCarryCrops(t *testing.T) {
	const centred = `{"x":50,"y":50,"scale":1}`
	tests := []struct {
		name       string
		resp       profileResponse
		wantAvatar string
		wantBanner string
	}{
		{"the empty default", defaultProfile("user-1"), centred, centred},
		{
			"a card never saved",
			toResponse(&profileRecord{UserID: "user-1", Visibility: VisibilityFriends}, time.Now()),
			centred, centred,
		},
		{
			"a stored crop",
			toResponse(&profileRecord{
				UserID:     "user-1",
				AvatarCrop: &crop{X: 33.3, Y: 66.7, Scale: 1.5},
				BannerCrop: &crop{X: 0, Y: 100, Scale: 4},
			}, time.Now()),
			`{"x":33.3,"y":66.7,"scale":1.5}`, `{"x":0,"y":100,"scale":4}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.resp)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if string(got["avatar_crop"]) != tc.wantAvatar {
				t.Errorf("avatar_crop = %s, want %s", got["avatar_crop"], tc.wantAvatar)
			}
			if string(got["banner_crop"]) != tc.wantBanner {
				t.Errorf("banner_crop = %s, want %s", got["banner_crop"], tc.wantBanner)
			}
		})
	}
}
