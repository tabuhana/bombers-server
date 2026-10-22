package messaging

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
)

// A caption is the picture's body: words a person typed, held to the rule a text body
// is held to, in the same request as the bytes. These tests are the other half of
// image_test.go - what the words do - and use its harness.

// multipartWriter is a multipart writer onto b and the Content-Type naming its boundary,
// for the tests that build a body part by part instead of through multipartBody.
func multipartWriter(b *bytes.Buffer) (*multipart.Writer, string) {
	w := multipart.NewWriter(b)
	return w, w.FormDataContentType()
}

func capturedBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d %s, want 201", rec.Code, rec.Body)
	}
	return decode[sendResponse](t, rec).Body
}

// ---- sending ----------------------------------------------------------------

// One request is ONE message: the picture and its words in one row, announced in one
// frame to each person. A caption that arrived as a second write would be two rows and
// a caption appearing a beat after the picture.
func TestACaptionTravelsWithThePictureAsOneMessage(t *testing.T) {
	e := imageEnv()
	data := testPNG(t, 37, 21)

	rec := e.postCaptioned("u1", "u2", "n1", data, "the view from the roof")
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d %s, want 201", rec.Code, rec.Body)
	}
	got := decode[sendResponse](t, rec)
	if got.Kind != "image" || got.Body != "the view from the roof" || got.Nonce != "n1" {
		t.Errorf("response = %+v", got)
	}

	if len(e.db.msgs) != 1 || e.db.msgs[0].Body != "the view from the roof" || e.db.msgs[0].Kind != kindImage {
		t.Fatalf("stored = %+v, want ONE image message carrying the caption", e.db.msgs)
	}
	if keys := e.objs.keys(); len(keys) != 1 || keys[0] != "messages/"+got.ID+"/image" {
		t.Errorf("objects = %v", keys)
	}
	// The payload is still only the picture's own: the words are the body, not a field of it.
	if p := string(e.db.msgs[0].Payload); strings.Contains(p, "caption") || strings.Contains(p, "roof") {
		t.Errorf("payload = %s: the caption belongs in the body", p)
	}

	// One frame per person, each carrying the picture and its words.
	if len(e.ln.pushes) != 2 {
		t.Fatalf("pushes = %d, want 2 (one frame per person)", len(e.ln.pushes))
	}
	for who, frame := range e.ln.frames(t) {
		msg := frame["message"].(map[string]any)
		if frame["op"] != "new" || msg["body"] != "the view from the roof" || msg["kind"] != "image" || msg["payload"] == nil {
			t.Errorf("%s was told %v", who, frame)
		}
	}

	// And it reads back the same from history and the catch-up feed.
	hist := asJSONBody(t, e.do("u2", http.MethodGet, "/messages/u1", ""))
	if m := hist["messages"].([]any)[0].(map[string]any); m["body"] != "the view from the roof" || m["kind"] != "image" {
		t.Errorf("history says %v", m)
	}
	feed := asJSONBody(t, e.do("u2", http.MethodGet, "/messages?since=0", ""))
	if m := feed["messages"].([]any)[0].(map[string]any); m["body"] != "the view from the roof" {
		t.Errorf("catch-up says %v", m)
	}
}

// A picture with no words is still a picture with no words: an absent caption, an empty
// one, one of whitespace and one of nothing but the bytes plainText drops are all "".
// None of them is the empty_body refusal that the same words would be as a text.
func TestAPictureWithoutWordsHasAnEmptyBody(t *testing.T) {
	png := testPNG(t, 8, 8)
	for name, parts := range map[string][]formPart{
		"no caption part":        {filePart(png)},
		"an empty caption":       {filePart(png), captionPart("")},
		"whitespace":             {filePart(png), captionPart("  \t\r\n  ")},
		"only NUL and bad bytes": {filePart(png), captionPart("\x00\xff\x00")},
	} {
		t.Run(name, func(t *testing.T) {
			e := imageEnv()
			if body := capturedBody(t, e.postParts("u1", "u2", "", parts...)); body != "" {
				t.Errorf("body = %q, want empty", body)
			}
			if e.db.msgs[0].Body != "" {
				t.Errorf("stored body = %q", e.db.msgs[0].Body)
			}
		})
	}
}

// The caption is cleaned by the rule a text body is: trimmed, and what Postgres refuses
// in a text parameter taken out BEFORE anything is counted, so no caption anyone can
// type is a 500.
func TestACaptionIsCleanedLikeABody(t *testing.T) {
	png := testPNG(t, 8, 8)
	for name, tc := range map[string]struct{ in, want string }{
		"trimmed":              {"  hello  ", "hello"},
		"a NUL inside":         {"he\x00llo", "hello"},
		"invalid UTF-8 inside": {"he\xffl\xc3lo", "hello"},
		"inner spacing kept":   {"a  b\nc", "a  b\nc"},
	} {
		t.Run(name, func(t *testing.T) {
			e := imageEnv()
			if body := capturedBody(t, e.postCaptioned("u1", "u2", "", png, tc.in)); body != tc.want {
				t.Errorf("body = %q, want %q", body, tc.want)
			}
		})
	}
}

// 4000 characters is a caption and 4001 is not, counted in characters and not bytes, and
// counted after the bytes plainText drops are gone: the same line a text body is held to.
func TestACaptionIsHeldToTheLengthOfABody(t *testing.T) {
	png := testPNG(t, 8, 8)
	for name, tc := range map[string]struct {
		caption string
		want    int
	}{
		"4000 characters":                         {strings.Repeat("a", 4000), http.StatusCreated},
		"4000 four-byte characters (16000 bytes)": {strings.Repeat("😀", 4000), http.StatusCreated},
		"4000 characters and 100 NULs":            {strings.Repeat("a", 4000) + strings.Repeat("\x00", 100), http.StatusCreated},
		"4000 characters padded with spaces":      {"   " + strings.Repeat("a", 4000) + "   ", http.StatusCreated},
		"4001 characters":                         {strings.Repeat("a", 4001), http.StatusBadRequest},
		"4001 four-byte characters":               {strings.Repeat("😀", 4001), http.StatusBadRequest},
		"past what a part is read to":             {strings.Repeat("a", maxCaptionBytes+1), http.StatusBadRequest},
		// Past the read cap, so what is left after plainText must not be taken for the
		// whole caption: the part is refused, not silently cut at the cap.
		"NULs past the cap, then words": {strings.Repeat("\x00", maxCaptionBytes) + "words", http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			e := imageEnv()
			rec := e.postCaptioned("u1", "u2", "", png, tc.caption)
			if rec.Code != tc.want {
				t.Fatalf("got %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.want == http.StatusBadRequest {
				if errorCode(t, rec) != "body_too_long" {
					t.Errorf("code = %s, want body_too_long (the answer a text body gets)", errorCode(t, rec))
				}
				nothingHappened(t, e)
			}
		})
	}
}

// The caption is checked by the same rule as the text, so the same words get the same
// verdict on both doors.
func TestACaptionAndATextBodyGetTheSameVerdict(t *testing.T) {
	png := testPNG(t, 8, 8)
	for _, words := range []string{"hi", " spaced ", strings.Repeat("a", 4000), strings.Repeat("a", 4001), "a\x00b", "   "} {
		e := imageEnv()
		text := e.do("u1", http.MethodPost, "/messages", jsonBody(t, map[string]any{"recipient_id": "u2", "body": words}))
		pic := e.postCaptioned("u1", "u2", "", png, words)
		switch {
		case text.Code == http.StatusCreated:
			if pic.Code != http.StatusCreated || decode[sendResponse](t, pic).Body != decode[sendResponse](t, text).Body {
				t.Errorf("%q: text stored %q, picture answered %d %s", words, decode[sendResponse](t, text).Body, pic.Code, pic.Body)
			}
		case errorCode(t, text) == "empty_body":
			// Words that are nothing: a text is refused, a picture simply has no caption.
			if pic.Code != http.StatusCreated || decode[sendResponse](t, pic).Body != "" {
				t.Errorf("%q: picture answered %d %s", words, pic.Code, pic.Body)
			}
		default:
			if pic.Code != text.Code || errorCode(t, pic) != errorCode(t, text) {
				t.Errorf("%q: text answered %d %s, picture %d %s", words, text.Code, text.Body, pic.Code, pic.Body)
			}
		}
	}
}

// Parts come in either order.
func TestTheCaptionMayComeBeforeTheFile(t *testing.T) {
	e := imageEnv()
	rec := e.postParts("u1", "u2", "", captionPart("first"), filePart(testPNG(t, 8, 8)))
	if body := capturedBody(t, rec); body != "first" {
		t.Errorf("body = %q", body)
	}
}

// A mention in a caption is a mention in a body: found, vouched for per reader, and
// not stored.
func TestAMentionInACaptionIsResolvedPerReader(t *testing.T) {
	e := imageEnv() // u1 is friends with u2 and u3; u2 and u3 are not friends
	rec := e.postCaptioned("u1", "u2", "", testPNG(t, 8, 8), "look <@u3> and <@u2>")
	if rec.Code != http.StatusCreated {
		t.Fatal(rec.Code, rec.Body)
	}
	if got := decode[sendResponse](t, rec).Mentions; len(got) != 2 || got[0] != "u3" || got[1] != "u2" {
		t.Errorf("the sender is told %v, want [u3 u2]", got)
	}
	// The recipient is not a friend of u3, so is not told of them: one message, two readers.
	frames := e.ln.frames(t)
	msg := frames["u2"]["message"].(map[string]any)
	if got := msg["mentions"].([]any); len(got) != 1 || got[0] != "u2" {
		t.Errorf("the recipient is told %v, want just themself", got)
	}
}

// The words of a caption are searchable like any other body.
func TestACaptionIsFoundBySearch(t *testing.T) {
	e := imageEnv()
	id := decode[sendResponse](t, e.postCaptioned("u1", "u2", "", testPNG(t, 8, 8), "sunset over the harbour")).ID
	if ids := searchIDs(t, e.search("u2", "harbour", "")); len(ids) != 1 || ids[0] != id {
		t.Errorf("search found %v, want the captioned picture %s", ids, id)
	}
	// And a tombstone has no words left to find.
	if rec := e.do("u1", http.MethodDelete, "/messages/"+id, ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if ids := searchIDs(t, e.search("u2", "harbour", "")); len(ids) != 0 {
		t.Errorf("a deleted picture's caption was found: %v", ids)
	}
}

// A repeat under the same nonce is the message already stored - its caption included,
// whatever the repeat says - and the bytes of the attempt that lost are thrown away.
func TestARepeatUploadReturnsTheStoredCaption(t *testing.T) {
	e := imageEnv()
	png := testPNG(t, 8, 8)
	first := e.postCaptioned("u1", "u2", "retry", png, "the first words")
	if first.Code != http.StatusCreated {
		t.Fatal(first.Code, first.Body)
	}
	e.ln.pushes = nil
	again := e.postCaptioned("u1", "u2", "retry", png, "different words")
	if again.Code != http.StatusOK || decode[sendResponse](t, again).Body != "the first words" {
		t.Errorf("repeat = %d %s, want 200 and the original caption", again.Code, again.Body)
	}
	if len(e.db.msgs) != 1 || len(e.ln.pushes) != 0 || len(e.objs.objs) != 1 {
		t.Errorf("rows %d, pushes %d, objects %v", len(e.db.msgs), len(e.ln.pushes), e.objs.keys())
	}
}

// A caption is validated BEFORE the bytes are put: a refused caption leaves nothing in
// the store, which is what nothingHappened checks above; and the order of two failures
// does not depend on which part came first.
func TestTheVerdictDoesNotDependOnThePartOrder(t *testing.T) {
	long := captionPart(strings.Repeat("a", 4001))
	notPicture := filePart([]byte("not a picture"))
	for name, parts := range map[string][]formPart{
		"file first":    {notPicture, long},
		"caption first": {long, notPicture},
	} {
		t.Run(name, func(t *testing.T) {
			e := imageEnv()
			rec := e.postParts("u1", "u2", "", parts...)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "body_too_long" {
				t.Errorf("got %d %s, want the caption's 400 body_too_long both ways", rec.Code, rec.Body)
			}
			nothingHappened(t, e)
		})
	}
}

// ---- the request itself -------------------------------------------------------

// Whatever is wrong with the shape of the request is a 400 invalid_body - the code
// POST /messages answers a body it does not take with - and nothing is stored or put.
func TestAnUploadThatIsNotWellFormedIsInvalidBody(t *testing.T) {
	png := testPNG(t, 8, 8)
	good, goodType := multipartBody(filePart(png), captionPart("words"))
	boundary := goodType[strings.Index(goodType, "boundary="):]

	cases := map[string]struct {
		body        []byte
		contentType string
	}{
		// What the endpoint used to take is refused, not misread as something else.
		"the old raw bytes, labelled as a png": {png, "image/png"},
		"raw bytes with no label":              {png, ""},
		"multipart with no boundary":           {good, "multipart/form-data"},
		"not form-data":                        {good, "multipart/mixed; " + boundary},
		"garbage under a boundary":             {[]byte("this is not multipart"), goodType},
		"an empty body":                        {nil, goodType},
		"cut off before the closing boundary":  {good[:len(good)-12], goodType},
		// Every part complete and only the closing "--" missing: the parts are not all
		// there is, and the body is still cut short.
		"cut off after the last boundary line":   {good[:len(good)-4], goodType},
		"cut off in the middle of the picture":   {good[:len(good)/2], goodType},
		"cut off in the middle of a part header": {good[:60], goodType},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := imageEnv()
			rec := e.postBody("u1", imageQuery("u2", ""), tc.body, tc.contentType)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_body" {
				t.Errorf("got %d %s, want 400 invalid_body", rec.Code, rec.Body)
			}
			nothingHappened(t, e)
		})
	}
}

func TestAnUploadWithoutAFileIsInvalidBody(t *testing.T) {
	png := testPNG(t, 8, 8)
	for name, parts := range map[string][]formPart{
		"no parts at all":       {},
		"only a caption":        {captionPart("words")},
		"the file misnamed":     {{name: "image", filename: "pic", data: png}},
		"a file part by case":   {{name: "File", filename: "pic", data: png}},
		"a caption and no file": {captionPart("words"), {name: "attachment", data: png}},
	} {
		t.Run(name, func(t *testing.T) {
			e := imageEnv()
			rec := e.postParts("u1", "u2", "", parts...)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_body" {
				t.Errorf("got %d %s, want 400 invalid_body", rec.Code, rec.Body)
			}
			nothingHappened(t, e)
		})
	}
}

// Only the two parts are the vocabulary. A part this does not take is a refusal and not
// an ignored field - above all a `kind` or a `payload`, which a client may not declare
// here any more than on POST /messages - and a part named twice is ambiguous, so it is
// refused rather than resolved by taking the first or the last.
func TestAnUploadWithAnyOtherPartOrARepeatedOneIsInvalidBody(t *testing.T) {
	png := testPNG(t, 8, 8)
	for name, parts := range map[string][]formPart{
		"a kind":               {filePart(png), {name: "kind", data: []byte("text")}},
		"a payload":            {filePart(png), {name: "payload", data: []byte(`{"width":1}`)}},
		"a reply_to_id":        {filePart(png), {name: "reply_to_id", data: []byte("m1")}},
		"a body":               {filePart(png), {name: "body", data: []byte("words")}},
		"a nameless part":      {filePart(png), {data: []byte("x")}},
		"two files":            {filePart(png), filePart(png)},
		"two captions":         {filePart(png), captionPart("a"), captionPart("b")},
		"a kind before it all": {{name: "kind", data: []byte("invite")}, filePart(png)},
	} {
		t.Run(name, func(t *testing.T) {
			e := imageEnv()
			rec := e.postParts("u1", "u2", "", parts...)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_body" {
				t.Errorf("got %d %s, want 400 invalid_body", rec.Code, rec.Body)
			}
			nothingHappened(t, e)
		})
	}
}

// The file's own label, its filename and its encoding header are the client's word and
// the server does not take it: what is sniffed is the bytes that were sent.
func TestTheFilePartIsTakenRawWhateverItClaims(t *testing.T) {
	e := imageEnv()
	// A tail that quoted-printable would decode into something else ("=41" is "A", and
	// "=" at the end of a line is a soft break), which is exactly what taking the part
	// raw keeps from happening.
	png := append(testPNG(t, 9, 7), []byte("=41=\r\n=3D")...)

	var b bytes.Buffer
	b.WriteString("--XB\r\nContent-Disposition: form-data; name=\"file\"; filename=\"../../etc/passwd\"\r\n")
	b.WriteString("Content-Type: text/html\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	b.Write(png)
	b.WriteString("\r\n--XB--\r\n")
	rec := e.postBody("u1", imageQuery("u2", ""), b.Bytes(), "multipart/form-data; boundary=XB")
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	key := "messages/" + decode[sendResponse](t, rec).ID + "/image"
	if !bytes.Equal(e.objs.objs[key], png) {
		t.Error("the stored bytes are not the bytes that were sent")
	}
}

// ---- the caps -------------------------------------------------------------------

// A picture of exactly 10 MiB with a caption of the longest legal size - four thousand
// four-byte characters - is one request that is bigger than 10 MiB, and it is a picture.
func TestTheLargestPictureWithTheLongestCaptionFits(t *testing.T) {
	png := testPNG(t, 8, 8)
	exact := append(append([]byte(nil), png...), make([]byte, maxImageBytes-len(png))...)
	e := imageEnv()
	rec := e.postCaptioned("u1", "u2", "", exact, strings.Repeat("😀", 4000))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201", rec.Code)
	}
	if got := decode[sendResponse](t, rec).Body; len([]rune(got)) != 4000 {
		t.Errorf("caption has %d characters", len([]rune(got)))
	}
}

// The request as a whole is capped, whatever each part says: parts that are each within
// their own cap, with headers that push the request past the room left for them, are a
// 413 - not read to the end, and not a 500. The same request without the padding gets
// as far as the caption's own verdict, which shows it was the total that stopped it.
func TestTheWholeRequestIsCappedAsWellAsEachPart(t *testing.T) {
	png := testPNG(t, 8, 8)
	exact := append(append([]byte(nil), png...), make([]byte, maxImageBytes-len(png))...)
	build := func(pad int) ([]byte, string) {
		var b bytes.Buffer
		w, contentType := multipartWriter(&b)
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="file"; filename="pic"`)
		part, _ := w.CreatePart(h)
		part.Write(exact)
		h = textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="caption"`)
		if pad > 0 {
			h.Set("X-Pad", strings.Repeat("p", pad))
		}
		part, _ = w.CreatePart(h)
		part.Write(bytes.Repeat([]byte("a"), maxCaptionBytes))
		w.Close()
		return b.Bytes(), contentType
	}

	e := imageEnv()
	body, ct := build(0)
	if rec := e.postBody("u1", imageQuery("u2", ""), body, ct); rec.Code != http.StatusBadRequest || errorCode(t, rec) != "body_too_long" {
		t.Fatalf("unpadded: got %d %s, want the caption's own 400 body_too_long", rec.Code, rec.Body)
	}
	e2 := imageEnv()
	body, ct = build(maxImageRequestBytes - maxImageBytes - maxCaptionBytes)
	rec := e2.postBody("u1", imageQuery("u2", ""), body, ct)
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "message_too_large" {
		t.Errorf("padded past the whole cap: got %d %s, want 413 message_too_large", rec.Code, rec.Body)
	}
	nothingHappened(t, e2)
}

// ---- who may be sent to ----------------------------------------------------------

// probe is a request body that counts how much of it was read.
type probe struct {
	r io.Reader
	n int
}

func (p *probe) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += n
	return n, err
}

// The friendship gate is before the body: a stranger gets the opaque 404 and the server
// has not read a byte of what they sent - whatever it is, well-formed multipart, a
// ten-megabyte file or no multipart at all. A friend's identical request is read, which
// is what shows the probe is measuring something.
func TestTheGateRunsBeforeAnyOfTheMultipartIsRead(t *testing.T) {
	png := testPNG(t, 8, 8)
	big := append(append([]byte(nil), png...), make([]byte, maxImageBytes)...)
	formed, formedType := multipartBody(filePart(png), captionPart("words"))
	huge, hugeType := multipartBody(filePart(big), captionPart("words"))

	send := func(e *env, to string, body []byte, contentType string) (*httptest.ResponseRecorder, int) {
		p := &probe{r: bytes.NewReader(body)}
		req := httptest.NewRequest(http.MethodPost, "/messages/image?"+imageQuery(to, ""), p)
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		e.router("u1").ServeHTTP(rec, req)
		return rec, p.n
	}

	for name, tc := range map[string]struct {
		body        []byte
		contentType string
	}{
		"well-formed":       {formed, formedType},
		"ten megabytes":     {huge, hugeType},
		"not multipart":     {png, "image/png"},
		"a broken boundary": {[]byte("junk"), "multipart/form-data"},
	} {
		t.Run(name, func(t *testing.T) {
			e := imageEnv()
			for _, to := range []string{"ghost", "u1", "", "u4"} {
				rec, read := send(e, to, tc.body, tc.contentType)
				if rec.Code != http.StatusNotFound || errorCode(t, rec) != "recipient_not_found" {
					t.Errorf("to %q: got %d %s, want 404 recipient_not_found", to, rec.Code, rec.Body)
				}
				if read != 0 {
					t.Errorf("to %q: %d bytes of a stranger's body were read", to, read)
				}
			}
			nothingHappened(t, e)
		})
	}

	// The control: a friend's request is read.
	e := imageEnv()
	if _, read := send(e, "u2", formed, formedType); read == 0 {
		t.Error("the probe saw nothing read for a friend, so it proves nothing")
	}
}

// ---- editing ----------------------------------------------------------------------

func captioned(t *testing.T, e *env, caption string) string {
	t.Helper()
	rec := e.postCaptioned("u1", "u2", "", testPNG(t, 8, 8), caption)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	e.ln.pushes = nil
	return decode[sendResponse](t, rec).ID
}

func patch(t *testing.T, e *env, user, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(user, http.MethodPatch, "/messages/"+id, jsonBody(t, map[string]any{"body": body}))
}

// A caption is text, and text can be rewritten: the body changes, edited_at is set and a
// new seq drawn, both people are told, and the PICTURE does not move - the payload is
// the same, the bytes are untouched and nothing was put or removed.
func TestACaptionCanBeEditedAndOnlyTheCaption(t *testing.T) {
	e := imageEnv()
	id := captioned(t, e, "first words")
	before := *e.db.find(id)
	puts, removes := len(e.objs.puts), len(e.objs.removes)

	rec := patch(t, e, "u1", id, "  better words <@u3>  ")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s, want 200", rec.Code, rec.Body)
	}
	got := decode[messageResponse](t, rec)
	if got.Body != "better words <@u3>" || got.EditedAt == nil || got.Seq <= before.Seq || got.Kind != "image" {
		t.Errorf("response = %+v", got)
	}
	if string(got.Payload) != string(before.Payload) || len(got.Mentions) != 1 || got.Mentions[0] != "u3" {
		t.Errorf("payload %s (was %s), mentions %v", got.Payload, before.Payload, got.Mentions)
	}
	if m := e.db.find(id); m.Kind != kindImage || string(m.Payload) != string(before.Payload) || m.ID != id {
		t.Errorf("stored = %+v: only the body may change", m)
	}
	if len(e.objs.puts) != puts || len(e.objs.removes) != removes || len(e.objs.objs) != 1 {
		t.Errorf("the picture's bytes were touched by a caption edit: puts %v removes %v", e.objs.puts, e.objs.removes)
	}
	for who, frame := range e.ln.frames(t) {
		msg := frame["message"].(map[string]any)
		if frame["op"] != "edit" || msg["body"] != "better words <@u3>" || msg["payload"] == nil || msg["kind"] != "image" {
			t.Errorf("%s was told %v", who, frame)
		}
	}
	if rec := e.getImage("u2", id); rec.Code != http.StatusOK {
		t.Errorf("the picture is no longer served after a caption edit: %d", rec.Code)
	}
}

// A picture sent without words can be given some.
func TestAPictureWithoutACaptionCanBeGivenOne(t *testing.T) {
	e := imageEnv()
	id := sentImage(t, e, testPNG(t, 8, 8))
	rec := patch(t, e, "u1", id, "afterthought")
	if rec.Code != http.StatusOK || decode[messageResponse](t, rec).Body != "afterthought" {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
}

// The edit holds a caption to the rule a body is held to, because it is the same
// cleanBody: too long is refused and nothing changes.
func TestEditingACaptionIsHeldToTheRuleForABody(t *testing.T) {
	e := imageEnv()
	id := captioned(t, e, "words")
	rec := patch(t, e, "u1", id, strings.Repeat("a", 4001))
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "body_too_long" {
		t.Errorf("got %d %s, want 400 body_too_long", rec.Code, rec.Body)
	}
	if rec := patch(t, e, "u1", id, strings.Repeat("a", 4000)); rec.Code != http.StatusOK {
		t.Errorf("4000 characters = %d, want 200", rec.Code)
	}
	if e.db.find(id).Body != strings.Repeat("a", 4000) || len(e.ln.pushes) != 2 {
		t.Error("only the accepted edit should have changed or pushed anything")
	}
}

// What an edit refuses is still the one opaque answer: someone else's picture, a deleted
// one, a former friend's, an id that names nothing - and an invite, which has no words.
// None of them reaches the friendship check except the sender's own, and none changes
// or pushes anything.
func TestEditingAPictureIsRefusedWithTheSameOpaque404(t *testing.T) {
	e := imageEnv()
	id := captioned(t, e, "words")
	deleted := captioned(t, e, "gone")
	if rec := e.do("u1", http.MethodDelete, "/messages/"+deleted, ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	if err := e.h.MintInvite(t.Context(), "inv1", "room1", "umber", "u1", "u2"); err != nil {
		t.Fatal(err)
	}
	invite := e.db.msgs[len(e.db.msgs)-1].ID
	e.ln.pushes = nil

	nobody := patch(t, e, "u1", "no-such-message", "x")
	if nobody.Code != http.StatusNotFound || errorCode(t, nobody) != "message_not_found" {
		t.Fatalf("unknown id = %d %s", nobody.Code, nobody.Body)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"the recipient editing the sender's picture": patch(t, e, "u2", id, "x"),
		"a third party":     patch(t, e, "u3", id, "x"),
		"a deleted picture": patch(t, e, "u1", deleted, "x"),
		"an invite":         patch(t, e, "u1", invite, "x"),
	} {
		if rec.Code != http.StatusNotFound || rec.Body.String() != nobody.Body.String() {
			t.Errorf("%s: %d %q differs from the unknown id's %d %q", name, rec.Code, rec.Body, nobody.Code, nobody.Body)
		}
	}
	e.db.friends = map[[2]string]bool{}
	if rec := patch(t, e, "u1", id, "x"); rec.Code != http.StatusNotFound || rec.Body.String() != nobody.Body.String() {
		t.Errorf("a former friend: %d %q", rec.Code, rec.Body)
	}
	if e.db.find(id).Body != "words" || e.db.find(invite).Body != "" || len(e.ln.pushes) != 0 {
		t.Error("a refused edit changed or pushed something")
	}
}

// The set of kinds an edit may touch is spelled twice - once in Go, once in the SQL a
// statement cannot call Go to get - and this holds the two to each other over every kind
// there is, so a kind added to one and not the other is a red test.
func TestTheEditableKindsInSQLAreTheKindsWithWords(t *testing.T) {
	for _, kind := range []string{kindText, kindInvite, kindImage} {
		named := strings.Contains(editableKindsSQL, "'"+kind+"'")
		if named != kindHasWords(kind) {
			t.Errorf("%s: editableKindsSQL names it = %v, kindHasWords = %v", kind, named, kindHasWords(kind))
		}
	}
}

// ---- deleting -----------------------------------------------------------------------

// A delete takes the words with the picture: the tombstone says nothing, and the frame
// carries neither.
func TestDeletingACaptionedPictureClearsTheWordsAndTheBytes(t *testing.T) {
	e := imageEnv()
	id := captioned(t, e, "private words")
	if rec := e.do("u1", http.MethodDelete, "/messages/"+id, ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	m := e.db.find(id)
	if m.Body != "" || m.Payload != nil || m.DeletedAt == nil || m.Kind != kindImage {
		t.Errorf("stored = %+v, want a tombstone with no words and no payload that is still an image", m)
	}
	if len(e.objs.objs) != 0 {
		t.Errorf("the bytes outlived the message: %v", e.objs.keys())
	}
	for who, frame := range e.ln.frames(t) {
		if msg := frame["message"].(map[string]any); msg["body"] != "" || frame["op"] != "delete" || msg["payload"] != nil {
			t.Errorf("%s was told %v", who, frame)
		}
	}
}
