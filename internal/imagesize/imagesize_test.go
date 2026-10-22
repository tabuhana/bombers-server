package imagesize

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"testing"
)

// ---- builders ---------------------------------------------------------------

func solid(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	return img
}

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, solid(w, h)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, solid(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func gifOf(t *testing.T, w, h int) []byte {
	t.Helper()
	pal := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White})
	var b bytes.Buffer
	if err := gif.Encode(&b, pal, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// riff wraps one chunk in the RIFF/WEBP container the way an encoder does, with the
// RIFF size filled in.
func riff(fourcc string, body []byte) []byte {
	chunk := append([]byte(fourcc), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(chunk[4:], uint32(len(body)))
	chunk = append(chunk, body...)
	out := append([]byte("RIFF"), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(out[4:], uint32(4+len(chunk)))
	out = append(out, "WEBP"...)
	return append(out, chunk...)
}

// vp8x is the extended header an animated WebP begins with: a flags byte, three
// reserved bytes, then canvas width-1 and height-1 as 24-bit little-endian numbers.
func vp8x(w, h int) []byte {
	body := make([]byte, 10)
	body[0] = 0x12 // animation + alpha flags; not part of the size
	body[4], body[5], body[6] = byte(w-1), byte((w-1)>>8), byte((w-1)>>16)
	body[7], body[8], body[9] = byte(h-1), byte((h-1)>>8), byte((h-1)>>16)
	return riff("VP8X", body)
}

// vp8l is lossless: a 0x2f signature, then width-1 and height-1 in 14 bits each.
func vp8l(w, h int) []byte {
	body := make([]byte, 5+4) // a few bytes of "bitstream" after the header
	body[0] = 0x2f
	binary.LittleEndian.PutUint32(body[1:], uint32(w-1)|uint32(h-1)<<14)
	return riff("VP8L", body)
}

// vp8 is lossy: a 3-byte key-frame tag, the start code, then 14-bit width and height
// with a scaling field in the top two bits of each.
func vp8(w, h int) []byte {
	body := make([]byte, 10+4)
	body[3], body[4], body[5] = 0x9d, 0x01, 0x2a
	binary.LittleEndian.PutUint16(body[6:], uint16(w)|0x4000) // scale bits set: not part of the size
	binary.LittleEndian.PutUint16(body[8:], uint16(h)|0x8000)
	return riff("VP8 ", body)
}

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withAPP1 puts an EXIF-sized APP1 segment, a comment and some fill bytes between the
// SOI and the rest of a JPEG, which is where a camera's files carry their metadata and
// what a naive "SOF is at byte 2" reader trips on.
func withAPP1(jpg []byte) []byte {
	app := append([]byte{0xff, 0xe1, 0x00, 0x00}, bytes.Repeat([]byte{0x41}, 300)...)
	binary.BigEndian.PutUint16(app[2:4], uint16(len(app)-2))
	com := []byte{0xff, 0xfe, 0x00, 0x07, 'h', 'e', 'l', 'l', 'o'}
	out := append([]byte{0xff, 0xd8}, app...)
	out = append(out, 0xff, 0xff, 0xff) // fill bytes before a marker are legal
	out = append(out, com...)
	return append(out, jpg[2:]...)
}

// ---- the table --------------------------------------------------------------

func TestDimensionsReadsEveryAcceptedFormat(t *testing.T) {
	cases := []struct {
		name          string
		mime          string
		data          []byte
		width, height int
	}{
		{"png", "image/png", pngOf(t, 37, 21), 37, 21},
		{"png, one pixel", "image/png", pngOf(t, 1, 1), 1, 1},
		{"png, wider than a byte", "image/png", pngOf(t, 600, 3), 600, 3},
		{"jpeg", "image/jpeg", jpegOf(t, 41, 17), 41, 17},
		{"jpeg, wider than a byte", "image/jpeg", jpegOf(t, 700, 5), 700, 5},
		{"jpeg behind APP1, a comment and fill bytes", "image/jpeg", withAPP1(jpegOf(t, 64, 48)), 64, 48},
		{"gif", "image/gif", gifOf(t, 13, 9), 13, 9},
		{"gif, wider than a byte", "image/gif", gifOf(t, 400, 2), 400, 2},
		{"webp VP8X (what an animated one begins with)", "image/webp", vp8x(1280, 720), 1280, 720},
		{"webp VP8X, past 16 bits", "image/webp", vp8x(70000, 3), 70000, 3},
		{"webp VP8X, one pixel", "image/webp", vp8x(1, 1), 1, 1},
		{"webp VP8L", "image/webp", vp8l(1920, 1080), 1920, 1080},
		{"webp VP8L at the 14-bit limit", "image/webp", vp8l(16384, 16384), 16384, 16384},
		{"webp VP8", "image/webp", vp8(640, 480), 640, 480},
		{"webp VP8 at the 14-bit limit", "image/webp", vp8(16383, 1), 16383, 1},
		// Real files from an encoder, so the builders above are not only agreeing with
		// the reader about a layout they both got wrong.
		{"webp lossy, from an encoder", "image/webp", b64(t, "UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA"), 1, 1},
		{"webp lossless, from an encoder", "image/webp", b64(t, "UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA=="), 1, 1},
		{"webp animated, from an encoder", "image/webp", b64(t, "UklGRlIAAABXRUJQVlA4WAoAAAASAAAAAAAAAAAAQU5JTQYAAAD/////AABBTk1GJgAAAAAAAAAAAAAAAAAAAGQAAABWUDhMDQAAAC8AAAAQBxAREYiI/gcA"), 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// What the server does: the type comes from the bytes, never from a label.
			if got := http.DetectContentType(tc.data); got != tc.mime {
				t.Fatalf("test data sniffs as %q, want %q", got, tc.mime)
			}
			w, h, err := Dimensions(tc.mime, tc.data)
			if err != nil {
				t.Fatalf("Dimensions: %v", err)
			}
			if w != tc.width || h != tc.height {
				t.Errorf("got %dx%d, want %dx%d", w, h, tc.width, tc.height)
			}
		})
	}
}

// The standard library decodes three of the four formats, so for those it is an
// independent answer to compare against.
func TestDimensionsAgreeWithTheStandardLibrary(t *testing.T) {
	for _, tc := range []struct {
		mime string
		data []byte
	}{
		{"image/png", pngOf(t, 123, 45)},
		{"image/jpeg", jpegOf(t, 99, 77)},
		{"image/jpeg", withAPP1(jpegOf(t, 31, 29))},
		{"image/gif", gifOf(t, 50, 60)},
	} {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(tc.data))
		if err != nil {
			t.Fatal(err)
		}
		w, h, err := Dimensions(tc.mime, tc.data)
		if err != nil {
			t.Fatalf("%s: %v", tc.mime, err)
		}
		if w != cfg.Width || h != cfg.Height {
			t.Errorf("%s: got %dx%d, stdlib says %dx%d", tc.mime, w, h, cfg.Width, cfg.Height)
		}
	}
}

func TestAcceptsExactlyTheFourFormats(t *testing.T) {
	for _, m := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
		if !Accepts(m) {
			t.Errorf("Accepts(%q) = false", m)
		}
	}
	for _, m := range []string{"", "image/svg+xml", "image/bmp", "image/avif", "image/tiff", "text/plain; charset=utf-8", "application/pdf", "IMAGE/PNG", "image/png; charset=x"} {
		if Accepts(m) {
			t.Errorf("Accepts(%q) = true", m)
		}
	}
}

func TestDimensionsRefusesAFormatItDoesNotRead(t *testing.T) {
	if _, _, err := Dimensions("image/bmp", []byte("BM....")); !errors.Is(err, ErrUnreadable) {
		t.Errorf("err = %v, want ErrUnreadable", err)
	}
}

// ---- files that are not what they claim -------------------------------------

func TestDimensionsRefusesAZeroSize(t *testing.T) {
	zeroPNG := pngOf(t, 4, 4)
	binary.BigEndian.PutUint32(zeroPNG[16:], 0) // width
	zeroPNGHeight := pngOf(t, 4, 4)
	binary.BigEndian.PutUint32(zeroPNGHeight[20:], 0)
	hugePNG := pngOf(t, 4, 4)
	binary.BigEndian.PutUint32(hugePNG[16:], 1<<31) // past the 2^31-1 the specification allows

	zeroGIF := gifOf(t, 4, 4)
	zeroGIF[8], zeroGIF[9] = 0, 0

	zeroJPEG := jpegOf(t, 4, 4)
	sof := bytes.Index(zeroJPEG, []byte{0xff, 0xc0})
	if sof < 0 {
		t.Fatal("no SOF0 in the test jpeg")
	}
	zeroJPEG[sof+5], zeroJPEG[sof+6] = 0, 0 // height

	// VP8X and VP8L store size-1, so they cannot say zero at all (their all-zero field
	// is one pixel: see TestWebPSizeMinusOneFieldsCannotBeZero). The only zero a WebP
	// can carry is a VP8 key frame's.
	zeroVP8 := vp8(5, 5)
	binary.LittleEndian.PutUint16(zeroVP8[26:], 0x4000) // 14-bit width of 0, scale bits set

	for _, tc := range []struct {
		name string
		mime string
		data []byte
	}{
		{"png width 0", "image/png", zeroPNG},
		{"png height 0", "image/png", zeroPNGHeight},
		{"png width past 2^31-1", "image/png", hugePNG},
		{"gif 0 high", "image/gif", zeroGIF},
		{"jpeg 0 high", "image/jpeg", zeroJPEG},
		{"webp VP8 0 wide", "image/webp", zeroVP8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w, h, err := Dimensions(tc.mime, tc.data); !errors.Is(err, ErrUnreadable) {
				t.Errorf("got %dx%d, %v; want ErrUnreadable", w, h, err)
			}
		})
	}
}

// VP8X and VP8L carry width-1 and height-1, so a zeroed field is one pixel and not
// none. Reading the field without adding the one back would report a zero-wide picture
// for every 1-pixel-wide image, and one pixel too few for all the rest.
func TestWebPSizeMinusOneFieldsCannotBeZero(t *testing.T) {
	zeroL := vp8l(2, 2)
	binary.LittleEndian.PutUint32(zeroL[21:], 0)
	zeroX := vp8x(2, 2)
	for i := 24; i < 30; i++ {
		zeroX[i] = 0
	}
	for name, data := range map[string][]byte{"VP8L": zeroL, "VP8X": zeroX} {
		if w, h, err := Dimensions("image/webp", data); err != nil || w != 1 || h != 1 {
			t.Errorf("%s with zeroed fields = %dx%d, %v; want 1x1", name, w, h, err)
		}
	}
}

// A truncated file is an error at every length short of the header, in every format,
// and never a panic. Cutting a real file at each byte is the cheapest way to meet every
// bounds check the reader has.
func TestDimensionsNeverPanicsOnATruncatedFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		mime string
		data []byte
		// need is how many leading bytes the answer depends on: any prefix shorter
		// than this has to be refused.
		need int
	}{
		{"png", "image/png", pngOf(t, 8, 8), 24},
		{"gif", "image/gif", gifOf(t, 8, 8), 10},
		{"jpeg", "image/jpeg", jpegOf(t, 8, 8), 0}, // measured below: the frame header's own end
		{"webp VP8X", "image/webp", vp8x(8, 8), len(vp8x(8, 8))},
		{"webp VP8L", "image/webp", vp8l(8, 8), len(vp8l(8, 8))},
		{"webp VP8", "image/webp", vp8(8, 8), len(vp8(8, 8))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			need := tc.need
			if tc.mime == "image/jpeg" {
				need = bytes.Index(tc.data, []byte{0xff, 0xc0}) + 2 + 7 // marker, then up to the width
			}
			for n := 0; n < need; n++ {
				if w, h, err := Dimensions(tc.mime, tc.data[:n]); err == nil {
					t.Errorf("a %d-byte prefix gave %dx%d with no error", n, w, h)
				}
			}
			// And every prefix, long enough or not, must at least not panic.
			for n := 0; n <= len(tc.data); n++ {
				_, _, _ = Dimensions(tc.mime, tc.data[:n])
			}
		})
	}
}

func TestDimensionsRefusesEmptyAndNilData(t *testing.T) {
	for _, mime := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
		if _, _, err := Dimensions(mime, nil); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s nil: err = %v", mime, err)
		}
		if _, _, err := Dimensions(mime, []byte{}); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s empty: err = %v", mime, err)
		}
	}
}

// A file whose bytes are not its format, however the format was arrived at.
func TestDimensionsRefusesTheWrongBytes(t *testing.T) {
	png := pngOf(t, 4, 4)
	badSig := append([]byte(nil), png...)
	badSig[1] = 'X'
	notIHDR := append([]byte(nil), png...)
	copy(notIHDR[12:16], "IDAT")
	shortIHDR := append([]byte(nil), png...)
	binary.BigEndian.PutUint32(shortIHDR[8:], 12)

	gifBad := append([]byte(nil), gifOf(t, 4, 4)...)
	gifBad[3] = '9'

	for _, tc := range []struct {
		name string
		mime string
		data []byte
	}{
		{"png with a damaged signature", "image/png", badSig},
		{"png whose first chunk is not IHDR", "image/png", notIHDR},
		{"png whose IHDR is the wrong length", "image/png", shortIHDR},
		{"a gif signature that is not one", "image/gif", gifBad},
		{"a png labelled as a gif", "image/gif", png},
		{"a gif labelled as a png", "image/png", gifOf(t, 4, 4)},
		{"a jpeg that does not start with SOI", "image/jpeg", append([]byte{0x00}, jpegOf(t, 4, 4)...)},
		{"plain text labelled as a jpeg", "image/jpeg", []byte("this is a text file that is long enough")},
		{"a riff that is not a webp", "image/webp", append([]byte("RIFF\x00\x00\x00\x00WAVE"), make([]byte, 40)...)},
		{"a webp whose first chunk is not an image chunk", "image/webp", riff("EXIF", make([]byte, 20))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w, h, err := Dimensions(tc.mime, tc.data); !errors.Is(err, ErrUnreadable) {
				t.Errorf("got %dx%d, %v; want ErrUnreadable", w, h, err)
			}
		})
	}
}

// A declared length that runs past the end of the data is an error and not a slice
// out of range, whatever the format spells it as.
func TestDimensionsRefusesALengthThatRunsPastTheEnd(t *testing.T) {
	// WebP: the first chunk claims more bytes than the file has.
	long := vp8x(8, 8)
	binary.LittleEndian.PutUint32(long[16:], 1<<20)
	huge := vp8l(8, 8)
	binary.LittleEndian.PutUint32(huge[16:], 0xffffffff) // would wrap a 32-bit sum
	// ...or fewer than the fields the reader needs.
	tiny := riff("VP8X", make([]byte, 4))
	tinyL := riff("VP8L", make([]byte, 3))
	tiny8 := riff("VP8 ", make([]byte, 6))

	// JPEG: a segment whose length takes it off the end, one with a nonsense length,
	// a frame header too short to hold a size, and a marker with nothing after it.
	app := []byte{0xff, 0xd8, 0xff, 0xe1, 0xff, 0xff, 1, 2, 3, 4}
	zeroLen := []byte{0xff, 0xd8, 0xff, 0xe1, 0x00, 0x01, 1, 2, 3, 4}
	shortSOF := []byte{0xff, 0xd8, 0xff, 0xc0, 0x00, 0x05, 8, 0, 4, 0, 4, 0}
	cutMarker := []byte{0xff, 0xd8, 0xff}
	cutLength := []byte{0xff, 0xd8, 0xff, 0xe1, 0x00}
	noFrame := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x04, 1, 2, 0xff, 0xda, 0x00, 0x04, 1, 2}
	eoi := []byte{0xff, 0xd8, 0xff, 0xd9, 0, 0, 0, 0}
	stuffed := []byte{0xff, 0xd8, 0xff, 0x00, 0, 0, 0, 0}
	nonMarker := []byte{0xff, 0xd8, 0x12, 0x34, 0, 0, 0, 0}
	// After the scan starts there is no marker walk to follow: bytes that merely look like a
	// frame header there (a scan whose length field is FF C0) are not one.
	fakeFrame := []byte{0xff, 0xd8, 0xff, 0xda, 0xff, 0xc0, 0x00, 0x0b, 8, 0x00, 0x20, 0x00, 0x30, 1, 1, 0x11, 0x00}

	for _, tc := range []struct {
		name string
		mime string
		data []byte
	}{
		{"webp chunk longer than the file", "image/webp", long},
		{"webp chunk length that would wrap", "image/webp", huge},
		{"webp VP8X too short for a canvas", "image/webp", tiny},
		{"webp VP8L too short for a header", "image/webp", tinyL},
		{"webp VP8 too short for a key frame header", "image/webp", tiny8},
		{"jpeg segment longer than the file", "image/jpeg", app},
		{"jpeg segment shorter than its own length bytes", "image/jpeg", zeroLen},
		{"jpeg frame header too short for a size", "image/jpeg", shortSOF},
		{"jpeg ending on a marker", "image/jpeg", cutMarker},
		{"jpeg ending inside a length", "image/jpeg", cutLength},
		{"jpeg that reaches its scan with no frame header", "image/jpeg", noFrame},
		{"jpeg that ends before any frame header", "image/jpeg", eoi},
		{"jpeg with a stuffed zero where a marker belongs", "image/jpeg", stuffed},
		{"jpeg with data where a marker belongs", "image/jpeg", nonMarker},
		{"jpeg with something frame-shaped only after its scan", "image/jpeg", fakeFrame},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w, h, err := Dimensions(tc.mime, tc.data); !errors.Is(err, ErrUnreadable) {
				t.Errorf("got %dx%d, %v; want ErrUnreadable", w, h, err)
			}
		})
	}
}

// A VP8 chunk that does not begin with a key frame, or whose start code is wrong, is
// not a picture header, whatever bytes sit where the size would be.
func TestDimensionsRefusesALossyHeaderThatIsNotAKeyFrame(t *testing.T) {
	inter := vp8(8, 8)
	inter[20] |= 1
	badCode := vp8(8, 8)
	badCode[23] = 0
	badL := vp8l(8, 8)
	badL[20] = 0x2e

	for name, data := range map[string][]byte{"an inter frame": inter, "a bad start code": badCode, "a bad VP8L signature": badL} {
		if w, h, err := Dimensions("image/webp", data); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s: got %dx%d, %v; want ErrUnreadable", name, w, h, err)
		}
	}
}

// The JPEG walk must reach a frame header that comes after other frame-like markers: a
// DHT (0xC4) sits inside the SOF range and is not one.
func TestJPEGWalkSkipsDHTAndOtherSegmentsBeforeTheFrame(t *testing.T) {
	dht := []byte{0xff, 0xc4, 0x00, 0x06, 0, 0, 0, 0}
	sof := []byte{0xff, 0xc2, 0x00, 0x0b, 8, 0x00, 0x20, 0x00, 0x30, 1, 1, 0x11, 0x00} // progressive: 0x30 x 0x20
	data := append([]byte{0xff, 0xd8}, append(dht, sof...)...)
	w, h, err := Dimensions("image/jpeg", data)
	if err != nil || w != 0x30 || h != 0x20 {
		t.Errorf("got %dx%d, %v; want 48x32", w, h, err)
	}
}
