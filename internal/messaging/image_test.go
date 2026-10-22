package messaging

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ---- the harness: an object store that records what it was asked --------------

// fakeObjects stands in for the media store. Like fakeDB it is a check on wiring - what
// is put, where, what is removed and when - and not on S3 or the filesystem.
type fakeObjects struct {
	mu    sync.Mutex
	objs  map[string][]byte
	types map[string]string

	puts, gets, removes []string // keys, and for removes the prefix, in call order

	putErr, getErr, removeErr error
}

func newFakeObjects() *fakeObjects {
	return &fakeObjects{objs: map[string][]byte{}, types: map[string]string{}}
}

func (o *fakeObjects) PutObject(_ context.Context, key string, data []byte, contentType string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.puts = append(o.puts, key)
	if o.putErr != nil {
		return o.putErr
	}
	o.objs[key] = append([]byte(nil), data...)
	o.types[key] = contentType
	return nil
}

func (o *fakeObjects) GetObject(_ context.Context, key string) (io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gets = append(o.gets, key)
	if o.getErr != nil {
		return nil, o.getErr
	}
	data, ok := o.objs[key]
	if !ok {
		return nil, errors.New("fake: no such object")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// RemovePrefix removes every key that begins with the prefix, as both real backends do.
func (o *fakeObjects) RemovePrefix(_ context.Context, prefix string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.removes = append(o.removes, prefix)
	if o.removeErr != nil {
		return o.removeErr
	}
	for k := range o.objs {
		if strings.HasPrefix(k, prefix) {
			delete(o.objs, k)
			delete(o.types, k)
		}
	}
	return nil
}

func (o *fakeObjects) keys() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for k := range o.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- pictures to send -----------------------------------------------------------

func solidImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 40, A: 255})
		}
	}
	return img
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, solidImage(w, h)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, solidImage(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	pal := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White})
	if err := gif.Encode(&b, pal, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// testWebP is the header an animated WebP begins with (VP8X) followed by filler, which
// is as much of one as the server ever reads.
func testWebP(w, h int) []byte {
	body := make([]byte, 10)
	body[0] = 0x12
	body[4], body[5], body[6] = byte(w-1), byte((w-1)>>8), byte((w-1)>>16)
	body[7], body[8], body[9] = byte(h-1), byte((h-1)>>8), byte((h-1)>>16)
	chunk := append([]byte("VP8X"), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(chunk[4:], uint32(len(body)))
	chunk = append(chunk, body...)
	out := append([]byte("RIFF"), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(out[4:], uint32(4+len(chunk)))
	out = append(out, "WEBP"...)
	return append(out, chunk...)
}

// formPart is one part of a picture upload as a test wants to send it. filename and
// contentType are what a client may say about the part and the server must not believe.
type formPart struct {
	name, filename, contentType string
	data                        []byte
}

func filePart(data []byte) formPart { return formPart{name: "file", filename: "pic", data: data} }
func captionPart(text string) formPart {
	return formPart{name: "caption", data: []byte(text)}
}

// multipartBody builds a multipart/form-data body the way a client does, and returns it
// with the Content-Type that names its boundary.
func multipartBody(parts ...formPart) ([]byte, string) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		disposition := `form-data; name="` + p.name + `"`
		if p.filename != "" {
			disposition += `; filename="` + p.filename + `"`
		}
		h.Set("Content-Disposition", disposition)
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		w, err := mw.CreatePart(h)
		if err != nil {
			panic(err)
		}
		w.Write(p.data)
	}
	mw.Close()
	return b.Bytes(), mw.FormDataContentType()
}

func imageQuery(to, nonce string) string {
	q := url.Values{}
	if to != "" {
		q.Set("to", to)
	}
	if nonce != "" {
		q.Set("nonce", nonce)
	}
	return q.Encode()
}

// postBody sends exactly the bytes it is given, labelled as it is told: the lowest
// level, for the tests whose subject is a body that is not what a client sends.
func (e *env) postBody(user, rawQuery string, body []byte, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/messages/image?"+rawQuery, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	e.router(user).ServeHTTP(rec, req)
	return rec
}

// postParts uploads a picture the way a client does: the parts as multipart/form-data,
// the recipient and nonce in the query.
func (e *env) postParts(user, to, nonce string, parts ...formPart) *httptest.ResponseRecorder {
	body, contentType := multipartBody(parts...)
	return e.postBody(user, imageQuery(to, nonce), body, contentType)
}

// postImage uploads a picture with no caption.
func (e *env) postImage(user, to, nonce string, data []byte) *httptest.ResponseRecorder {
	return e.postParts(user, to, nonce, filePart(data))
}

// postCaptioned uploads a picture with a caption.
func (e *env) postCaptioned(user, to, nonce string, data []byte, caption string) *httptest.ResponseRecorder {
	return e.postParts(user, to, nonce, filePart(data), captionPart(caption))
}

func (e *env) getImage(user, id string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/messages/"+id+"/image", nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	e.router(user).ServeHTTP(rec, req)
	return rec
}

// imageEnv is two friends, u1 and u2, who have not said anything yet.
func imageEnv() *env {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	e.db.befriend("u1", "u3")
	return e
}

// sentImage uploads a picture from u1 to u2 and returns its message id.
func sentImage(t *testing.T, e *env, data []byte) string {
	t.Helper()
	rec := e.postImage("u1", "u2", "", data)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d %s, want 201", rec.Code, rec.Body)
	}
	return decode[sendResponse](t, rec).ID
}

func nothingHappened(t *testing.T, e *env) {
	t.Helper()
	if len(e.db.msgs) != 0 || len(e.ln.pushes) != 0 || len(e.objs.puts) != 0 || len(e.objs.objs) != 0 {
		t.Errorf("a refused upload stored or pushed something: msgs=%d pushes=%d puts=%v objects=%v",
			len(e.db.msgs), len(e.ln.pushes), e.objs.puts, e.objs.keys())
	}
}

// ---- sending ------------------------------------------------------------------

// The picture is the message: kind image, no words, a payload read from the bytes, and
// the bytes themselves under the message's own id. Both people are told, each with the
// payload; the response is the same shape POST /messages answers with.
func TestSendImageStoresTheMessageAndPutsTheBytesUnderItsID(t *testing.T) {
	e := imageEnv()
	data := testPNG(t, 37, 21)

	rec := e.postImage("u1", "u2", "n1", data)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d %s, want 201", rec.Code, rec.Body)
	}
	got := decode[sendResponse](t, rec)
	if got.Kind != "image" || got.Body != "" || got.SenderID != "u1" || got.RecipientID != "u2" || got.Nonce != "n1" {
		t.Errorf("response = %+v", got)
	}
	var p map[string]any
	if err := json.Unmarshal(got.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if len(p) != 4 || p["width"] != float64(37) || p["height"] != float64(21) || p["mime"] != "image/png" || p["size"] != float64(len(data)) {
		t.Errorf("payload = %v, want width, height, mime and size and nothing else (above all no id)", p)
	}

	// One row, and the bytes under messages/<that id>/image, untouched.
	if len(e.db.msgs) != 1 || e.db.msgs[0].ID != got.ID || e.db.msgs[0].Kind != kindImage {
		t.Fatalf("stored = %+v", e.db.msgs)
	}
	key := "messages/" + got.ID + "/image"
	if keys := e.objs.keys(); len(keys) != 1 || keys[0] != key {
		t.Fatalf("objects = %v, want exactly %s", keys, key)
	}
	if !bytes.Equal(e.objs.objs[key], data) || e.objs.types[key] != "image/png" {
		t.Error("the stored bytes or their type are not what was sent")
	}

	// Told to both, as a new message carrying the payload.
	for who, frame := range e.ln.frames(t) {
		msg := frame["message"].(map[string]any)
		pl, _ := msg["payload"].(map[string]any)
		if frame["op"] != "new" || frame["nonce"] != "n1" || msg["kind"] != "image" || pl["width"] != float64(37) {
			t.Errorf("%s was told %v", who, frame)
		}
	}

	// And it is in the thread as an image.
	hist := asJSONBody(t, e.do("u2", http.MethodGet, "/messages/u1", ""))
	m := hist["messages"].([]any)[0].(map[string]any)
	if m["kind"] != "image" || m["body"] != "" || m["payload"] == nil {
		t.Errorf("history says %v", m)
	}
}

// All four accepted formats, with their real dimensions and their sniffed type.
func TestSendImageAcceptsEachFormat(t *testing.T) {
	for _, tc := range []struct {
		name          string
		data          []byte
		mime          string
		width, height int
	}{
		{"png", testPNG(t, 30, 20), "image/png", 30, 20},
		{"jpeg", testJPEG(t, 31, 21), "image/jpeg", 31, 21},
		{"gif", testGIF(t, 32, 22), "image/gif", 32, 22},
		{"webp (animated canvas)", testWebP(640, 480), "image/webp", 640, 480},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := imageEnv()
			rec := e.postImage("u1", "u2", "", tc.data)
			if rec.Code != http.StatusCreated {
				t.Fatalf("got %d %s", rec.Code, rec.Body)
			}
			var p imagePayload
			if err := json.Unmarshal(decode[sendResponse](t, rec).Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p != (imagePayload{Width: tc.width, Height: tc.height, Mime: tc.mime, Size: len(tc.data)}) {
				t.Errorf("payload = %+v", p)
			}
		})
	}
}

// A phone stores a portrait photo turned on its side and says so in EXIF; the browser
// draws it upright, and the payload is what the thread reserves the picture's box from,
// so it has to carry the drawn shape or the bubble changes shape when the picture lands.
func TestSendImageStoresTheShapeTheBrowserDraws(t *testing.T) {
	upright := testJPEG(t, 31, 21) // as the sensor saw it: wider than tall
	// An APP1 right after the start of image: "Exif", then a big-endian TIFF whose one
	// IFD0 entry is Orientation (0x0112), type SHORT, count 1, value 6.
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0, 0, 0, 0, 0, 0, 0}
	app1 := append([]byte{0xff, 0xe1, 0, 0}, append([]byte("Exif\x00\x00"), tiff...)...)
	app1[2], app1[3] = byte((len(app1)-2)>>8), byte(len(app1)-2)
	turned := append(append([]byte{0xff, 0xd8}, app1...), upright[2:]...)

	e := imageEnv()
	rec := e.postImage("u1", "u2", "", turned)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	var p imagePayload
	if err := json.Unmarshal(decode[sendResponse](t, rec).Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Width != 21 || p.Height != 31 {
		t.Errorf("payload is %dx%d, want the drawn 21x31", p.Width, p.Height)
	}
	if p.Size != len(turned) {
		t.Errorf("size = %d, want the length of what was sent (%d)", p.Size, len(turned))
	}
}

// What the client says the file is does not matter, in either direction: the type is
// sniffed from the bytes.
func TestSendImageIgnoresTheDeclaredContentType(t *testing.T) {
	e := imageEnv()
	png := filePart(testPNG(t, 4, 4))
	png.contentType = "text/plain"
	if rec := e.postParts("u1", "u2", "", png); rec.Code != http.StatusCreated {
		t.Errorf("a png labelled text/plain = %d %s, want 201", rec.Code, rec.Body)
	}
	e2 := imageEnv()
	html := filePart([]byte("<html>not a picture</html>"))
	html.contentType = "image/png"
	rec := e2.postParts("u1", "u2", "", html)
	if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
		t.Errorf("html labelled image/png = %d %s, want 415 unsupported_media_type", rec.Code, rec.Body)
	}
	nothingHappened(t, e2)
}

func TestSendImageRefusesWhatIsNotOneOfTheFourFormats(t *testing.T) {
	bmp := append([]byte("BM"), make([]byte, 60)...)
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"></svg>`)
	for name, body := range map[string][]byte{
		"text": []byte("hello there"), "svg": svg, "bmp": bmp, "empty": {}, "a pdf": []byte("%PDF-1.4 ......"),
	} {
		t.Run(name, func(t *testing.T) {
			e := imageEnv()
			rec := e.postImage("u1", "u2", "", body)
			if rec.Code != http.StatusUnsupportedMediaType || errorCode(t, rec) != "unsupported_media_type" {
				t.Errorf("got %d %s, want 415 unsupported_media_type", rec.Code, rec.Body)
			}
			nothingHappened(t, e)
		})
	}
}

// 10 MiB is the cap: exactly that is a picture, one byte more is refused, and nothing
// of the refused one is kept.
func TestSendImageCapIsTenMiB(t *testing.T) {
	// The number itself, not the constant: the cap is a promise to the client, and a test
	// built from the constant would follow it wherever it was moved.
	const tenMiB = 10 * 1024 * 1024
	if maxImageBytes != tenMiB {
		t.Fatalf("maxImageBytes = %d, want %d", maxImageBytes, tenMiB)
	}
	png := testPNG(t, 8, 8)
	exact := append(append([]byte(nil), png...), make([]byte, tenMiB-len(png))...)
	over := append(append([]byte(nil), png...), make([]byte, tenMiB-len(png)+1)...)

	e := imageEnv()
	if rec := e.postImage("u1", "u2", "", exact); rec.Code != http.StatusCreated {
		t.Fatalf("a picture of exactly %d bytes = %d %s, want 201", tenMiB, rec.Code, rec.Body)
	}

	e2 := imageEnv()
	rec := e2.postImage("u1", "u2", "", over)
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "message_too_large" {
		t.Errorf("a picture one byte over = %d %s, want 413 message_too_large", rec.Code, rec.Body)
	}
	nothingHappened(t, e2)
}

// The size has to come out of the bytes. One that cannot is refused, not stored without
// it, and a picture that would ask a client to hold billions of pixels is refused even
// though it is a handful of bytes.
func TestSendImageRefusesWhatCannotBeMeasured(t *testing.T) {
	pngBytes := testPNG(t, 8, 8)
	zero := append([]byte(nil), pngBytes...)
	binary.BigEndian.PutUint32(zero[16:], 0)
	huge := append([]byte(nil), pngBytes...)
	binary.BigEndian.PutUint32(huge[16:], 20000)
	binary.BigEndian.PutUint32(huge[20:], 20000) // 400 megapixels in a few hundred bytes
	atLimit := append([]byte(nil), pngBytes...)
	binary.BigEndian.PutUint32(atLimit[16:], 10000)
	binary.BigEndian.PutUint32(atLimit[20:], 10000) // exactly maxImagePixels

	for name, tc := range map[string]struct {
		body []byte
		want int
	}{
		"truncated in the middle of its header":    {pngBytes[:20], http.StatusBadRequest},
		"just the signature":                       {pngBytes[:8], http.StatusBadRequest},
		"a jpeg cut off before its frame header":   {testJPEG(t, 8, 8)[:12], http.StatusBadRequest},
		"a zero width":                             {zero, http.StatusBadRequest},
		"more pixels than a client should be told": {huge, http.StatusBadRequest},
		"exactly the pixel limit":                  {atLimit, http.StatusCreated},
	} {
		t.Run(name, func(t *testing.T) {
			e := imageEnv()
			rec := e.postImage("u1", "u2", "", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("got %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.want == http.StatusBadRequest {
				if errorCode(t, rec) != "invalid_image" {
					t.Errorf("code = %s, want invalid_image", errorCode(t, rec))
				}
				nothingHappened(t, e)
			}
		})
	}
}

// Who may be sent to is the question POST /messages answers, and with the same answer:
// the opaque 404, whether the target is a stranger, nobody, yourself or missing. The
// gate comes BEFORE the body is read, so a stranger cannot make the server buffer a
// picture - and an oversize body to a stranger is therefore the same 404, not a 413.
func TestSendImageIsFriendGatedWithThePlainSendsAnswer(t *testing.T) {
	e := newEnv("u1", "u2", "u3")
	e.db.befriend("u1", "u2")
	big := append(testPNG(t, 8, 8), make([]byte, maxImageBytes)...)

	want := e.do("u1", http.MethodPost, "/messages", jsonBody(t, map[string]any{"recipient_id": "u3", "body": "hi"}))
	if want.Code != http.StatusNotFound {
		t.Fatalf("plain send to a non-friend = %d", want.Code)
	}
	for name, to := range map[string]string{"a non-friend": "u3", "nobody": "ghost", "yourself": "u1", "no recipient": ""} {
		for _, body := range [][]byte{testPNG(t, 8, 8), big, []byte("junk")} {
			rec := e.postImage("u1", to, "", body)
			if rec.Code != http.StatusNotFound || errorCode(t, rec) != "recipient_not_found" {
				t.Errorf("%s: got %d %s, want 404 recipient_not_found", name, rec.Code, rec.Body)
			}
			if name == "a non-friend" && rec.Body.String() != want.Body.String() {
				t.Errorf("the refusal differs from POST /messages' byte for byte:\n%s\n%s", rec.Body, want.Body)
			}
		}
	}
	nothingHappened(t, e)
}

func TestSendImageNonceLength(t *testing.T) {
	e := imageEnv()
	rec := e.postImage("u1", "u2", strings.Repeat("n", maxNonce+1), testPNG(t, 4, 4))
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "nonce_too_long" {
		t.Errorf("got %d %s, want 400 nonce_too_long", rec.Code, rec.Body)
	}
	nothingHappened(t, e)
	if rec := e.postImage("u1", "u2", strings.Repeat("n", maxNonce), testPNG(t, 4, 4)); rec.Code != http.StatusCreated {
		t.Errorf("a nonce of exactly %d characters = %d, want 201", maxNonce, rec.Code)
	}
}

// A retried upload must not post the picture twice, and must not leave the bytes of the
// attempt that lost behind: the repeat is a 200 with the message that is already there,
// pushes nothing, and the only object is the first one's.
func TestARepeatUploadUnderTheSameNonceIsTheSameMessageAndLeavesNoStrayBytes(t *testing.T) {
	e := imageEnv()
	data := testPNG(t, 16, 9)

	first := e.postImage("u1", "u2", "retry-1", data)
	if first.Code != http.StatusCreated {
		t.Fatal(first.Code, first.Body)
	}
	e.ln.pushes = nil

	again := e.postImage("u1", "u2", "retry-1", data)
	if again.Code != http.StatusOK {
		t.Fatalf("repeat = %d %s, want 200", again.Code, again.Body)
	}
	a, b := decode[sendResponse](t, first), decode[sendResponse](t, again)
	if a.ID != b.ID || a.Nonce != "retry-1" || b.Nonce != "retry-1" {
		t.Errorf("first %s, repeat %s", a.ID, b.ID)
	}
	if len(e.db.msgs) != 1 {
		t.Errorf("messages = %d, want 1", len(e.db.msgs))
	}
	if len(e.ln.pushes) != 0 {
		t.Errorf("a repeat pushed %d frames", len(e.ln.pushes))
	}
	if keys := e.objs.keys(); len(keys) != 1 || keys[0] != "messages/"+a.ID+"/image" {
		t.Errorf("objects = %v, want only the first upload's", keys)
	}
	if len(e.objs.puts) != 2 || len(e.objs.removes) != 1 || e.objs.removes[0] == "messages/"+a.ID+"/" {
		t.Errorf("puts %v removes %v: the second attempt's bytes should have been put and then removed, and not the first's", e.objs.puts, e.objs.removes)
	}

	// The same nonce to someone else is a different message.
	other := e.postImage("u1", "u3", "retry-1", data)
	if other.Code != http.StatusCreated || decode[sendResponse](t, other).ID == a.ID {
		t.Errorf("the same nonce to another recipient = %d %s, want a new message", other.Code, other.Body)
	}
}

func TestARepeatUploadIsStillFriendGated(t *testing.T) {
	e := imageEnv()
	e.postImage("u1", "u2", "n", testPNG(t, 4, 4))
	e.db.friends = map[[2]string]bool{} // no longer friends
	e.ln.pushes = nil
	rec := e.postImage("u1", "u2", "n", testPNG(t, 4, 4))
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "recipient_not_found" {
		t.Errorf("got %d %s, want 404", rec.Code, rec.Body)
	}
}

// Bytes go first, then the row. If the row fails to land, the bytes are removed, and
// nothing is pushed: they would otherwise be waste that nothing ever points at.
func TestAFailedInsertRemovesTheBytes(t *testing.T) {
	e := imageEnv()
	e.db.failOnQuery = insertMessageSQL

	rec := e.postImage("u1", "u2", "", testPNG(t, 8, 8))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d %s, want 500", rec.Code, rec.Body)
	}
	if len(e.objs.puts) != 1 {
		t.Fatalf("puts = %v: the bytes go first, and only once", e.objs.puts)
	}
	if len(e.objs.objs) != 0 {
		t.Errorf("objects left behind: %v", e.objs.keys())
	}
	if len(e.db.msgs) != 0 || len(e.ln.pushes) != 0 {
		t.Error("a failed upload stored or pushed something")
	}
	// The removal is of exactly the prefix that was put under, and of nothing broader.
	wantPrefix := strings.TrimSuffix(e.objs.puts[0], "image")
	if len(e.objs.removes) != 1 || e.objs.removes[0] != wantPrefix || !strings.HasSuffix(wantPrefix, "/") {
		t.Errorf("removes = %v, want [%s]", e.objs.removes, wantPrefix)
	}
}

// Failing to clean up is logged and nothing more: the upload was already failing, and
// the person gets the same answer.
func TestAFailedInsertWhoseCleanupAlsoFailsIsStillJustA500(t *testing.T) {
	e := imageEnv()
	e.db.failOnQuery = insertMessageSQL
	e.objs.removeErr = errors.New("bucket unreachable")

	rec := e.postImage("u1", "u2", "", testPNG(t, 8, 8))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d %s, want 500", rec.Code, rec.Body)
	}
	if len(e.objs.removes) != 1 {
		t.Errorf("removes = %v, want one attempt", e.objs.removes)
	}
}

// An error from the commit does not say the commit failed. When the row may have
// landed, removing the bytes would make a broken picture, which is the failure the order
// exists to avoid; so they stay - whether the row is known to be there, or cannot be
// looked for.
func TestACommitWhoseAnswerWasLostDoesNotRemoveBytesTheRowMayNeed(t *testing.T) {
	t.Run("the row is there", func(t *testing.T) {
		e := imageEnv()
		e.db.commitErr = errors.New("connection reset")
		rec := e.postImage("u1", "u2", "", testPNG(t, 8, 8))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", rec.Code)
		}
		if len(e.db.msgs) != 1 || len(e.objs.objs) != 1 || len(e.objs.removes) != 0 {
			t.Errorf("rows %d, objects %v, removes %v: the bytes of a stored message must stay", len(e.db.msgs), e.objs.keys(), e.objs.removes)
		}
	})
	t.Run("the row cannot be looked for", func(t *testing.T) {
		e := imageEnv()
		e.db.commitErr = errors.New("connection reset")
		e.db.failOnQuery = messageExistsSQL
		rec := e.postImage("u1", "u2", "", testPNG(t, 8, 8))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", rec.Code)
		}
		if len(e.objs.objs) != 1 || len(e.objs.removes) != 0 {
			t.Errorf("objects %v, removes %v: with no way to know, the bytes stay", e.objs.keys(), e.objs.removes)
		}
	})
}

func TestAFailedPutStoresAndPushesNothing(t *testing.T) {
	e := imageEnv()
	e.objs.putErr = errors.New("bucket unreachable")
	rec := e.postImage("u1", "u2", "", testPNG(t, 8, 8))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d %s, want 500", rec.Code, rec.Body)
	}
	if len(e.db.msgs) != 0 || len(e.ln.pushes) != 0 {
		t.Error("a picture whose bytes did not land became a message")
	}
}

func TestSendImageWithNoObjectStoreIsA500NotAPanic(t *testing.T) {
	e := imageEnv()
	e.h.objects = nil
	if rec := e.postImage("u1", "u2", "", testPNG(t, 8, 8)); rec.Code != http.StatusInternalServerError {
		t.Errorf("got %d, want 500", rec.Code)
	}
	if len(e.db.msgs) != 0 {
		t.Error("stored a message with nowhere to keep its picture")
	}
}

// A client still cannot declare a kind: the picture endpoint does not read one, and
// POST /messages still refuses it. The endpoint mints an image from bytes it looked at.
func TestAClientStillCannotDeclareAnImageKindOrPayload(t *testing.T) {
	e := imageEnv()
	for _, extra := range []map[string]any{
		{"kind": "image"},
		{"kind": "image", "payload": map[string]any{"width": 1, "height": 1, "mime": "image/png", "size": 1}},
		{"payload": map[string]any{"width": 1}},
	} {
		fields := map[string]any{"recipient_id": "u2", "body": "hi"}
		for k, v := range extra {
			fields[k] = v
		}
		rec := e.do("u1", http.MethodPost, "/messages", jsonBody(t, fields))
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_body" {
			t.Errorf("%v: got %d %s, want 400 invalid_body", extra, rec.Code, rec.Body)
		}
	}
	// And query parameters of the same names on the picture endpoint do nothing.
	body, contentType := multipartBody(filePart(testPNG(t, 6, 5)))
	rec := e.postBody("u1", "to=u2&kind=text&payload=%7B%7D&width=1&height=1", body, contentType)
	if rec.Code != http.StatusCreated {
		t.Fatal(rec.Code, rec.Body)
	}
	if m := e.db.msgs[0]; m.Kind != kindImage || !strings.Contains(string(m.Payload), `"width":6`) {
		t.Errorf("stored = %+v: the server's own measurement is the only one", m)
	}
}

// ---- reading ------------------------------------------------------------------

func TestBothPeopleCanReadThePictureAndGetItsBytesAndType(t *testing.T) {
	e := imageEnv()
	data := testJPEG(t, 24, 12)
	id := sentImage(t, e, data)

	for _, who := range []string{"u1", "u2"} {
		rec := e.getImage(who, id)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d %s", who, rec.Code, rec.Body)
		}
		if !bytes.Equal(rec.Body.Bytes(), data) {
			t.Errorf("%s: the bytes differ", who)
		}
		h := rec.Header()
		if h.Get("Content-Type") != "image/jpeg" || h.Get("Content-Length") != strconv.Itoa(len(data)) ||
			h.Get("ETag") == "" || h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers = %v", who, h)
		}
	}
}

// Everything that is not "one of the two people, still friends, reading a live image"
// is the SAME 404, to the byte, so an id cannot be probed: a stranger, an id that
// names nothing, a deleted message, a message that is not an image, and somebody who
// is no longer a friend. None of them reaches the object store.
func TestEveryRefusalToReadAPictureIsTheSameOpaque404(t *testing.T) {
	e := imageEnv()
	id := sentImage(t, e, testPNG(t, 8, 8))
	e.db.seed("text1", "u1", "u2", "just words")
	deleted := sentImage(t, e, testPNG(t, 9, 9))
	if rec := e.do("u1", http.MethodDelete, "/messages/"+deleted, ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	e.objs.gets = nil

	nobody := e.getImage("u2", "no-such-message")
	if nobody.Code != http.StatusNotFound || errorCode(t, nobody) != "message_not_found" {
		t.Fatalf("unknown id = %d %s", nobody.Code, nobody.Body)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"a third party (a friend of the sender)": e.getImage("u3", id),
		"a stranger":                             e.getImage("ghost", id),
		"a deleted message":                      e.getImage("u2", deleted),
		"the sender of a deleted message":        e.getImage("u1", deleted),
		"a text message":                         e.getImage("u2", "text1"),
	} {
		if rec.Code != http.StatusNotFound || rec.Body.String() != nobody.Body.String() {
			t.Errorf("%s: %d %q differs from the unknown id's %d %q", name, rec.Code, rec.Body, nobody.Code, nobody.Body)
		}
	}

	// Unfriended: the thread is gone from history and the catch-up feed, so is the picture.
	e.db.friends = map[[2]string]bool{}
	for _, who := range []string{"u1", "u2"} {
		if rec := e.getImage(who, id); rec.Code != http.StatusNotFound || rec.Body.String() != nobody.Body.String() {
			t.Errorf("%s after unfriending: %d %q", who, rec.Code, rec.Body)
		}
	}
	if len(e.objs.gets) != 0 {
		t.Errorf("a refused read reached the object store: %v", e.objs.gets)
	}
}

// The id is the version of the bytes - nothing rewrites them - and a conditional
// request is answered only after it has been authorised.
func TestAConditionalReadIsAnswered304OnlyForSomeoneAllowedToRead(t *testing.T) {
	e := imageEnv()
	id := sentImage(t, e, testPNG(t, 8, 8))
	etag := e.getImage("u2", id).Header().Get("ETag")
	e.objs.gets = nil

	if rec := e.getImage("u2", id, "If-None-Match", etag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("got %d with %d bytes, want an empty 304", rec.Code, rec.Body.Len())
	}
	if len(e.objs.gets) != 0 {
		t.Error("a 304 read the object")
	}
	if rec := e.getImage("u3", id, "If-None-Match", etag); rec.Code != http.StatusNotFound {
		t.Errorf("a third party with the right ETag = %d, want 404", rec.Code)
	}
}

// A row with no bytes behind it is a fault, not "not found": the person may read this
// picture, and saying it does not exist would hide a bucket outage as a deletion.
func TestAMissingObjectIsA500AndNotA404(t *testing.T) {
	e := imageEnv()
	id := sentImage(t, e, testPNG(t, 8, 8))
	e.objs.objs = map[string][]byte{}
	if rec := e.getImage("u2", id); rec.Code != http.StatusInternalServerError {
		t.Errorf("got %d %s, want 500", rec.Code, rec.Body)
	}
}

// ---- deleting -----------------------------------------------------------------

// A delete clears the payload and keeps the kind, as it always has, and now takes the
// bytes with it. The picture stops being served.
func TestDeletingAnImageMessageRemovesItsBytes(t *testing.T) {
	e := imageEnv()
	id := sentImage(t, e, testPNG(t, 8, 8))
	e.ln.pushes = nil
	e.objs.removes = nil

	if rec := e.do("u1", http.MethodDelete, "/messages/"+id, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	m := e.db.msgs[0]
	if m.DeletedAt == nil || m.Payload != nil || m.Kind != kindImage || m.Body != "" {
		t.Errorf("stored = %+v, want a tombstone that is still an image and carries nothing", m)
	}
	if len(e.objs.objs) != 0 || len(e.objs.removes) != 1 || e.objs.removes[0] != "messages/"+id+"/" {
		t.Errorf("objects %v, removes %v", e.objs.keys(), e.objs.removes)
	}
	_, frame := e.ln.frame(t)
	msg := frame["message"].(map[string]any)
	if _, has := msg["payload"]; has || msg["kind"] != "image" || frame["op"] != "delete" {
		t.Errorf("the delete frame says %v", frame)
	}
	for _, who := range []string{"u1", "u2"} {
		if rec := e.getImage(who, id); rec.Code != http.StatusNotFound {
			t.Errorf("%s reading a deleted picture = %d, want 404", who, rec.Code)
		}
	}
}

// The row is the commit point. If the bytes cannot be removed the delete still
// succeeds - refusing it for a bucket being briefly down would be refusing the one
// thing the person asked for - and the message is a tombstone either way.
func TestADeleteSucceedsEvenWhenTheBytesCannotBeRemoved(t *testing.T) {
	e := imageEnv()
	id := sentImage(t, e, testPNG(t, 8, 8))
	e.objs.removeErr = errors.New("bucket unreachable")

	if rec := e.do("u1", http.MethodDelete, "/messages/"+id, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s, want 204", rec.Code, rec.Body)
	}
	if e.db.msgs[0].DeletedAt == nil {
		t.Error("the message was not deleted")
	}
	// ...and the row being gone is what stops it being served, whatever the bucket holds.
	if rec := e.getImage("u2", id); rec.Code != http.StatusNotFound {
		t.Errorf("a tombstone served its picture: %d", rec.Code)
	}
}

// Deleting is the sender's alone and a second delete is the same 404 as ever; neither
// touches the object store.
func TestOnlyTheSenderDeletesAPictureAndOnlyOnce(t *testing.T) {
	e := imageEnv()
	id := sentImage(t, e, testPNG(t, 8, 8))
	e.objs.removes = nil

	if rec := e.do("u2", http.MethodDelete, "/messages/"+id, ""); rec.Code != http.StatusNotFound {
		t.Errorf("the recipient deleting = %d, want 404", rec.Code)
	}
	if len(e.objs.removes) != 0 || len(e.objs.objs) != 1 {
		t.Error("a refused delete touched the object store")
	}
	e.do("u1", http.MethodDelete, "/messages/"+id, "")
	e.objs.removes = nil
	if rec := e.do("u1", http.MethodDelete, "/messages/"+id, ""); rec.Code != http.StatusNotFound {
		t.Errorf("deleting twice = %d, want 404", rec.Code)
	}
	if len(e.objs.removes) != 0 {
		t.Error("a repeated delete touched the object store")
	}
}

// Only a picture has bytes to remove: deleting words, or a card, asks the store for
// nothing.
func TestDeletingAnythingButAPictureNeverTouchesTheObjectStore(t *testing.T) {
	e := imageEnv()
	e.db.seed("t1", "u1", "u2", "words")
	if err := e.h.MintInvite(context.Background(), "inv1", "room1", "umber", "u1", "u2"); err != nil {
		t.Fatal(err)
	}
	invite := e.db.msgs[1].ID
	for _, id := range []string{"t1", invite} {
		if rec := e.do("u1", http.MethodDelete, "/messages/"+id, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("delete %s = %d", id, rec.Code)
		}
	}
	if len(e.objs.removes) != 0 {
		t.Errorf("removes = %v, want none", e.objs.removes)
	}
}

// ---- what a picture is, among messages --------------------------------------------

// It can be pinned, replied to and reacted to like any other message.
func TestAnImageMessageCanBePinnedRepliedToAndReactedTo(t *testing.T) {
	e := imageEnv()
	id := sentImage(t, e, testPNG(t, 8, 8))

	if rec := e.pin("u2", id); rec.Code != http.StatusOK {
		t.Errorf("pin = %d %s", rec.Code, rec.Body)
	}
	if rec := reactAs(t, e, "u2", id, "👍"); rec.Code != http.StatusOK {
		t.Errorf("react = %d %s", rec.Code, rec.Body)
	}
	rec := sendAs(t, e, "u2", "u1", "nice", map[string]any{"reply_to_id": id})
	if rec.Code != http.StatusCreated {
		t.Fatalf("reply = %d %s", rec.Code, rec.Body)
	}
	if rp := replyOf(t, rec); rp == nil || rp.ID != id || rp.Deleted || rp.Body != "" {
		t.Errorf("reply_to = %+v", rp)
	}
}

// A picture reaches an offline client through the catch-up feed like everything else.
func TestAnImageMessageIsInTheCatchUpFeedWithItsPayload(t *testing.T) {
	e := imageEnv()
	sentImage(t, e, testPNG(t, 8, 8))
	feed := asJSONBody(t, e.do("u2", http.MethodGet, "/messages?since=0", ""))
	m := feed["messages"].([]any)[0].(map[string]any)
	pl, _ := m["payload"].(map[string]any)
	if m["kind"] != "image" || pl["mime"] != "image/png" {
		t.Errorf("catch-up says %v", m)
	}
}

// ---- the rules of the payload ---------------------------------------------------

func TestCheckKindForAnImagePayload(t *testing.T) {
	good := imagePayload{Width: 1280, Height: 720, Mime: "image/png", Size: 184320}
	enc := func(mod func(p *imagePayload)) string {
		p := good
		if mod != nil {
			mod(&p)
		}
		b, _ := json.Marshal(p)
		return string(b)
	}
	cases := []struct {
		name    string
		payload string
		ok      bool
	}{
		{"a good one", enc(nil), true},
		{"the largest allowed", enc(func(p *imagePayload) { p.Width, p.Height, p.Size = 10000, 10000, maxImageBytes }), true},
		{"one pixel too many", enc(func(p *imagePayload) { p.Width, p.Height = 10001, 10000 }), false},
		{"no payload", "", false},
		{"null", "null", false},
		{"not an object", `[1,2]`, false},
		{"no width", `{"height":1,"mime":"image/png","size":1}`, false},
		{"no height", `{"width":1,"mime":"image/png","size":1}`, false},
		{"zero width", enc(func(p *imagePayload) { p.Width = 0 }), false},
		{"negative height", enc(func(p *imagePayload) { p.Height = -5 }), false},
		{"a width that is not whole", `{"width":1.5,"height":1,"mime":"image/png","size":1}`, false},
		{"a mime that is not on the list", enc(func(p *imagePayload) { p.Mime = "image/svg+xml" }), false},
		{"no mime", enc(func(p *imagePayload) { p.Mime = "" }), false},
		{"no size", enc(func(p *imagePayload) { p.Size = 0 }), false},
		{"over the cap", enc(func(p *imagePayload) { p.Size = maxImageBytes + 1 }), false},
		{"an id of its own (the id is the message's)", `{"width":1,"height":1,"mime":"image/png","size":1,"id":"m1"}`, false},
		{"a field of its own invention", `{"width":1,"height":1,"mime":"image/png","size":1,"url":"x"}`, false},
		{"a second document after it", enc(nil) + enc(nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkKind(kindImage, "", []byte(tc.payload))
			if tc.ok && err != nil {
				t.Errorf("refused: %v", err)
			}
			if !tc.ok && !errors.Is(err, errBadPayload) {
				t.Errorf("err = %v, want errBadPayload", err)
			}
		})
	}
}

// A text message and a picture's caption have words; a card does not. A card with a
// body is text the server wrote on somebody's behalf, and search would index it.
func TestOnlyAMessageWithWordsMayHaveABody(t *testing.T) {
	image := []byte(`{"width":1,"height":1,"mime":"image/png","size":1}`)
	invite := []byte(`{"invite_id":"i","room":"r","room_name":"n"}`)
	for _, tc := range []struct {
		name    string
		kind    string
		body    string
		payload []byte
		ok      bool
	}{
		{"an image with a caption", kindImage, "a caption", image, true},
		{"an image with no words", kindImage, "", image, true},
		{"text with words", kindText, "words", nil, true},
		{"an invite with no words", kindInvite, "", invite, true},
		{"an invite with a body", kindInvite, "words", invite, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newFakeDB("a", "b")
			_, err := insertMessage(context.Background(), db, &messageRecord{ID: "m1", SenderID: "a", RecipientID: "b", Kind: tc.kind, Payload: tc.payload, Body: tc.body})
			if tc.ok && err != nil || !tc.ok && !errors.Is(err, errBadPayload) {
				t.Errorf("err = %v, want ok=%v", err, tc.ok)
			}
			if wrote := len(db.msgs) == 1; wrote != tc.ok {
				t.Errorf("rows = %d, want a row only when it is allowed", len(db.msgs))
			}
		})
	}
}

// ---- where the bytes live ---------------------------------------------------------

// The key is the message's id and nothing else; the prefix ends in a slash so removing
// one message cannot remove another whose id merely starts the same way.
func TestTheObjectKeyIsTheMessagesOwnID(t *testing.T) {
	if got := imageKey("01JABC"); got != "messages/01JABC/image" {
		t.Errorf("imageKey = %q", got)
	}
	if got := ImagePrefix("01JABC"); got != "messages/01JABC/" {
		t.Errorf("ImagePrefix = %q", got)
	}
	if !strings.HasPrefix(imageKey("01JABC"), ImagePrefix("01JABC")) {
		t.Error("the object is not under its own prefix, so removing the prefix would not remove it")
	}
	if strings.HasPrefix(imageKey("01JABCD"), ImagePrefix("01JABC")) {
		t.Error("one message's prefix covers another message's key")
	}
}

func TestSQLMessageExistsLooksAtTheMessagesTableByID(t *testing.T) {
	mustContain(t, "messageExistsSQL", messageExistsSQL, "FROM messages WHERE id = $1")
	mustNotContain(t, "messageExistsSQL", messageExistsSQL, "deleted_at")
}

// Only a message whose kind is image is ever served, whatever else it holds. A text
// message that somehow carried an image's payload, with bytes sitting at its key, must
// not be readable as a picture.
func TestOnlyAnImageKindIsEverServed(t *testing.T) {
	e := imageEnv()
	data := testPNG(t, 8, 8)
	for _, kind := range []string{kindText, kindInvite} {
		id := "not-an-image-" + kind
		m := e.db.seed(id, "u1", "u2", "")
		e.db.find(m.ID).Kind = kind
		e.db.find(m.ID).Payload = []byte(`{"width":8,"height":8,"mime":"image/png","size":` + strconv.Itoa(len(data)) + `}`)
		e.objs.objs[imageKey(id)] = data
	}
	for _, kind := range []string{kindText, kindInvite} {
		if rec := e.getImage("u2", "not-an-image-"+kind); rec.Code != http.StatusNotFound {
			t.Errorf("a %s message was served as a picture: %d", kind, rec.Code)
		}
	}
	if len(e.objs.gets) != 0 {
		t.Errorf("the object store was asked for %v", e.objs.gets)
	}
}

// A conditional request that does not match gets the picture, in full.
func TestAConditionalReadThatDoesNotMatchGetsTheBytes(t *testing.T) {
	e := imageEnv()
	data := testPNG(t, 8, 8)
	id := sentImage(t, e, data)
	for _, inm := range []string{`"some-other-id"`, `W/"` + id + `"x`, "*"} {
		rec := e.getImage("u2", id, "If-None-Match", inm)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), data) {
			t.Errorf("If-None-Match %s = %d with %d bytes, want 200 and the picture", inm, rec.Code, rec.Body.Len())
		}
	}
}
