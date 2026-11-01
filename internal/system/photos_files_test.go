package system

import (
	"os"
	"testing"
	"time"
)

func TestExifDateFromRealCameraStyleFiles(t *testing.T) {
	want := time.Date(2019, 7, 4, 12, 30, 45, 0, time.UTC)
	for _, name := range []string{"sample.jpg", "sample.heic"} {
		file, err := os.Open("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		info, _ := file.Stat()
		got, ok := exifDate(file, info.Size(), mediaExt(name))
		_ = file.Close()
		if !ok || !got.Equal(want) {
			t.Errorf("%s: got %v, %v", name, got, ok)
		}
	}
	for _, name := range []string{"sample.png", "sample.webp", "rotated.jpg"} {
		file, _ := os.Open("testdata/" + name)
		info, _ := file.Stat()
		if got, ok := exifDate(file, info.Size(), mediaExt(name)); ok {
			t.Errorf("%s has no DateTimeOriginal but gave %v", name, got)
		}
		_ = file.Close()
	}
}
