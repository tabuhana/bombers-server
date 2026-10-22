package imagesize

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"testing"
)

// ---- builders ---------------------------------------------------------------
//
// Every fixture is built here from bytes, so each test says what is in the file it reads
// and nothing is a binary nobody can inspect.

// ifdEntry is one 12-byte entry of an image file directory.
type ifdEntry struct {
	tag, typ uint16
	count    uint32
	value    [4]byte
}

func orientationEntry(o order, v uint16) ifdEntry {
	e := ifdEntry{tag: 0x0112, typ: 3, count: 1}
	o.PutUint16(e.value[:2], v) // a SHORT is left-justified in the four value bytes
	return e
}

// otherEntry is a tag that is not the orientation, here Make: ASCII, four bytes, inline.
func otherEntry() ifdEntry {
	return ifdEntry{tag: 0x010f, typ: 2, count: 4, value: [4]byte{'A', 'B', 'C', 0}}
}

// order is both halves of a byte order: reading a value and appending one.
type order interface {
	binary.ByteOrder
	binary.AppendByteOrder
}

func byteOrder(little bool) (order, string) {
	if little {
		return binary.LittleEndian, "II"
	}
	return binary.BigEndian, "MM"
}

// tiffOf is a TIFF structure whose IFD0 holds entries, in the byte order asked for.
func tiffOf(little bool, entries ...ifdEntry) []byte {
	order, mark := byteOrder(little)
	b := []byte(mark)
	b = order.AppendUint16(b, 42)
	b = order.AppendUint32(b, 8)
	b = order.AppendUint16(b, uint16(len(entries)))
	for _, e := range entries {
		b = order.AppendUint16(b, e.tag)
		b = order.AppendUint16(b, e.typ)
		b = order.AppendUint32(b, e.count)
		b = append(b, e.value[:]...)
	}
	return order.AppendUint32(b, 0) // no IFD1
}

// photoTIFF is what a camera writes: a Make and then the orientation, in tag order.
func photoTIFF(little bool, orientation uint16) []byte {
	order, _ := byteOrder(little)
	return tiffOf(little, otherEntry(), orientationEntry(order, orientation))
}

// orientationWith is photoTIFF's orientation entry with one field changed by edit.
func orientationWith(little bool, edit func(*ifdEntry)) []byte {
	order, _ := byteOrder(little)
	e := orientationEntry(order, 6)
	edit(&e)
	return tiffOf(little, otherEntry(), e)
}

func segment(marker byte, body []byte) []byte {
	out := []byte{0xff, marker, 0, 0}
	binary.BigEndian.PutUint16(out[2:], uint16(2+len(body)))
	return append(out, body...)
}

func app1Exif(tiff []byte) []byte { return segment(0xe1, append([]byte(exifHeader), tiff...)) }

// sof0 is a baseline frame header and nothing after it: enough for the walk, which never
// decodes, and small enough that a 12-megapixel header costs nothing.
func sof0(w, h int) []byte {
	return segment(0xc0, []byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), 1, 1, 0x11, 0})
}

// jpegOfParts is a start of image and then the segments given, in order.
func jpegOfParts(parts ...[]byte) []byte {
	return append([]byte{0xff, 0xd8}, bytes.Join(parts, nil)...)
}

func pngChunk(kind string, body []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	out = append(out, kind...)
	out = append(out, body...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(kind), body...)))
}

// pngOfParts is a PNG signature, an IHDR for w x h and then the chunks given.
func pngOfParts(w, h int, chunks ...[]byte) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, uint32(w))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(h))
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	out := append([]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", ihdr)...)
	return append(out, bytes.Join(chunks, nil)...)
}

// webpChunk is a RIFF chunk with its pad byte.
func webpChunk(fourcc string, body []byte) []byte {
	out := append([]byte(fourcc), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(body)))
	out = append(out, body...)
	if len(body)&1 == 1 {
		out = append(out, 0)
	}
	return out
}

func webpOfChunks(chunks ...[]byte) []byte {
	body := append([]byte("WEBP"), bytes.Join(chunks, nil)...)
	out := append([]byte("RIFF"), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(body)))
	return append(out, body...)
}

// vp8xChunk is the extended header, with the flags given.
func vp8xChunk(flags byte, w, h int) []byte {
	body := make([]byte, 10)
	body[0] = flags
	body[4], body[5], body[6] = byte(w-1), byte((w-1)>>8), byte((w-1)>>16)
	body[7], body[8], body[9] = byte(h-1), byte((h-1)>>8), byte((h-1)>>16)
	return webpChunk("VP8X", body)
}

func vp8lChunk(w, h int) []byte {
	body := make([]byte, 9)
	body[0] = 0x2f
	binary.LittleEndian.PutUint32(body[1:], uint32(w-1)|uint32(h-1)<<14)
	return webpChunk("VP8L", body)
}

const vp8xExifFlag = 0x08

// webpWithExif is an extended WebP: VP8X (with the EXIF flag), the image, then EXIF.
func webpWithExif(w, h int, block []byte) []byte {
	return webpOfChunks(vp8xChunk(vp8xExifFlag, w, h), vp8lChunk(w, h), webpChunk("EXIF", block))
}

// containers wraps one TIFF block as each format carries it, for a w x h picture. The
// same damage has to give the same answer in every one of them.
var containers = []struct {
	name string
	mime string
	make func(w, h int, tiff []byte) []byte
}{
	{"jpeg", "image/jpeg", func(w, h int, tiff []byte) []byte { return jpegOfParts(app1Exif(tiff), sof0(w, h)) }},
	{"png", "image/png", func(w, h int, tiff []byte) []byte {
		return pngOfParts(w, h, pngChunk("eXIf", tiff), pngChunk("IDAT", []byte{0}))
	}},
	{"webp", "image/webp", func(w, h int, tiff []byte) []byte { return webpWithExif(w, h, tiff) }},
}

func mustDimensions(t *testing.T, mime string, data []byte) (int, int) {
	t.Helper()
	if got := http.DetectContentType(data); got != mime {
		t.Fatalf("fixture sniffs as %q, want %q", got, mime)
	}
	// Capacity clipped to length, so a read past the end is a panic and not the bytes that
	// happen to follow in memory.
	w, h, err := Dimensions(mime, data[:len(data):len(data)])
	if err != nil {
		t.Fatalf("Dimensions: %v", err)
	}
	return w, h
}

// ---- the rule ---------------------------------------------------------------

// The drawn shape: orientations 5 to 8 exchange the sides and nothing else does, in every
// format that can carry one, in both byte orders.
func TestDimensionsAppliesTheOrientationBrowsersApply(t *testing.T) {
	for _, c := range containers {
		for _, little := range []bool{true, false} {
			for o := uint16(0); o <= 9; o++ {
				_, mark := byteOrder(little)
				t.Run(fmt.Sprintf("%s %s orientation %d", c.name, mark, o), func(t *testing.T) {
					w, h := mustDimensions(t, c.mime, c.make(40, 30, photoTIFF(little, o)))
					wantW, wantH := 40, 30
					if o >= 5 && o <= 8 {
						wantW, wantH = 30, 40
					}
					if w != wantW || h != wantH {
						t.Errorf("got %dx%d, want %dx%d", w, h, wantW, wantH)
					}
				})
			}
		}
	}
}

// The case this exists for: a phone's portrait photo is stored 4032x3024 with orientation
// 6, and the browser draws it 3024x4032.
func TestDimensionsOfAPortraitPhotoFromAPhone(t *testing.T) {
	for _, little := range []bool{true, false} {
		data := jpegOfParts(app1Exif(photoTIFF(little, 6)), segment(0xe0, []byte("JFIF\x00")), sof0(4032, 3024))
		if w, h := mustDimensions(t, "image/jpeg", data); w != 3024 || h != 4032 {
			t.Errorf("little=%v: got %dx%d, want 3024x4032", little, w, h)
		}
	}
	// The same photo turned the other way (8), and one that is already upright (1).
	if w, h := mustDimensions(t, "image/jpeg", jpegOfParts(app1Exif(photoTIFF(true, 8)), sof0(4032, 3024))); w != 3024 || h != 4032 {
		t.Errorf("orientation 8: got %dx%d", w, h)
	}
	if w, h := mustDimensions(t, "image/jpeg", jpegOfParts(app1Exif(photoTIFF(true, 1)), sof0(4032, 3024))); w != 4032 || h != 3024 {
		t.Errorf("orientation 1: got %dx%d", w, h)
	}
}

// A real encoder's JPEG, with the EXIF block put in after the start of image.
func TestDimensionsTurnsARealJPEG(t *testing.T) {
	real := jpegOf(t, 64, 48)
	data := append(append([]byte{0xff, 0xd8}, app1Exif(photoTIFF(false, 6))...), real[2:]...)
	if w, h := mustDimensions(t, "image/jpeg", data); w != 48 || h != 64 {
		t.Errorf("got %dx%d, want 48x64", w, h)
	}
}

func TestAGIFHasNoOrientation(t *testing.T) {
	if w, h := mustDimensions(t, "image/gif", gifOf(t, 13, 9)); w != 13 || h != 9 {
		t.Errorf("got %dx%d, want 13x9", w, h)
	}
}

// ---- damage: always an absence, never an error ------------------------------

// header is a TIFF header with the byte-order mark, magic and first-IFD offset given,
// and rest after it.
func header(mark string, magic uint16, ifd uint32, rest ...byte) []byte {
	var order order = binary.LittleEndian
	if mark == "MM" {
		order = binary.BigEndian
	}
	b := []byte(mark)
	b = order.AppendUint16(b, magic)
	b = order.AppendUint32(b, ifd)
	return append(b, rest...)
}

// withMark is tiff with its first two bytes replaced.
func withMark(mark string, tiff []byte) []byte {
	return append([]byte(mark), tiff[2:]...)
}

// damagedTIFFs are EXIF blocks that cannot give an orientation, each for its own reason.
// The picture's size is still good, so each one has to read as if it had no EXIF at all.
func damagedTIFFs() map[string][]byte {
	good := photoTIFF(true, 6)

	// A count of 5 with two entries behind it: the directory runs off the end of the block.
	truncatedIFD := append([]byte(nil), good...)
	binary.LittleEndian.PutUint16(truncatedIFD[8:], 5)
	// A count of 65535 would put the end of the directory near 786 KB.
	hugeCount := append([]byte(nil), good...)
	binary.LittleEndian.PutUint16(hugeCount[8:], 0xffff)
	// A count one more than the entries present, with the trailing offset standing in
	// for most of the extra entry: only a few bytes short.
	oneMore := append([]byte(nil), good...)
	binary.LittleEndian.PutUint16(oneMore[8:], 3)

	return map[string][]byte{
		"empty":                       {},
		"shorter than a header":       good[:7:7],
		"a header and nothing behind": good[:8:8],
		// Everything after the mark is a good directory, so only the mark can refuse these.
		"byte order neither":     withMark("XX", good),
		"byte order lowercase":   withMark("ii", good),
		"byte order half given":  withMark("I\x00", good),
		"wrong magic":            header("II", 43, 8, 0, 0, 0, 0, 0, 0),
		"wrong magic, good rest": append(append([]byte("II"), 43, 0), good[4:]...),
		// The first-IFD offset points back into the header, where bytes 4 to 5 read as a count
		// of 4 and the fourth of those entries is a perfectly good orientation: only the rule
		// that a directory cannot start inside the header refuses it.
		"IFD offset inside the header, onto a good entry": func() []byte {
			b := header("II", 42, 4, make([]byte, 10)...) // bytes 8 to 17: filler
			e := orientationEntry(binary.LittleEndian, 6)
			b = binary.LittleEndian.AppendUint16(b, e.tag)
			b = binary.LittleEndian.AppendUint16(b, e.typ)
			b = binary.LittleEndian.AppendUint32(b, e.count)
			b = append(b, e.value[:]...)
			return append(b, make([]byte, 24)...)
		}(),
		"magic in the wrong order": append([]byte("II\x00\x2a"), good[4:]...),
		"IFD offset past the end":  header("II", 42, 0x1000, make([]byte, 40)...),
		"IFD offset the largest":   header("II", 42, 0xffffffff, make([]byte, 40)...),
		// Offsets whose sum with the count field wraps a 32-bit int to a small or negative number.
		"IFD offset that wraps a 32-bit sum": header("II", 42, 0x7ffffffe, make([]byte, 40)...),
		"IFD offset 0x80000000":              header("II", 42, 0x80000000, make([]byte, 40)...),
		"IFD offset 0xfffffff8":              header("II", 42, 0xfffffff8, make([]byte, 40)...),
		"IFD offset 0xfffffffe":              header("II", 42, 0xfffffffe, make([]byte, 40)...),
		"IFD offset leaves no count":         header("II", 42, 9, 0, 0),
		"IFD offset inside the head":         header("II", 42, 4, make([]byte, 40)...),
		"IFD offset zero":                    header("II", 42, 0, make([]byte, 40)...),
		"count of zero":                      tiffOf(true),
		"truncated IFD":                      truncatedIFD,
		"count that cannot fit":              hugeCount,
		"count one more than present":        oneMore,
		"directory one byte short":           good[: len(good)-4-1 : len(good)-4-1],
		"cut inside an entry":                good[: 8+2+12+6 : 8+2+12+6],
		"type BYTE":                          orientationWith(true, func(e *ifdEntry) { e.typ = 1 }),
		"type LONG":                          orientationWith(true, func(e *ifdEntry) { e.typ = 4 }),
		"nonsense type":                      orientationWith(true, func(e *ifdEntry) { e.typ = 0xffff }),
		"count of two":                       orientationWith(true, func(e *ifdEntry) { e.count = 2 }),
		"count of zero values":               orientationWith(true, func(e *ifdEntry) { e.count = 0 }),
		"value 0":                            photoTIFF(true, 0),
		"value 9":                            photoTIFF(true, 9),
		"value 0xffff":                       photoTIFF(true, 0xffff),
		"no orientation tag":                 tiffOf(true, otherEntry()),
	}
}

func TestADamagedOrientationBlockIsNoOrientationAndNotAnError(t *testing.T) {
	for name, tiff := range damagedTIFFs() {
		for _, c := range containers {
			t.Run(c.name+" "+name, func(t *testing.T) {
				w, h := mustDimensions(t, c.mime, c.make(40, 30, tiff))
				if w != 40 || h != 30 {
					t.Errorf("got %dx%d, want the stored 40x30", w, h)
				}
			})
		}
	}
}

// Whatever a byte of the EXIF block is changed to, the read neither panics nor fails: the
// answer is the stored size or the turned one. This meets every bounds check the parser
// has with real data around it, in both byte orders.
func TestEveryByteOfTheBlockChangedNeverFailsTheRead(t *testing.T) {
	for _, c := range containers {
		for _, little := range []bool{true, false} {
			base := photoTIFF(little, 6)
			for at := range base {
				for v := 0; v < 256; v++ {
					tiff := append([]byte(nil), base...)
					tiff[at] = byte(v)
					w, h, err := Dimensions(c.mime, c.make(40, 30, tiff))
					if err != nil {
						t.Fatalf("%s little=%v byte %d = %#x: %v", c.name, little, at, v, err)
					}
					if !(w == 40 && h == 30) && !(w == 30 && h == 40) {
						t.Fatalf("%s little=%v byte %d = %#x: got %dx%d", c.name, little, at, v, w, h)
					}
				}
			}
		}
	}
}

// A file cut at any byte never panics, and where it still has a size it is the stored or
// the turned one.
func TestAnOrientedFileCutAnywhereNeverPanics(t *testing.T) {
	for _, c := range containers {
		data := c.make(40, 30, photoTIFF(true, 6))
		for n := 0; n <= len(data); n++ {
			w, h, err := Dimensions(c.mime, data[:n])
			if err != nil {
				if !errors.Is(err, ErrUnreadable) {
					t.Errorf("%s cut at %d: %v", c.name, n, err)
				}
				continue
			}
			if !(w == 40 && h == 30) && !(w == 30 && h == 40) {
				t.Errorf("%s cut at %d: got %dx%d", c.name, n, w, h)
			}
		}
	}
}

// ---- JPEG: where the block is ------------------------------------------------

func TestJPEGOrientationIsFoundWhereverTheBlockSits(t *testing.T) {
	xmp := segment(0xe1, append([]byte("http://ns.adobe.com/xap/1.0/\x00"), "<x:xmpmeta/>"...))
	exif6 := app1Exif(photoTIFF(true, 6))
	le := binary.LittleEndian
	for _, tc := range []struct {
		name string
		data []byte
		w, h int
	}{
		{"no APP1 at all", jpegOfParts(sof0(40, 30)), 40, 30},
		{"an APP1 that is XMP and not EXIF", jpegOfParts(xmp, sof0(40, 30)), 40, 30},
		{"XMP first and EXIF second", jpegOfParts(xmp, exif6, sof0(40, 30)), 30, 40},
		{"EXIF after other segments and fill bytes", jpegOfParts(segment(0xe0, []byte("JFIF\x00")), []byte{0xff, 0xff}, exif6, sof0(40, 30)), 30, 40},
		{"EXIF after the frame header, before the scan", jpegOfParts(sof0(40, 30), exif6), 30, 40},
		{"the first EXIF block decides, with no orientation in it", jpegOfParts(app1Exif(tiffOf(true, otherEntry())), exif6, sof0(40, 30)), 40, 30},
		{"the first EXIF block decides, with one", jpegOfParts(exif6, app1Exif(photoTIFF(true, 1)), sof0(40, 30)), 30, 40},
		{"two orientation entries: the first", jpegOfParts(app1Exif(tiffOf(true, orientationEntry(le, 1), orientationEntry(le, 6))), sof0(40, 30)), 40, 30},
		{"an APP1 whose EXIF header is cut short", jpegOfParts(segment(0xe1, []byte("Exi")), sof0(40, 30)), 40, 30},
		{"an APP1 with the EXIF header and nothing else", jpegOfParts(segment(0xe1, []byte(exifHeader)), sof0(40, 30)), 40, 30},
		{"EXIF in an APP2", jpegOfParts(segment(0xe2, append([]byte(exifHeader), photoTIFF(true, 6)...)), sof0(40, 30)), 40, 30},
		{"EXIF header without the two zero bytes", jpegOfParts(segment(0xe1, append([]byte("Exif"), photoTIFF(true, 6)...)), sof0(40, 30)), 40, 30},
		{"a bare TIFF in the APP1, without the EXIF header", jpegOfParts(segment(0xe1, photoTIFF(true, 6)), sof0(40, 30)), 40, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w, h := mustDimensions(t, "image/jpeg", tc.data); w != tc.w || h != tc.h {
				t.Errorf("got %dx%d, want %dx%d", w, h, tc.w, tc.h)
			}
		})
	}
}

// Once the size has been read nothing after it can take it back: the walk goes on only
// to look for an orientation, and a damaged or missing segment there ends it.
func TestJPEGDamageAfterTheFrameHeaderKeepsTheSize(t *testing.T) {
	exif6 := app1Exif(photoTIFF(true, 6))
	for name, tail := range map[string][]byte{
		"the end of the data":               nil,
		"a segment that runs past the end":  {0xff, 0xe1, 0xff, 0xff, 1, 2},
		"a marker cut short":                {0xff},
		"a length cut short":                {0xff, 0xe1, 0x00},
		"a stuffed zero":                    {0xff, 0x00},
		"bytes that are not a marker":       {0x12, 0x34},
		"a segment shorter than its length": {0xff, 0xe1, 0x00, 0x01},
		"the scan":                          {0xff, 0xda, 0x00, 0x04, 1, 2},
		"the end of image":                  {0xff, 0xd9},
	} {
		t.Run(name, func(t *testing.T) {
			if w, h := mustDimensions(t, "image/jpeg", jpegOfParts(sof0(40, 30), tail)); w != 40 || h != 30 {
				t.Errorf("no EXIF: got %dx%d", w, h)
			}
			// EXIF that was read before the damage still counts.
			if w, h := mustDimensions(t, "image/jpeg", jpegOfParts(exif6, sof0(40, 30), tail)); w != 30 || h != 40 {
				t.Errorf("EXIF before: got %dx%d, want 30x40", w, h)
			}
		})
	}
	// Bytes after the scan are image data, and are not read for an EXIF block.
	data := jpegOfParts(sof0(40, 30), segment(0xda, []byte{1, 2}), exif6)
	if w, h := mustDimensions(t, "image/jpeg", data); w != 40 || h != 30 {
		t.Errorf("EXIF after the scan: got %dx%d", w, h)
	}
	// A second frame header is not the frame: the first one is.
	if w, h := mustDimensions(t, "image/jpeg", jpegOfParts(exif6, sof0(40, 30), sof0(8, 9))); w != 30 || h != 40 {
		t.Errorf("second SOF: got %dx%d", w, h)
	}
}

// What was already an error stays one: an EXIF block does not make a file without a
// frame header readable.
func TestAnOrientationDoesNotMakeAJPEGWithNoFrameReadable(t *testing.T) {
	data := jpegOfParts(app1Exif(photoTIFF(true, 6)), segment(0xda, []byte{1, 2}))
	if _, _, err := Dimensions("image/jpeg", data); !errors.Is(err, ErrUnreadable) {
		t.Errorf("err = %v, want ErrUnreadable", err)
	}
	if _, _, err := Dimensions("image/jpeg", jpegOfParts(app1Exif(photoTIFF(true, 6)))); !errors.Is(err, ErrUnreadable) {
		t.Errorf("no frame: err = %v, want ErrUnreadable", err)
	}
}

// ---- PNG ---------------------------------------------------------------------

func TestPNGOrientationIsReadBeforeTheImageDataOnly(t *testing.T) {
	exif6 := pngChunk("eXIf", photoTIFF(true, 6))
	other := pngChunk("pHYs", make([]byte, 9))
	idat := pngChunk("IDAT", []byte{0})
	huge := append(binary.BigEndian.AppendUint32(nil, 0xffffffff), "eXIf"...) // a length that wraps a 32-bit sum
	for _, tc := range []struct {
		name string
		data []byte
		w, h int
	}{
		{"eXIf straight after IHDR", pngOfParts(40, 30, exif6, idat), 30, 40},
		{"eXIf after other chunks", pngOfParts(40, 30, other, pngChunk("sRGB", []byte{0}), exif6, idat), 30, 40},
		{"no eXIf", pngOfParts(40, 30, other, idat), 40, 30},
		{"no chunks after IHDR", pngOfParts(40, 30), 40, 30},
		{"eXIf after IDAT is not looked for", pngOfParts(40, 30, idat, exif6), 40, 30},
		{"eXIf after IEND is not looked for", pngOfParts(40, 30, pngChunk("IEND", nil), exif6), 40, 30},
		{"a chunk whose length runs past the end", pngOfParts(40, 30, other[:len(other)-6], exif6), 40, 30},
		{"a chunk length that would wrap", pngOfParts(40, 30, huge), 40, 30},
		{"the eXIf chunk cut short", pngOfParts(40, 30, exif6[:len(exif6)-9]), 40, 30},
		{"a chunk header cut short", pngOfParts(40, 30, []byte{0, 0, 0}), 40, 30},
		{"a chunk header cut after its length and one byte", pngOfParts(40, 30, []byte{0, 0, 0, 0, 'e'}), 40, 30},
		{"a chunk header cut one byte short of whole", pngOfParts(40, 30, []byte{0, 0, 0, 0, 'e', 'X', 'I'}), 40, 30},
		{"the eXIf chunk with its CRC cut off", pngOfParts(40, 30, exif6[:len(exif6)-4]), 40, 30},
		{"the eXIf chunk with its CRC cut off part-way", pngOfParts(40, 30, exif6[:len(exif6)-2]), 40, 30},
		{"the first eXIf decides", pngOfParts(40, 30, pngChunk("eXIf", tiffOf(true, otherEntry())), exif6, idat), 40, 30},
		{"the block with the EXIF prefix, as some writers leave it", pngOfParts(40, 30, pngChunk("eXIf", append([]byte(exifHeader), photoTIFF(true, 6)...)), idat), 30, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w, h := mustDimensions(t, "image/png", tc.data); w != tc.w || h != tc.h {
				t.Errorf("got %dx%d, want %dx%d", w, h, tc.w, tc.h)
			}
		})
	}
}

// ---- WebP --------------------------------------------------------------------

func TestWebPOrientationIsReadFromTheEXIFChunkOfAnExtendedFile(t *testing.T) {
	tiff6 := photoTIFF(true, 6)
	anmf := webpChunk("ANMF", make([]byte, 31)) // odd length: carries a pad byte
	odd := webpChunk("ICCP", []byte("odd"))
	exifLen := func(d []byte) int { return len(d) - len(tiff6) - 4 } // where the EXIF chunk's length sits
	for _, tc := range []struct {
		name string
		data []byte
		w, h int
	}{
		{"EXIF after the image", webpWithExif(40, 30, tiff6), 30, 40},
		{"EXIF straight after VP8X", webpOfChunks(vp8xChunk(vp8xExifFlag, 40, 30), webpChunk("EXIF", tiff6), vp8lChunk(40, 30)), 30, 40},
		{"EXIF with the prefix some writers keep", webpWithExif(40, 30, append([]byte(exifHeader), tiff6...)), 30, 40},
		{"EXIF past an odd-length chunk and its pad byte", webpOfChunks(vp8xChunk(vp8xExifFlag, 40, 30), odd, vp8lChunk(40, 30), webpChunk("EXIF", tiff6)), 30, 40},
		{"EXIF past an animation's frames", webpOfChunks(vp8xChunk(vp8xExifFlag|0x02, 40, 30), webpChunk("ANIM", make([]byte, 6)), anmf, anmf, webpChunk("EXIF", tiff6)), 30, 40},
		{"EXIF chunk with the flag unset", webpOfChunks(vp8xChunk(0, 40, 30), vp8lChunk(40, 30), webpChunk("EXIF", tiff6)), 40, 30},
		{"the flag set and no chunk", webpOfChunks(vp8xChunk(vp8xExifFlag, 40, 30), vp8lChunk(40, 30)), 40, 30},
		{"a simple file has no EXIF", webpOfChunks(vp8lChunk(40, 30), webpChunk("EXIF", tiff6)), 40, 30},
		{"the first EXIF chunk decides", webpOfChunks(vp8xChunk(vp8xExifFlag, 40, 30), vp8lChunk(40, 30), webpChunk("EXIF", tiffOf(true, otherEntry())), webpChunk("EXIF", tiff6)), 40, 30},
		{"an EXIF chunk longer than the file", func() []byte {
			d := webpWithExif(40, 30, tiff6)
			binary.LittleEndian.PutUint32(d[exifLen(d):], 1<<20)
			return d
		}(), 40, 30},
		{"an EXIF chunk length that would wrap", func() []byte {
			d := webpWithExif(40, 30, tiff6)
			binary.LittleEndian.PutUint32(d[exifLen(d):], 0xffffffff)
			return d
		}(), 40, 30},
		{"a chunk before it whose length would wrap", webpOfChunks(vp8xChunk(vp8xExifFlag, 40, 30), append([]byte("JUNK\xff\xff\xff\xff"), 0, 0), webpChunk("EXIF", tiff6)), 40, 30},
		{"EXIF past the end the RIFF header declares", func() []byte {
			d := webpWithExif(40, 30, tiff6)
			binary.LittleEndian.PutUint32(d[4:], uint32(len(d)-8-len(tiff6)-8)) // ends before the EXIF chunk
			return d
		}(), 40, 30},
		{"a RIFF size larger than the data", func() []byte {
			d := webpWithExif(40, 30, tiff6)
			binary.LittleEndian.PutUint32(d[4:], 0xffffffff)
			return d
		}(), 30, 40},
		{"bytes after the RIFF data are not read", append(webpOfChunks(vp8xChunk(vp8xExifFlag, 40, 30), vp8lChunk(40, 30)), webpChunk("EXIF", tiff6)...), 40, 30},
		{"a chunk header cut short at the end", webpOfChunks(vp8xChunk(vp8xExifFlag, 40, 30), vp8lChunk(40, 30), []byte("EXI")), 40, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w, h := mustDimensions(t, "image/webp", tc.data); w != tc.w || h != tc.h {
				t.Errorf("got %dx%d, want %dx%d", w, h, tc.w, tc.h)
			}
		})
	}
}

// ---- the parts ---------------------------------------------------------------

func TestQuarterTurned(t *testing.T) {
	for o := -1; o <= 10; o++ {
		if got, want := quarterTurned(o), o >= 5 && o <= 8; got != want {
			t.Errorf("quarterTurned(%d) = %v, want %v", o, got, want)
		}
	}
}

// A value in the entry is read in the file's own byte order: 6 written big-endian is the
// bytes 00 06, which read the wrong way round is 1536.
func TestTIFFOrientationReadsTheValueInTheFilesByteOrder(t *testing.T) {
	for _, little := range []bool{true, false} {
		for o := uint16(1); o <= 8; o++ {
			if got := tiffOrientation(photoTIFF(little, o)); got != int(o) {
				t.Errorf("little=%v: tiffOrientation = %d, want %d", little, got, o)
			}
		}
	}
	if got := tiffOrientation(nil); got != 0 {
		t.Errorf("nil: %d", got)
	}
}

// Only 1 to 8 are orientations; anything else is none, and is not passed on for a caller
// to have to know about.
func TestTIFFOrientationIsNoneOutsideOneToEight(t *testing.T) {
	for _, v := range []uint16{0, 9, 10, 0x0100, 0x0600, 0xffff} {
		for _, little := range []bool{true, false} {
			if got := tiffOrientation(photoTIFF(little, v)); got != 0 {
				t.Errorf("little=%v value %#x: tiffOrientation = %d, want 0", little, v, got)
			}
		}
	}
}

// A directory that ends exactly where the block does, with no pointer to a next one, is
// whole: its entries are all there. It is common enough (writers that do not emit IFD1)
// that refusing it would lose real orientations.
func TestADirectoryThatEndsExactlyAtTheEndOfTheBlockIsWhole(t *testing.T) {
	for _, little := range []bool{true, false} {
		full := photoTIFF(little, 6)
		exact := full[: len(full)-4 : len(full)-4]
		if got := tiffOrientation(exact); got != 6 {
			t.Errorf("little=%v: got %d, want 6", little, got)
		}
		if got := tiffOrientation(exact[: len(exact)-1 : len(exact)-1]); got != 0 {
			t.Errorf("little=%v: one byte short gave %d, want 0", little, got)
		}
		for _, c := range containers {
			if w, h := mustDimensions(t, c.mime, c.make(40, 30, exact)); w != 30 || h != 40 {
				t.Errorf("%s little=%v: got %dx%d, want 30x40", c.name, little, w, h)
			}
		}
	}
}

// Slices cut with their capacity clipped to their length, so that reading past the end is
// a panic and not a quiet read of the bytes that happen to follow in memory (inside a
// file, a block is a window onto the rest of it, and those bytes are there). A block is
// an orientation exactly when everything up to the end of its directory is present.
func TestTIFFOrientationOnEveryClippedPrefix(t *testing.T) {
	for _, little := range []bool{true, false} {
		full := photoTIFF(little, 6)
		whole := len(full) - 4 // the directory ends here; the next-IFD pointer is not needed
		for n := 0; n <= len(full); n++ {
			want := 0
			if n >= whole {
				want = 6
			}
			if got := tiffOrientation(full[:n:n]); got != want {
				t.Errorf("little=%v, %d of %d bytes: got %d, want %d", little, n, len(full), got, want)
			}
		}
	}
}

// The chunk walks are handed the whole file by Dimensions, but they do not lean on that:
// each one, on a clipped prefix of every length, neither panics nor reads past what it
// was given, and finds the orientation only once the chunk that holds it is whole.
func TestTheChunkWalksOnEveryClippedPrefix(t *testing.T) {
	tiff := photoTIFF(true, 6)
	for _, tc := range []struct {
		name  string
		walk  func([]byte) int
		data  []byte
		whole int // the shortest prefix that holds the EXIF chunk whole
	}{
		{"png", pngOrientation, pngOfParts(40, 30, pngChunk("eXIf", tiff), pngChunk("IDAT", []byte{0})), 8 + 25 + 12 + len(tiff)},
		{"webp", webpOrientation, webpWithExif(40, 30, tiff), 12 + 18 + 18 + 8 + len(tiff)},
	} {
		for n := 0; n <= len(tc.data); n++ {
			want := 0
			if n >= tc.whole {
				want = 6
			}
			if got := tc.walk(tc.data[:n:n]); got != want {
				t.Errorf("%s, %d of %d bytes: got %d, want %d", tc.name, n, len(tc.data), got, want)
			}
		}
	}
}
