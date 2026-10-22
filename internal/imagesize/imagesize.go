// Package imagesize reads the width and height of an image out of its header, for
// the four formats this server accepts (PNG, JPEG, GIF, WebP) and for nothing else.
//
// It exists because the dimensions of a picture sent in a message have to come from
// the bytes: a client that declared them would be a client declaring payload, which
// is the forged-state hole message kinds exist to close. The standard library's
// image.DecodeConfig covers three of the four formats and not WebP, and
// golang.org/x/image/webp would be a new dependency that still refuses an animated
// WebP, which a person will send.
//
// It never decodes an image. Only the few bytes of header that hold the size are
// read, so a huge or hostile file costs nothing beyond the bytes the caller already
// capped. Every offset is bounds-checked and every declared length is checked against
// the data that is actually there: a short, truncated or lying file is an error and
// never a slice panic. A size of zero is an error too, since no picture has one.
//
// The size returned is the one a browser DRAWS: a photo with an EXIF orientation that
// turns it a quarter way round comes back with its sides exchanged (exif.go). That is
// read where it sits, and only ever refines the answer: a missing or damaged orientation
// is no turn and never an error.
//
// A leaf: standard library only.
package imagesize

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ErrUnreadable is the one error this package returns. What is wrong with the file is
// in the wrapped text, for a log; callers act on the fact, not on the reason.
var ErrUnreadable = errors.New("imagesize: cannot read the dimensions")

func unreadable(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnreadable, fmt.Sprintf(format, args...))
}

// Accepts reports whether mime, as http.DetectContentType names it, is one of the four
// formats Dimensions can read. It is the single accept-list for pictures: profile media
// and message images both ask it, so the two cannot disagree about what a picture is.
func Accepts(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// Dimensions returns the pixel width and height of data as a browser will DRAW it,
// which the caller has already sniffed to be mime. Both are at least 1.
//
// That is not always the stored size: a photo from a phone is stored as the sensor saw
// it with an EXIF orientation saying how to turn it, and for the four orientations that
// turn it a quarter way round (5 to 8) the stored width is the drawn height. The turn
// is applied here, for PNG (eXIf), JPEG (APP1) and WebP (EXIF) alike, so that a client
// laying out a box from these numbers needs no knowledge of EXIF at all; GIF has none.
// A missing or damaged orientation is no turn and never an error (see exif.go).
func Dimensions(mime string, data []byte) (width, height int, err error) {
	orientation := 0
	switch mime {
	case "image/png":
		if width, height, err = pngSize(data); err == nil {
			orientation = pngOrientation(data)
		}
	case "image/gif":
		width, height, err = gifSize(data)
	case "image/webp":
		if width, height, err = webpSize(data); err == nil {
			orientation = webpOrientation(data)
		}
	case "image/jpeg":
		width, height, orientation, err = jpegSize(data)
	default:
		return 0, 0, unreadable("%q is not a format this package reads", mime)
	}
	if err != nil {
		return 0, 0, err
	}
	if quarterTurned(orientation) {
		width, height = height, width
	}
	return width, height, nil
}

// positive is the last check every format makes: a picture is at least one pixel each way.
func positive(w, h int) (int, int, error) {
	if w < 1 || h < 1 {
		return 0, 0, unreadable("a dimension is zero (%dx%d)", w, h)
	}
	return w, h, nil
}

// pngSize reads IHDR. A PNG is an 8-byte signature and then chunks, and IHDR must be
// the first: its length is 13, its type "IHDR", and its first eight bytes are width and
// height as big-endian uint32s, so they sit at a fixed offset. The specification limits
// each to 2^31-1, which also keeps the value inside an int on a 32-bit build.
func pngSize(data []byte) (int, int, error) {
	const (
		signature = "\x89PNG\r\n\x1a\n"
		need      = 8 + 4 + 4 + 8 // signature, chunk length, chunk type, width + height
	)
	if len(data) < need {
		return 0, 0, unreadable("png is shorter than its IHDR (%d bytes)", len(data))
	}
	if string(data[:8]) != signature {
		return 0, 0, unreadable("png signature")
	}
	if binary.BigEndian.Uint32(data[8:12]) != 13 || string(data[12:16]) != "IHDR" {
		return 0, 0, unreadable("png does not begin with an IHDR of 13 bytes")
	}
	w, h := binary.BigEndian.Uint32(data[16:20]), binary.BigEndian.Uint32(data[20:24])
	if w > math.MaxInt32 || h > math.MaxInt32 {
		return 0, 0, unreadable("png dimension is past 2^31-1")
	}
	return positive(int(w), int(h))
}

// gifSize reads the logical screen descriptor: after the 6-byte "GIF87a"/"GIF89a"
// header, width and height are little-endian uint16s. That is the canvas, which is what
// an animated GIF is drawn on, so it is the size to lay out.
func gifSize(data []byte) (int, int, error) {
	if len(data) < 10 {
		return 0, 0, unreadable("gif is shorter than its screen descriptor (%d bytes)", len(data))
	}
	if sig := string(data[:6]); sig != "GIF87a" && sig != "GIF89a" {
		return 0, 0, unreadable("gif signature")
	}
	return positive(int(binary.LittleEndian.Uint16(data[6:8])), int(binary.LittleEndian.Uint16(data[8:10])))
}

// webpSize reads the first chunk of the RIFF container, which says how big the picture
// is in one of three ways:
//
//   - VP8X, the extended format, holds the canvas as two 24-bit little-endian numbers,
//     each one less than the size. This is the chunk an animated WebP begins with, which
//     is why it is the one worth having.
//   - VP8L, lossless, packs width-1 and height-1 into 14 bits each after a one-byte
//     signature.
//   - "VP8 ", lossy, has them in the key frame header, 14 bits each with two spare
//     scaling bits on top that are not part of the size.
//
// The first chunk's declared length has to fit in the data (a truncated file is the
// likeliest way to meet a length that does not), and has to be long enough to hold the
// fields read from it.
func webpSize(data []byte) (int, int, error) {
	const header = 12 + 8 // RIFF header, then the first chunk's fourcc and length
	if len(data) < header {
		return 0, 0, unreadable("webp is shorter than its first chunk header (%d bytes)", len(data))
	}
	if string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, unreadable("webp signature")
	}
	chunk := binary.LittleEndian.Uint32(data[16:20])
	// uint64 so a lying length cannot wrap on a 32-bit build.
	if uint64(header)+uint64(chunk) > uint64(len(data)) {
		return 0, 0, unreadable("webp first chunk runs past the end of the file")
	}
	body := data[header : header+int(chunk)]

	switch string(data[12:16]) {
	case "VP8X":
		if len(body) < 10 {
			return 0, 0, unreadable("webp VP8X chunk is %d bytes, want at least 10", len(body))
		}
		w := int(body[4]) | int(body[5])<<8 | int(body[6])<<16
		h := int(body[7]) | int(body[8])<<8 | int(body[9])<<16
		return positive(w+1, h+1)
	case "VP8L":
		if len(body) < 5 {
			return 0, 0, unreadable("webp VP8L chunk is %d bytes, want at least 5", len(body))
		}
		if body[0] != 0x2f {
			return 0, 0, unreadable("webp VP8L signature byte")
		}
		bits := binary.LittleEndian.Uint32(body[1:5])
		return positive(int(bits&0x3fff)+1, int((bits>>14)&0x3fff)+1)
	case "VP8 ":
		if len(body) < 10 {
			return 0, 0, unreadable("webp VP8 chunk is %d bytes, want at least 10", len(body))
		}
		// Bit 0 of the frame tag is 0 for a key frame, the only kind that opens a file.
		if body[0]&1 != 0 {
			return 0, 0, unreadable("webp VP8 does not begin with a key frame")
		}
		if body[3] != 0x9d || body[4] != 0x01 || body[5] != 0x2a {
			return 0, 0, unreadable("webp VP8 start code")
		}
		return positive(int(binary.LittleEndian.Uint16(body[6:8])&0x3fff), int(binary.LittleEndian.Uint16(body[8:10])&0x3fff))
	}
	return 0, 0, unreadable("webp first chunk %q carries no size", data[12:16])
}

// jpegSize walks the marker segments from the start of the file to the first
// start-of-frame (SOF) and reads height then width from it, and takes the EXIF
// orientation from the first APP1 segment that holds an EXIF block on the same pass.
// SOF0-3, 5-7, 9-11 and 13-15 are the frame headers (baseline, extended, progressive,
// lossless and their arithmetic-coded variants); 0xC4 (DHT), 0xC8 (JPG) and 0xCC (DAC)
// sit in the same range and are not.
//
// Between segments a marker may be preceded by any number of 0xFF fill bytes, and a few
// markers have no length at all, so they are stepped over by themselves. Every other
// segment declares a length that includes its own two bytes; a segment that runs past
// the end of the data is an error. The frame header has to come before the start of
// scan (0xDA), after which the bytes are entropy-coded image data that no marker walk
// can follow, so reaching a scan or the end of the image first means there is no size.
//
// The walk goes on past the frame header to the scan, because a decoder reads every
// marker up to there and so honours an EXIF block that sits after it. Once the size is
// in hand nothing further can fail the read: a damaged segment, the end of the data or
// a scan end the walk with the size and whatever orientation was found.
func jpegSize(data []byte) (width, height, orientation int, err error) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 0, 0, 0, unreadable("jpeg signature")
	}
	var haveFrame, haveExif bool
	// stop ends the walk where it cannot go on: an error if there is no size yet, the
	// answer so far if there is.
	stop := func(format string, args ...any) (int, int, int, error) {
		if haveFrame {
			return width, height, orientation, nil
		}
		return 0, 0, 0, unreadable(format, args...)
	}
	i := 2
	for {
		// Find the next marker: one 0xFF, then any run of further 0xFF fill, then the code.
		if i >= len(data) || data[i] != 0xff {
			return stop("jpeg: expected a marker at byte %d", i)
		}
		for i < len(data) && data[i] == 0xff {
			i++
		}
		if i >= len(data) {
			return stop("jpeg ends inside a marker")
		}
		code := data[i]
		i++

		switch {
		case code == 0x00:
			return stop("jpeg: a stuffed zero where a marker should be")
		case code == 0x01, code >= 0xd0 && code <= 0xd8:
			// TEM, RSTn and SOI stand alone: no length follows.
			continue
		case code == 0xd9, code == 0xda:
			return stop("jpeg has no frame header before its scan")
		}

		if i+2 > len(data) {
			return stop("jpeg ends inside a segment length")
		}
		length := int(binary.BigEndian.Uint16(data[i : i+2]))
		if length < 2 || i+length > len(data) {
			return stop("jpeg segment 0x%02x runs past the end of the file", code)
		}
		switch {
		case isSOF(code) && !haveFrame:
			// length, precision, height, width, components: 8 bytes at least.
			if length < 8 {
				return 0, 0, 0, unreadable("jpeg frame header is %d bytes, want at least 8", length)
			}
			h := int(binary.BigEndian.Uint16(data[i+3 : i+5]))
			w := int(binary.BigEndian.Uint16(data[i+5 : i+7]))
			var perr error
			if width, height, perr = positive(w, h); perr != nil {
				return 0, 0, 0, perr
			}
			haveFrame = true
		case code == 0xe1 && !haveExif:
			// APP1 also carries XMP, which is not EXIF. The first EXIF block is the one
			// used, whether or not it yields an orientation: a second would be a file
			// that disagrees with itself, and a decoder does not go looking for it.
			if block := data[i+2 : i+length]; len(block) >= len(exifHeader) && string(block[:len(exifHeader)]) == exifHeader {
				haveExif = true
				orientation = exifOrientation(block)
			}
		}
		i += length
	}
}

func isSOF(code byte) bool {
	return code >= 0xc0 && code <= 0xcf && code != 0xc4 && code != 0xc8 && code != 0xcc
}
