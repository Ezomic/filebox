package system

import (
	"bytes"
	"encoding/binary"
	"io"
	"time"
)

const (
	exifJPEGWindow = 256 << 10
	exifMetaLimit  = 4 << 20
	exifItemLimit  = 1 << 20
	exifBoxLimit   = 64

	tagExifIFD           = 0x8769
	tagDateTimeOriginal  = 0x9003
	exifDateLayout       = "2006:01:02 15:04:05"
	exifDateLength       = len(exifDateLayout)
	tiffTypeASCII        = 2
	tiffEntrySize        = 12
	maxTIFFEntriesPerIFD = 4096
)

// exifDate reads DateTimeOriginal from the start of a JPEG or from the Exif item of a HEIC, HEIF or AVIF file.
// Anything unexpected is a quiet miss, because the caller falls back to the modification time.
func exifDate(r io.ReaderAt, size int64, ext string) (time.Time, bool) {
	var tiff []byte
	switch ext {
	case "jpg", "jpeg":
		tiff = jpegTIFF(r, size)
	case "heic", "heif", "avif":
		tiff = isoTIFF(r, size)
	}
	if tiff == nil {
		return time.Time{}, false
	}

	return tiffDate(tiff)
}

func jpegTIFF(r io.ReaderAt, size int64) []byte {
	data := make([]byte, min(size, exifJPEGWindow))
	n, _ := r.ReadAt(data, 0)
	data = data[:n]
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil
	}

	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return nil
		}
		marker := data[i+1]
		switch {
		case marker == 0xFF:
			i++
			continue
		case marker == 0xDA || marker == 0xD9:
			return nil
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			i += 2
			continue
		}
		length := int(binary.BigEndian.Uint16(data[i+2:]))
		if length < 2 {
			return nil
		}
		end := min(i+2+length, len(data))
		if marker == 0xE1 && bytes.HasPrefix(data[i+4:end], []byte("Exif\x00\x00")) {
			return data[i+10 : end]
		}
		i += 2 + length
	}

	return nil
}

func tiffDate(tiff []byte) (time.Time, bool) {
	var order binary.ByteOrder
	switch {
	case len(tiff) < 8:
		return time.Time{}, false
	case tiff[0] == 'I' && tiff[1] == 'I':
		order = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		order = binary.BigEndian
	default:
		return time.Time{}, false
	}
	if order.Uint16(tiff[2:]) != 42 {
		return time.Time{}, false
	}

	pointer, ok := tiffEntry(tiff, order, order.Uint32(tiff[4:]), tagExifIFD)
	if !ok {
		return time.Time{}, false
	}
	entry, ok := tiffEntry(tiff, order, order.Uint32(pointer[8:]), tagDateTimeOriginal)
	if !ok || order.Uint16(entry[2:]) != tiffTypeASCII || int(order.Uint32(entry[4:])) < exifDateLength {
		return time.Time{}, false
	}
	text, ok := tiffValue(tiff, order, entry, exifDateLength)
	if !ok {
		return time.Time{}, false
	}
	parsed, err := time.Parse(exifDateLayout, string(text))
	if err != nil || parsed.Year() < 1900 {
		return time.Time{}, false
	}

	return parsed, true
}

func tiffEntry(tiff []byte, order binary.ByteOrder, ifd uint32, tag uint16) ([]byte, bool) {
	if uint64(ifd)+2 > uint64(len(tiff)) {
		return nil, false
	}
	count := min(int(order.Uint16(tiff[ifd:])), maxTIFFEntriesPerIFD)
	for i := range count {
		start := uint64(ifd) + 2 + uint64(i)*tiffEntrySize
		if start+tiffEntrySize > uint64(len(tiff)) {
			return nil, false
		}
		if entry := tiff[start : start+tiffEntrySize]; order.Uint16(entry) == tag {
			return entry, true
		}
	}

	return nil, false
}

func tiffValue(tiff []byte, order binary.ByteOrder, entry []byte, length int) ([]byte, bool) {
	if length <= 4 {
		return entry[8 : 8+length], true
	}
	offset := uint64(order.Uint32(entry[8:]))
	if offset+uint64(length) > uint64(len(tiff)) {
		return nil, false
	}

	return tiff[offset : offset+uint64(length)], true
}

type isoBox struct {
	kind    string
	payload []byte
}

func isoBoxes(data []byte) []isoBox {
	var boxes []isoBox
	for len(data) >= 8 && len(boxes) < exifBoxLimit {
		size, header := uint64(binary.BigEndian.Uint32(data)), 8
		switch size {
		case 1:
			if len(data) < 16 {
				return boxes
			}
			size, header = binary.BigEndian.Uint64(data[8:]), 16
		case 0:
			size = uint64(len(data))
		}
		if size < uint64(header) || size > uint64(len(data)) {
			return boxes
		}
		boxes = append(boxes, isoBox{kind: string(data[4:8]), payload: data[header:size]})
		data = data[size:]
	}

	return boxes
}

func isoTIFF(r io.ReaderAt, size int64) []byte {
	meta := isoMeta(r, size)
	if meta == nil || len(meta) < 4 {
		return nil
	}

	var exifID uint32
	var location []byte
	for _, box := range isoBoxes(meta[4:]) {
		switch box.kind {
		case "iinf":
			exifID = exifItemID(box.payload)
		case "iloc":
			location = box.payload
		}
	}
	if exifID == 0 || location == nil {
		return nil
	}
	offset, length, ok := itemExtent(location, exifID)
	if !ok || length < 8 || length > exifItemLimit || offset+length > uint64(size) {
		return nil
	}
	item := make([]byte, length)
	if n, err := r.ReadAt(item, int64(offset)); n != len(item) && err != nil {
		return nil
	}
	skip := uint64(binary.BigEndian.Uint32(item))
	if skip > uint64(len(item))-4 {
		return nil
	}

	return item[4+skip:]
}

func isoMeta(r io.ReaderAt, size int64) []byte {
	var offset int64
	for range exifBoxLimit {
		header := make([]byte, 16)
		n, _ := r.ReadAt(header, offset)
		if n < 8 {
			return nil
		}
		boxSize, headerSize := int64(binary.BigEndian.Uint32(header)), int64(8)
		switch boxSize {
		case 1:
			if n < 16 {
				return nil
			}
			large := binary.BigEndian.Uint64(header[8:])
			if large > uint64(size) {
				return nil
			}
			boxSize, headerSize = int64(large), 16
		case 0:
			boxSize = size - offset
		}
		if boxSize < headerSize || offset+boxSize > size {
			return nil
		}
		if string(header[4:8]) == "meta" {
			if boxSize-headerSize > exifMetaLimit {
				return nil
			}
			payload := make([]byte, boxSize-headerSize)
			if n, err := r.ReadAt(payload, offset+headerSize); n != len(payload) && err != nil {
				return nil
			}

			return payload
		}
		offset += boxSize
	}

	return nil
}

func exifItemID(iinf []byte) uint32 {
	if len(iinf) < 6 {
		return 0
	}
	skip := 6
	if iinf[0] != 0 {
		skip = 8
	}
	if len(iinf) < skip {
		return 0
	}
	for _, box := range isoBoxes(iinf[skip:]) {
		payload := box.payload
		if box.kind != "infe" || len(payload) < 4 || payload[0] < 2 {
			continue
		}
		var id uint32
		rest := payload[4:]
		if payload[0] == 2 && len(rest) >= 8 {
			id, rest = uint32(binary.BigEndian.Uint16(rest)), rest[2:]
		} else if payload[0] >= 3 && len(rest) >= 10 {
			id, rest = binary.BigEndian.Uint32(rest), rest[4:]
		} else {
			continue
		}
		if string(rest[2:6]) == "Exif" {
			return id
		}
	}

	return 0
}

type isoReader struct {
	data []byte
	bad  bool
}

func (s *isoReader) uint(n int) uint64 {
	if n == 0 {
		return 0
	}
	if (n != 2 && n != 4 && n != 8) || len(s.data) < n {
		s.bad = true
		return 0
	}
	var value uint64
	for _, b := range s.data[:n] {
		value = value<<8 | uint64(b)
	}
	s.data = s.data[n:]

	return value
}

func itemExtent(iloc []byte, want uint32) (offset, length uint64, ok bool) {
	if len(iloc) < 6 {
		return 0, 0, false
	}
	version := iloc[0]
	if version > 2 {
		return 0, 0, false
	}
	offsetSize, lengthSize := int(iloc[4]>>4), int(iloc[4]&15)
	baseSize, indexSize := int(iloc[5]>>4), int(iloc[5]&15)
	if version == 0 {
		indexSize = 0
	}
	reader := &isoReader{data: iloc[6:]}
	count := reader.uint(2)
	if version == 2 {
		count = reader.uint(4)
	}
	for range min(count, 65536) {
		id := reader.uint(2)
		if version == 2 {
			id = reader.uint(4)
		}
		method := uint64(0)
		if version >= 1 {
			method = reader.uint(2) & 15
		}
		reader.uint(2)
		base := reader.uint(baseSize)
		extents := reader.uint(2)
		var firstOffset, firstLength uint64
		for i := range min(extents, 65536) {
			reader.uint(indexSize)
			extentOffset, extentLength := reader.uint(offsetSize), reader.uint(lengthSize)
			if i == 0 {
				firstOffset, firstLength = extentOffset, extentLength
			}
		}
		if reader.bad {
			return 0, 0, false
		}
		if uint32(id) == want {
			if method != 0 || extents == 0 || base+firstOffset < base {
				return 0, 0, false
			}

			return base + firstOffset, firstLength, true
		}
	}

	return 0, 0, false
}
