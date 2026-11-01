package system

import (
	"bytes"
	"context"
	"hash/fnv"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"sync"
	"time"
)

// FakeThumbnailer draws a coloured tile derived from the start of the file, so tests and the demo server need no real tools.
// A file starting with "BAD" fails like a corrupt image would.
type FakeThumbnailer struct {
	Delay time.Duration
	Err   error

	mu     sync.Mutex
	calls  int
	active int
	peak   int
}

func NewFakeThumbnailer() *FakeThumbnailer {
	return &FakeThumbnailer{}
}

func (f *FakeThumbnailer) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

func (f *FakeThumbnailer) Peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.peak
}

func (f *FakeThumbnailer) Render(ctx context.Context, src File, kind string, side int) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	f.active++
	f.peak = max(f.peak, f.active)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()

	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.Err != nil {
		return nil, f.Err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	head, _ := io.ReadAll(io.LimitReader(src, 64))
	if bytes.HasPrefix(head, []byte("BAD")) {
		return nil, ErrBadMedia
	}

	return drawTile(head, kind, side)
}

func drawTile(seed []byte, kind string, side int) ([]byte, error) {
	hash := fnv.New32a()
	_, _ = hash.Write(seed)
	sum := hash.Sum32()
	base := color.RGBA{R: uint8(sum), G: uint8(sum >> 8), B: uint8(sum >> 16), A: 255}

	width, height := min(side, 400), min(side, 400)*3/4
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		shade := uint8(y * 90 / height)
		for x := range width {
			img.SetRGBA(x, y, color.RGBA{R: satAdd(base.R, shade), G: satAdd(base.G, shade), B: satAdd(base.B, shade), A: 255})
		}
	}
	if kind == PhotoVideo {
		drawPlay(img)
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 70}); err != nil {
		return nil, err
	}

	return out.Bytes(), nil
}

func satAdd(a, b uint8) uint8 {
	if sum := int(a) + int(b); sum < 255 {
		return uint8(sum)
	}

	return 255
}

func drawPlay(img *image.RGBA) {
	bounds := img.Bounds()
	cx, cy, size := bounds.Dx()/2, bounds.Dy()/2, bounds.Dy()/6
	for y := -size; y <= size; y++ {
		span := (size - abs(y)) * 2 / 3
		for x := -size / 2; x <= -size/2+span; x++ {
			img.SetRGBA(cx+x, cy+y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}

	return v
}
