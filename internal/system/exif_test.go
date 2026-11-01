package system

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func tiffWithDate(order binary.ByteOrder, date string) []byte {
	var buf bytes.Buffer
	if order == binary.LittleEndian {
		buf.WriteString("II")
	} else {
		buf.WriteString("MM")
	}
	put16 := func(v uint16) { _ = binary.Write(&buf, order, v) }
	put32 := func(v uint32) { _ = binary.Write(&buf, order, v) }
	put16(42)
	put32(8)
	put16(1)
	put16(tagExifIFD)
	put16(4)
	put32(1)
	put32(26)
	put32(0)
	put16(1)
	put16(tagDateTimeOriginal)
	put16(tiffTypeASCII)
	put32(uint32(len(date) + 1))
	put32(44)
	put32(0)
	buf.WriteString(date)
	buf.WriteByte(0)

	return buf.Bytes()
}

func jpegWithExif(tiff []byte) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x04, 'J', 'F'})
	segment := append([]byte("Exif\x00\x00"), tiff...)
	buf.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&buf, binary.BigEndian, uint16(len(segment)+2))
	buf.Write(segment)
	buf.Write([]byte{0xFF, 0xDA, 0x00, 0x02, 0x00})

	return buf.Bytes()
}

func box(kind string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:], kind)

	return append(out, body...)
}

func be(size int, value uint64) []byte {
	out := make([]byte, size)
	for i := size - 1; i >= 0; i-- {
		out[i] = byte(value)
		value >>= 8
	}

	return out
}

func heicWithExif(tiff []byte, ilocVersion byte) []byte {
	exifItem := append(be(4, 6), append([]byte("Exif\x00\x00"), tiff...)...)
	other := box("infe", []byte{2, 0, 0, 0}, be(2, 1), be(2, 0), []byte("hvc1"), []byte{0})
	exif := box("infe", []byte{2, 0, 0, 0}, be(2, 7), be(2, 0), []byte("Exif"), []byte{0})
	iinf := box("iinf", []byte{0, 0, 0, 0}, be(2, 2), other, exif)

	build := func(offset uint64) []byte {
		item := [][]byte{be(2, 7)}
		if ilocVersion == 1 {
			item = append(item, be(2, 0))
		}
		item = append(item, be(2, 0), be(2, 1), be(4, offset), be(4, uint64(len(exifItem))))
		iloc := box("iloc", []byte{ilocVersion, 0, 0, 0}, []byte{0x44, 0x00}, be(2, 1), bytes.Join(item, nil))

		return box("meta", []byte{0, 0, 0, 0}, iinf, iloc)
	}
	ftyp := box("ftyp", []byte("heic"), be(4, 0), []byte("mif1"))
	offset := uint64(len(ftyp) + len(build(0)) + 8)

	return bytes.Join([][]byte{ftyp, build(offset), box("mdat", exifItem)}, nil)
}

func TestExifDateFromJPEG(t *testing.T) {
	want := time.Date(2019, 7, 4, 12, 30, 45, 0, time.UTC)
	for name, order := range map[string]binary.ByteOrder{"little endian": binary.LittleEndian, "big endian": binary.BigEndian} {
		t.Run(name, func(t *testing.T) {
			data := jpegWithExif(tiffWithDate(order, "2019:07:04 12:30:45"))
			got, ok := exifDate(bytes.NewReader(data), int64(len(data)), "jpg")
			if !ok || !got.Equal(want) {
				t.Fatalf("got %v, %v", got, ok)
			}
		})
	}
}

func TestExifDateFromHEIC(t *testing.T) {
	want := time.Date(2021, 12, 31, 23, 59, 58, 0, time.UTC)
	for _, version := range []byte{0, 1} {
		data := heicWithExif(tiffWithDate(binary.BigEndian, "2021:12:31 23:59:58"), version)
		for _, ext := range []string{"heic", "heif", "avif"} {
			got, ok := exifDate(bytes.NewReader(data), int64(len(data)), ext)
			if !ok || !got.Equal(want) {
				t.Fatalf("version %d %s: got %v, %v", version, ext, got, ok)
			}
		}
	}
}

func TestExifDateMissesQuietly(t *testing.T) {
	good := jpegWithExif(tiffWithDate(binary.LittleEndian, "2019:07:04 12:30:45"))
	heic := heicWithExif(tiffWithDate(binary.BigEndian, "2021:12:31 23:59:58"), 0)
	cases := map[string][]byte{
		"empty":             nil,
		"plain text":        []byte("this is not an image at all"),
		"jpeg without exif": {0xFF, 0xD8, 0xFF, 0xDA, 0x00, 0x02, 0x00},
		"zero date":         jpegWithExif(tiffWithDate(binary.LittleEndian, "0000:00:00 00:00:00")),
		"garbage date":      jpegWithExif(tiffWithDate(binary.LittleEndian, "not a date at all")),
		"truncated jpeg":    good[:30],
		"truncated tiff":    good[:len(good)-20],
		"truncated heic":    heic[:len(heic)-20],
		"heic head only":    heic[:40],
	}
	for name, data := range cases {
		for _, ext := range []string{"jpg", "heic", "png"} {
			if got, ok := exifDate(bytes.NewReader(data), int64(len(data)), ext); ok {
				t.Errorf("%s as %s gave %v", name, ext, got)
			}
		}
	}
}

func FuzzExifDate(f *testing.F) {
	f.Add(jpegWithExif(tiffWithDate(binary.LittleEndian, "2019:07:04 12:30:45")), "jpg")
	f.Add(heicWithExif(tiffWithDate(binary.BigEndian, "2021:12:31 23:59:58"), 0), "heic")
	f.Add(heicWithExif(tiffWithDate(binary.BigEndian, "2021:12:31 23:59:58"), 1), "heic")
	f.Fuzz(func(t *testing.T, data []byte, ext string) {
		exifDate(bytes.NewReader(data), int64(len(data)), ext)
	})
}

func TestItemExtentIgnoresALocationTableWithoutFieldSizes(t *testing.T) {
	iloc := append([]byte{0, 0, 0, 0, 0x00, 0x00, 0xFF, 0xFF}, make([]byte, 400000)...)
	start := time.Now()
	if _, _, ok := itemExtent(iloc, 1); ok {
		t.Fatal("an empty location table found an item")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}
