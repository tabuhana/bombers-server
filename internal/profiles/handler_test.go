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

// A birthday is as much of the date as the user knows, so every shape that can
// take is saved exactly as sent, and a blank one clears it. Anything else is
// refused rather than guessed at — including a date that only looks right.
func TestABirthdayIsAsMuchOfTheDateAsIsKnown(t *testing.T) {
	const refused = "refused" // not a birthday, so never a stored value
	tests := []struct {
		sent string
		want string // what's stored: "" for nothing, or refused
	}{
		{"1990-03-14", "1990-03-14"}, // a whole date
		{"2024-02-29", "2024-02-29"}, // Feb 29, in a year that had one
		{"03-14", "03-14"},           // month and day, no year
		{"02-29", "02-29"},           // Feb 29 needs no year: somebody was born on one
		{"1990-03", "1990-03"},       // month and year
		{"1990", "1990"},             // year only
		{"03", "03"},                 // month only
		{"0001-01-01", "0001-01-01"}, // the first year
		{"9999-12-31", "9999-12-31"}, // and the last
		{" 03-14\n", "03-14"},        // trimmed first
		{"", ""},                     // clears it
		{"   ", ""},                  // and so does whitespace

		{"2023-02-29", refused}, // 2023 had no Feb 29
		{"02-30", refused},      // and no February has a 30th
		{"13", refused},
		{"1990-13", refused},
		{"0000", refused},     // nobody was born in year 0
		{"1990-3-4", refused}, // every part is zero-padded
		{"3/14", refused},
		{"abc", refused},
	}
	for _, tc := range tests {
		req := updateProfileRequest{Birthday: tc.sent}
		rec, code := req.toRecord("user-1")
		switch {
		case tc.want == refused:
			if code != errInvalidBirthday {
				t.Errorf("%q: error = %q, want %q", tc.sent, code, errInvalidBirthday)
			}
		case code != "":
			t.Errorf("%q was refused: %q", tc.sent, code)
		case tc.want == "":
			if rec.Birthday != nil {
				t.Errorf("%q should clear the birthday, not store %q", tc.sent, *rec.Birthday)
			}
		case rec.Birthday == nil || *rec.Birthday != tc.want:
			t.Errorf("%q wasn't stored as %q", tc.sent, tc.want)
		}
	}
}

// A birthday goes back out exactly as it was stored, whole or not, and an age
// comes only from a whole date: without the year there's nothing to count from,
// and without the day nobody can say whether this year's birthday has passed.
func TestAgeComesOnlyFromAWholeBirthday(t *testing.T) {
	now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		stored string // "" for a card with no birthday
		age    string // as it goes out on the wire
	}{
		{"1990-09-14", "36"}, // the birthday is today
		{"1990-09-15", "35"}, // and this one is tomorrow
		{"09-14", "null"},
		{"1990-09", "null"},
		{"1990", "null"},
		{"09", "null"},
		{"", "null"},
	}
	for _, tc := range tests {
		rec := profileRecord{UserID: "user-1"}
		wantBirthday := "null"
		if tc.stored != "" {
			rec.Birthday = &tc.stored
			wantBirthday = `"` + tc.stored + `"`
		}
		raw, err := json.Marshal(toResponse(&rec, now))
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if string(got["birthday"]) != wantBirthday {
			t.Errorf("birthday %q went out as %s", tc.stored, got["birthday"])
		}
		if string(got["age"]) != tc.age {
			t.Errorf("birthday %q: age = %s, want %s", tc.stored, got["age"], tc.age)
		}
	}
}
