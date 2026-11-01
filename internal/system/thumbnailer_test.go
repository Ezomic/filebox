package system

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFakeThumbnailerDrawsAJPEG(t *testing.T) {
	files := NewFakeFiles()
	put(t, files, "a.jpg", "pixels")
	put(t, files, "bad.jpg", "BAD data")
	put(t, files, "clip.mp4", "movie")
	fake := NewFakeThumbnailer()

	render := func(name, kind string) ([]byte, error) {
		file, _, err := files.OpenPhoto(name)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()

		return fake.Render(context.Background(), file, kind, ThumbSide)
	}
	data, err := render("a.jpg", PhotoImage)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() > ThumbSide {
		t.Fatalf("not a thumbnail: %v %v", img, err)
	}
	if video, err := render("clip.mp4", PhotoVideo); err != nil || bytes.Equal(video, data) {
		t.Fatalf("video poster: %v", err)
	}
	if _, err := render("bad.jpg", PhotoImage); !errors.Is(err, ErrBadMedia) {
		t.Fatalf("corrupt file: %v", err)
	}
	if fake.Calls() != 3 {
		t.Fatalf("calls %d", fake.Calls())
	}
}

func TestFakeThumbnailerStopsWhenTheRequestIsCancelled(t *testing.T) {
	files := NewFakeFiles()
	put(t, files, "a.jpg", "pixels")
	file, _, _ := files.OpenPhoto("a.jpg")
	fake := &FakeThumbnailer{Delay: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	var err error
	go func() {
		defer wg.Done()
		_, err = fake.Render(ctx, file, PhotoImage, ThumbSide)
	}()
	cancel()
	wg.Wait()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestToolArgumentsAreFixedAndTakeTheSourceFromADescriptor(t *testing.T) {
	vips := vipsArgs(400, "/tmp/x/out.jpg")
	if vips[0] != procSource || !strings.HasSuffix(vips[len(vips)-1], "out.jpg[Q=82,strip]") {
		t.Fatalf("vips args %v", vips)
	}
	for _, args := range ffmpegArgs(400, "/tmp/x/out.jpg") {
		input := -1
		for i, arg := range args {
			if arg == "-i" {
				input = i
			}
		}
		if input < 0 || args[input+1] != ffmpegSource || args[len(args)-1] != "/tmp/x/out.jpg" || args[0] != "-nostdin" {
			t.Fatalf("ffmpeg args %v", args)
		}
	}
}
