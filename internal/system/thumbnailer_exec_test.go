//go:build linux

package system

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stubTool(t *testing.T, script string) (tool, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	tool = filepath.Join(dir, "tool")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\n" + script
	if err := os.WriteFile(tool, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	return tool, argsFile
}

const copyToOutput = `
for last; do :; done
out=${last%%\[*}
cat <&3 > "$out"
`

func openTemp(t *testing.T, name string, content []byte) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })

	return file
}

func TestExecThumbnailerHandsTheOpenFileToTheTool(t *testing.T) {
	tool, argsFile := stubTool(t, copyToOutput)
	src := openTemp(t, "-rf --version.jpg", []byte("picture bytes"))
	e := &ExecThumbnailer{Vips: tool}

	data, err := e.Render(context.Background(), src, PhotoImage, ThumbSide)
	if err != nil || string(data) != "picture bytes" {
		t.Fatalf("got %q, %v", data, err)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), procSource+"\n") || !strings.Contains(string(args), "400x400>") {
		t.Fatalf("args %q", args)
	}
	if strings.Contains(string(args), "-rf") || strings.Contains(string(args), "version") {
		t.Fatalf("the file name must never reach the tool: %q", args)
	}
}

func TestExecThumbnailerFailuresAreBadMedia(t *testing.T) {
	cases := map[string]string{
		"exit code":    "echo 'unable to load' >&2\nexit 1\n",
		"no output":    "exit 0\n",
		"empty output": copyToOutput + ": > \"$out\"\n",
		"killed":       "kill -9 $$\n",
		"huge stderr":  "head -c 100000 /dev/zero | tr '\\0' x >&2\nexit 3\n",
		"output a dir": "for last; do :; done\nmkdir -p \"${last%%\\[*}\"\n",
		"stdout spam":  "head -c 1000000 /dev/zero\nexit 1\n",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			tool, _ := stubTool(t, script)
			e := &ExecThumbnailer{Vips: tool}
			_, err := e.Render(context.Background(), openTemp(t, "a.jpg", []byte("x")), PhotoImage, ThumbSide)
			if !errors.Is(err, ErrBadMedia) {
				t.Fatalf("got %v", err)
			}
			if len(err.Error()) > 4000 {
				t.Fatalf("the error must stay short: %d bytes", len(err.Error()))
			}
		})
	}
}

func TestExecThumbnailerTimeoutKillsTheTool(t *testing.T) {
	tool, _ := stubTool(t, "sleep 60\n")
	e := &ExecThumbnailer{Vips: tool, Timeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := e.Render(context.Background(), openTemp(t, "a.jpg", []byte("x")), PhotoImage, ThumbSide)
	if !errors.Is(err, ErrBadMedia) || !strings.Contains(err.Error(), "took longer") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

func TestExecThumbnailerStopsWhenTheRequestIsCancelled(t *testing.T) {
	tool, _ := stubTool(t, "sleep 60\n")
	e := &ExecThumbnailer{Vips: tool}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := e.Render(ctx, openTemp(t, "a.jpg", []byte("x")), PhotoImage, ThumbSide)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 10*time.Second {
		t.Fatalf("got %v after %s", err, time.Since(start))
	}
}

func TestExecThumbnailerReportsAMissingTool(t *testing.T) {
	src := openTemp(t, "a.jpg", []byte("x"))
	e := &ExecThumbnailer{Vips: "/nonexistent/vipsthumbnail"}
	if _, err := e.Render(context.Background(), src, PhotoImage, ThumbSide); !errors.Is(err, ErrToolMissing) {
		t.Fatalf("configured path missing: %v", err)
	}

	t.Setenv("PATH", t.TempDir())
	if _, err := NewExecThumbnailer().Render(context.Background(), src, PhotoImage, ThumbSide); !errors.Is(err, ErrToolMissing) || !strings.Contains(err.Error(), "vipsthumbnail") {
		t.Fatalf("not on PATH: %v", err)
	}
	if _, err := NewExecThumbnailer().Render(context.Background(), src, PhotoVideo, ThumbSide); !errors.Is(err, ErrToolMissing) || !strings.Contains(err.Error(), "ffmpeg") {
		t.Fatalf("ffmpeg not on PATH: %v", err)
	}
}

func TestExecThumbnailerRefusesHugeImagesWithoutRunningAnything(t *testing.T) {
	tool, argsFile := stubTool(t, copyToOutput)
	src := openTemp(t, "huge.png", []byte("x"))
	path := src.Name()
	if err := os.Truncate(path, maxImageBytes+1); err != nil {
		t.Skipf("sparse files are not available: %v", err)
	}
	reopened, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	e := &ExecThumbnailer{Vips: tool}
	if _, err := e.Render(context.Background(), reopened, PhotoImage, ThumbSide); !errors.Is(err, ErrBadMedia) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("the tool must not run for an oversized image")
	}
}

func TestExecThumbnailerRetriesTheVideoPosterFromTheStart(t *testing.T) {
	tool, argsFile := stubTool(t, `
case "$*" in *"-ss 1 "*) exit 1;; esac
`+copyToOutput)
	e := &ExecThumbnailer{FFmpeg: tool}
	data, err := e.Render(context.Background(), openTemp(t, "short.mp4", []byte("frame")), PhotoVideo, ThumbSide)
	if err != nil || string(data) != "frame" {
		t.Fatalf("got %q, %v", data, err)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(strings.ReplaceAll(string(args), "\n", " "), "-ss 0 ") {
		t.Fatalf("second attempt must seek to 0: %q", args)
	}
}

func TestExecThumbnailerNeedsARealFile(t *testing.T) {
	files := NewFakeFiles()
	put(t, files, "a.jpg", "x")
	file, _, _ := files.OpenPhoto("a.jpg")
	if _, err := NewExecThumbnailer().Render(context.Background(), file, PhotoImage, ThumbSide); err == nil {
		t.Fatal("an in-memory file can not be handed to a process")
	}
}

func TestExecThumbnailerWrapsTheToolWithNiceAndLimitsWhenAvailable(t *testing.T) {
	nice, prlimit := "", ""
	if path, err := exec.LookPath("nice"); err == nil {
		nice = path
	}
	if path, err := exec.LookPath("prlimit"); err == nil {
		prlimit = path
	}
	joined := strings.Join((&ExecThumbnailer{Nice: nice, Prlimit: prlimit}).wrapper(1<<30), " ")
	if nice != "" && !strings.Contains(joined, "-n 19") {
		t.Fatalf("nice missing: %q", joined)
	}
	if prlimit != "" && (!strings.Contains(joined, "--as=1073741824") || !strings.Contains(joined, "--cpu=") || !strings.HasSuffix(joined, "--")) {
		t.Fatalf("prlimit missing: %q", joined)
	}
}

func TestExecThumbnailerThroughTheFilesInterface(t *testing.T) {
	tool, _ := stubTool(t, copyToOutput)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "-rf.jpg"), []byte("dash name"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(interface{ Close() error }).Close()

	file, photo, err := files.OpenPhoto("-rf.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := (&ExecThumbnailer{Vips: tool}).Render(context.Background(), file, photo.Kind, ThumbSide)
	if err != nil || string(data) != "dash name" {
		t.Fatalf("got %q, %v", data, err)
	}
}

func lookTools(t *testing.T) {
	t.Helper()
	for _, name := range []string{"vipsthumbnail", "ffmpeg"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is not installed", name)
		}
	}
}

func dimensions(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}

	return cfg.Width, cfg.Height
}

func TestRealToolsRenderEveryFormat(t *testing.T) {
	lookTools(t)
	e := NewExecThumbnailer()
	cases := []struct {
		file, kind string
		side       int
		width      int
		height     int
	}{
		{"sample.jpg", PhotoImage, ThumbSide, 400, 300},
		{"sample.png", PhotoImage, ThumbSide, 400, 300},
		{"sample.webp", PhotoImage, ThumbSide, 400, 300},
		{"sample.heic", PhotoImage, ThumbSide, 400, 300},
		{"sample.heic", PhotoImage, PreviewSide, 1200, 900},
		{"rotated.jpg", PhotoImage, ThumbSide, 300, 400},
		{"sample.mp4", PhotoVideo, ThumbSide, 320, 240},
		{"short.mp4", PhotoVideo, ThumbSide, 160, 120},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			src, err := os.Open("testdata/" + c.file)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			start := time.Now()
			data, err := e.Render(context.Background(), src, c.kind, c.side)
			if err != nil {
				t.Fatal(err)
			}
			width, height := dimensions(t, data)
			t.Logf("%s side=%d: %d bytes, %dx%d, %s", c.file, c.side, len(data), width, height, time.Since(start).Round(time.Millisecond))
			if width != c.width || height != c.height {
				t.Fatalf("got %dx%d, want %dx%d", width, height, c.width, c.height)
			}
		})
	}
}

func TestRealToolsFailCleanlyOnBadInput(t *testing.T) {
	lookTools(t)
	e := NewExecThumbnailer()
	heic, _ := os.ReadFile("testdata/sample.heic")
	jpg, _ := os.ReadFile("testdata/sample.jpg")
	mp4, _ := os.ReadFile("testdata/sample.mp4")
	cases := map[string]struct {
		name, kind string
		content    []byte
	}{
		"zero byte jpg":  {"a.jpg", PhotoImage, nil},
		"text as jpg":    {"a.jpg", PhotoImage, []byte("this is not a picture")},
		"truncated heic": {"a.heic", PhotoImage, heic[:len(heic)/3]},
		"text as heic":   {"a.heic", PhotoImage, []byte("nope")},
		"text as mp4":    {"a.mp4", PhotoVideo, []byte("nope")},
		"zero byte mp4":  {"a.mp4", PhotoVideo, nil},
		"truncated mp4":  {"a.mp4", PhotoVideo, mp4[:200]},
		"jpg as png":     {"a.png", PhotoImage, jpg[:100]},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			_, err := e.Render(context.Background(), openTemp(t, c.name, c.content), c.kind, ThumbSide)
			t.Logf("%v in %s", err, time.Since(start).Round(time.Millisecond))
			if !errors.Is(err, ErrBadMedia) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func writePNGBomb(t *testing.T, path string, side uint32) {
	t.Helper()
	var idat bytes.Buffer
	compressor := zlib.NewWriter(&idat)
	row := make([]byte, 1+(side+7)/8)
	for range side {
		if _, err := compressor.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	chunk := func(kind string, data []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
		out = append(out, kind...)
		out = append(out, data...)

		return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(kind), data...)))
	}
	header := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, side), side)
	header = append(header, 1, 0, 0, 0, 0)
	png := append([]byte("\x89PNG\r\n\x1a\n"), chunk("IHDR", header)...)
	png = append(png, chunk("IDAT", idat.Bytes())...)
	png = append(png, chunk("IEND", nil)...)
	if err := os.WriteFile(path, png, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRealToolsSurviveADecompressionBomb(t *testing.T) {
	lookTools(t)
	path := filepath.Join(t.TempDir(), "bomb.png")
	writePNGBomb(t, path, 60000)
	src, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	info, _ := src.Stat()

	e := &ExecThumbnailer{Timeout: 5 * time.Second}
	start := time.Now()
	data, err := e.Render(context.Background(), src, PhotoImage, ThumbSide)
	t.Logf("bomb of %d bytes (%d x %d pixels): %d output bytes, err %v, %s", info.Size(), 60000, 60000, len(data), err, time.Since(start).Round(time.Millisecond))
	if err != nil && !errors.Is(err, ErrBadMedia) {
		t.Fatalf("a bomb must either render or fail cleanly, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the bomb outlived the timeout: %s", time.Since(start))
	}
}
