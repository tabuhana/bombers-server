package imagesize

import "encoding/binary"

// Which way up a photograph is turned lives in its EXIF block, as the Orientation tag.
// A phone writes the sensor's pixels as they came off it and records here how to turn
// them, and every browser applies that turn when it draws. Orientations 5, 6, 7 and 8
// are the ones that turn the picture a quarter way round, so the shape it is drawn in
// has its width and height exchanged; 1 to 4 are the identity and the mirrors, which do
// not. Dimensions returns the DRAWN shape, because the one caller lays a box out from
// it before the picture arrives, and a client then needs no knowledge of EXIF at all.
//
// An orientation is never required. What follows reads it where it can be read, and
// everything else (no block, a block that is not EXIF, a byte order that is neither,
// an offset or a count that does not fit the data held, a tag of the wrong type) is the
// same answer: 0, "none". A file whose EXIF is damaged still has a usable size, so none
// of it is an error, and the bytes are attacker-supplied: every offset is held in a
// uint64, since int is 32 bits on the 386 build and an offset is a full uint32, and is
// checked against the length of what is actually there before it is used to slice.

// exifOrientationTag is the tag number of Orientation in IFD0, and tiffMagic the 42 a
// TIFF header carries after its byte-order mark.
const (
	exifOrientationTag = 0x0112
	tiffShort          = 3 // the TIFF field type of an unsigned 16-bit integer
	tiffMagic          = 42
	ifdEntrySize       = 12
)

// quarterTurned reports whether an orientation draws the picture with its sides
// exchanged. 0 (none) is not one.
func quarterTurned(orientation int) bool { return orientation >= 5 && orientation <= 8 }

// tiffOrientation reads the Orientation tag out of a TIFF structure, which is what an
// EXIF block is once the container's own wrapping is taken off: a header (II or MM for
// the byte order, 42, and the offset of the first image file directory) and then the
// directory, a count and that many 12-byte entries of tag, type, count and value.
// Offsets are from the start of tiff. Only IFD0 is read: the thumbnail's directory,
// further on, carries the thumbnail's orientation, which is not the picture's.
//
// It returns 1 to 8, or 0 for no orientation. The tag must be one SHORT, as the
// specification has it; a value outside 1 to 8 is none, which is what a decoder draws
// for it as well.
func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	if order.Uint16(tiff[2:4]) != tiffMagic {
		return 0
	}
	held := uint64(len(tiff))
	ifd := uint64(order.Uint32(tiff[4:8]))
	// The directory cannot start inside the header, and has to hold its own count.
	if ifd < 8 || ifd+2 > held {
		return 0
	}
	count := uint64(order.Uint16(tiff[ifd : ifd+2]))
	first := ifd + 2
	// Every entry the directory declares has to be there. A directory that runs off the
	// end is damaged as a whole, and nothing in it is trusted.
	if first+count*ifdEntrySize > held {
		return 0
	}
	for i := uint64(0); i < count; i++ {
		at := first + i*ifdEntrySize
		entry := tiff[at : at+ifdEntrySize]
		if order.Uint16(entry[0:2]) != exifOrientationTag {
			continue
		}
		// One SHORT: a value that small sits in the entry itself, in the first two of
		// its four bytes, whichever byte order the file is in.
		if order.Uint16(entry[2:4]) != tiffShort || order.Uint32(entry[4:8]) != 1 {
			return 0
		}
		if v := int(order.Uint16(entry[8:10])); v >= 1 && v <= 8 {
			return v
		}
		return 0
	}
	return 0
}

// exifHeader is what opens an EXIF block inside a JPEG's APP1 segment, ahead of the
// TIFF header. A PNG's eXIf chunk has no such prefix, a WebP's EXIF chunk may or may
// not, and a TIFF header starts with II or MM, so the two cannot be mistaken.
const exifHeader = "Exif\x00\x00"

// exifOrientation reads the orientation out of an EXIF block with or without the
// "Exif\0\0" prefix.
func exifOrientation(block []byte) int {
	if len(block) >= len(exifHeader) && string(block[:len(exifHeader)]) == exifHeader {
		block = block[len(exifHeader):]
	}
	return tiffOrientation(block)
}

// pngOrientation looks for the eXIf chunk between IHDR and the first IDAT, where a
// decoder looks for it, and reads the orientation out of it. The chunks before the
// image data are few, but the walk is bounded by the data held in any case: each step
// is checked to fit before it is taken, and a chunk that runs off the end ends the
// walk with none.
func pngOrientation(data []byte) int {
	held := uint64(len(data))
	pos := uint64(8) // past the signature; pngSize has already checked it and IHDR
	for pos+8 <= held {
		length := uint64(binary.BigEndian.Uint32(data[pos : pos+4]))
		kind := string(data[pos+4 : pos+8])
		body := pos + 8
		// The chunk is whole only with its CRC after it; a file cut inside the CRC is a damaged one.
		if body+length+4 > held {
			return 0
		}
		switch kind {
		case "eXIf":
			return exifOrientation(data[body : body+length])
		case "IDAT", "IEND":
			return 0
		}
		pos = body + length + 4 // the chunk's CRC, which is not checked: nothing here decodes
	}
	return 0
}

// webpOrientation looks for the EXIF chunk of an extended WebP. Only a file that opens
// with VP8X can have one, and the VP8X flags say whether it does (bit 3), which is what
// a decoder goes by. The chunk follows the image data, so this steps over the chunks
// between: each is a fourcc, a little-endian length, the body and a pad byte if the
// length is odd. An animation's frames are chunks of their own and are stepped over
// whole. The walk ends at the end of the RIFF data the file declares, or of the bytes
// held if that is less, and a chunk that runs past either ends it with none.
func webpOrientation(data []byte) int {
	const (
		header    = 12 // RIFF, its size, WEBP
		exifFlag  = 0x08
		chunkHead = 8
	)
	held := uint64(len(data))
	if held < header+chunkHead+10 || string(data[12:16]) != "VP8X" {
		return 0
	}
	if data[header+chunkHead]&exifFlag == 0 {
		return 0
	}
	end := held
	if declared := 8 + uint64(binary.LittleEndian.Uint32(data[4:8])); declared < end {
		end = declared
	}
	pos := uint64(header)
	for pos+chunkHead <= end {
		length := uint64(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		body := pos + chunkHead
		if body+length > end {
			return 0
		}
		if string(data[pos:pos+4]) == "EXIF" {
			return exifOrientation(data[body : body+length])
		}
		pos = body + length + length&1
	}
	return 0
}
