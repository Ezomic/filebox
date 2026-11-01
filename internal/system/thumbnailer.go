package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	ThumbSide   = 400
	PreviewSide = 2000

	// procSource is how a tool reads the already opened source: the file is passed as descriptor 3, so the tool never
	// resolves a user controlled path and a symlink swapped in meanwhile can not redirect it.
	procSource = "/proc/self/fd/3"

	thumbTimeout    = 30 * time.Second
	maxImageBytes   = 256 << 20
	maxThumbOutput  = 32 << 20
	imageAddressMax = 3 << 30
	videoAddressMax = 4 << 30
	cpuSecondsMax   = 60
	maxToolStderr   = 2048
)

var (
	// ErrToolMissing means vipsthumbnail or ffmpeg is not installed.
	ErrToolMissing = errors.New("thumbnail tool is not installed")
	// ErrBadMedia means the file could not be turned into a picture: corrupt, not really an image, too big or too slow.
	ErrBadMedia = errors.New("could not render the file")
)

// Thumbnailer turns an image or a video into a JPEG no larger than side pixels on its longest edge.
type Thumbnailer interface {
	Render(ctx context.Context, src File, kind string, side int) ([]byte, error)
}

// ExecThumbnailer runs vipsthumbnail for images and ffmpeg for video posters. Arguments are fixed lists, never a shell,
// and no user text is ever part of them.
type ExecThumbnailer struct {
	// Tool paths are looked up on every call when empty, so installing a package needs no restart.
	Vips, FFmpeg, Nice, Prlimit string
	Timeout                     time.Duration
}

func NewExecThumbnailer() *ExecThumbnailer {
	return &ExecThumbnailer{}
}

func (e *ExecThumbnailer) find(configured, name string) string {
	if configured != "" {
		if _, err := os.Stat(configured); err != nil {
			return ""
		}

		return configured
	}
	found, err := exec.LookPath(name)
	if err != nil {
		return ""
	}

	return found
}

func (e *ExecThumbnailer) Render(ctx context.Context, src File, kind string, side int) ([]byte, error) {
	file, ok := src.(*os.File)
	if !ok {
		return nil, errors.New("the thumbnailer needs a file on disk")
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if kind == PhotoImage && info.Size() > maxImageBytes {
		return nil, fmt.Errorf("%w: image is larger than %d MiB", ErrBadMedia, maxImageBytes>>20)
	}

	tool, name, attempts, addressMax := e.Vips, "vipsthumbnail", func(out string) [][]string { return [][]string{vipsArgs(side, out)} }, int64(imageAddressMax)
	if kind == PhotoVideo {
		tool, name, attempts, addressMax = e.FFmpeg, "ffmpeg", func(out string) [][]string { return ffmpegArgs(side, out) }, int64(videoAddressMax)
	}
	tool = e.find(tool, name)
	if tool == "" {
		return nil, fmt.Errorf("%w: %s", ErrToolMissing, name)
	}

	dir, err := os.MkdirTemp("", "filebox-thumb-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "out.jpg")

	timeout := e.Timeout
	if timeout <= 0 {
		timeout = thumbTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var failure error
	for _, args := range attempts(out) {
		if failure = e.run(runCtx, file, tool, addressMax, dir, args); failure == nil {
			break
		}
		if runCtx.Err() != nil {
			break
		}
	}
	if failure != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if runCtx.Err() != nil {
			return nil, fmt.Errorf("%w: %s took longer than %s", ErrBadMedia, name, timeout)
		}

		return nil, failure
	}

	return readRendered(out)
}

func vipsArgs(side int, out string) []string {
	return []string{procSource, "--size", fmt.Sprintf("%dx%d>", side, side), "--rotate", "--eprofile", "srgb", "-o", out + "[Q=82,strip]"}
}

// The poster is taken a second in, because the first frame is often black. A clip shorter than that is retried from the start.
func ffmpegArgs(side int, out string) [][]string {
	var attempts [][]string
	for _, seek := range []string{"1", "0"} {
		attempts = append(attempts, []string{
			"-nostdin", "-hide_banner", "-loglevel", "error", "-threads", "1",
			"-ss", seek, "-i", procSource, "-an", "-sn", "-frames:v", "1",
			"-vf", fmt.Sprintf("scale=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease", side, side),
			"-q:v", "4", "-y", out,
		})
	}

	return attempts
}

func (e *ExecThumbnailer) wrapper(addressMax int64) []string {
	var wrap []string
	if nice := e.find(e.Nice, "nice"); nice != "" {
		wrap = append(wrap, nice, "-n", "19")
	}
	if prlimit := e.find(e.Prlimit, "prlimit"); prlimit != "" {
		wrap = append(wrap, prlimit, "--as="+strconv.FormatInt(addressMax, 10), "--cpu="+strconv.Itoa(cpuSecondsMax), "--fsize="+strconv.Itoa(maxThumbOutput), "--")
	}

	return wrap
}

func (e *ExecThumbnailer) run(ctx context.Context, file *os.File, tool string, addressMax int64, dir string, args []string) error {
	argv := append(e.wrapper(addressMax), tool)
	argv = append(argv, args...)

	stderr := &limitedBuffer{max: maxToolStderr}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.ExtraFiles = []*os.File{file}
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir, "LC_ALL=C", "VIPS_CONCURRENCY=1", "OMP_NUM_THREADS=1"}
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	var exited *exec.ExitError
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case !errors.As(err, &exited):
		return fmt.Errorf("%w: %s: %v", ErrToolMissing, filepath.Base(tool), err)
	}

	return fmt.Errorf("%w: %s: %v: %s", ErrBadMedia, filepath.Base(tool), err, strings.TrimSpace(stderr.String()))
}

func readRendered(out string) ([]byte, error) {
	file, err := os.Open(out)
	if err != nil {
		return nil, fmt.Errorf("%w: the tool wrote no picture", ErrBadMedia)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxThumbOutput+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: the output is unreadable", ErrBadMedia)
	case len(data) == 0 || len(data) > maxThumbOutput:
		return nil, fmt.Errorf("%w: the picture has a bad size", ErrBadMedia)
	}

	return data, nil
}

type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}

	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }
