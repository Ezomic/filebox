package system

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ThumbsDir holds generated thumbnails and previews at the top of a Files root. Like UploadsDir it is invisible to every file operation.
const ThumbsDir = ".thumbs"

const (
	PhotoImage = "image"
	PhotoVideo = "video"

	MaxPhotoPage    = 500
	maxPhotoEntries = 100000
	maxPhotoDepth   = 32
	thumbIndexName  = "index.json"
	thumbTempPrefix = ".tmp-"
	maxThumbIndex   = 16 << 20
	staleTempAfter  = time.Hour
)

var ErrNotMedia = errors.New("not a photo or video")

var mediaKinds = map[string]string{
	"jpg": PhotoImage, "jpeg": PhotoImage, "png": PhotoImage, "gif": PhotoImage, "webp": PhotoImage,
	"avif": PhotoImage, "heic": PhotoImage, "heif": PhotoImage,
	"mp4": PhotoVideo, "mov": PhotoVideo, "m4v": PhotoVideo, "webm": PhotoVideo,
}

var thumbNamePattern = regexp.MustCompile(`^([0-9a-f]{32})\.[tp]\.jpg$`)

// Photo describes one image or video. Key names its cached thumbnails and changes whenever the path, size or modification time does.
type Photo struct {
	Path     string
	Kind     string
	Size     int64
	Date     time.Time
	Modified time.Time
	// Custom is set when Date is a day the user chose instead of the original date.
	Custom bool
	Key    string
	// Scope tells two different Files roots apart, so the same Key in two roots is never mistaken for one job.
	Scope string
}

type PhotoPage struct {
	Photos []Photo
	// Next is the cursor for the following page, empty on the last page.
	Next string
}

func (p Photo) ThumbName() string { return p.Key + ".t.jpg" }

func (p Photo) PreviewName() string { return p.Key + ".p.jpg" }

// HasPreview reports whether browsers can not show the original, so a converted JPEG is served instead.
func (p Photo) HasPreview() bool {
	ext := mediaExt(p.Path)

	return ext == "heic" || ext == "heif"
}

func mediaExt(name string) string {
	dot := strings.LastIndexByte(baseOf(name), '.')
	if dot <= 0 {
		return ""
	}

	return strings.ToLower(baseOf(name)[dot+1:])
}

func mediaKind(name string) (kind, ext string, ok bool) {
	ext = mediaExt(name)
	kind, ok = mediaKinds[ext]

	return kind, ext, ok
}

func photoKey(path string, size int64, modified time.Time) string {
	sum := sha256.Sum256([]byte(path + "\x00" + strconv.FormatInt(size, 10) + "\x00" + strconv.FormatInt(modified.UnixNano(), 10)))

	return hex.EncodeToString(sum[:16])
}

func hiddenName(name string) bool {
	return strings.EqualFold(name, UploadsDir) || strings.EqualFold(name, TrashDir) || strings.EqualFold(name, ThumbsDir)
}

// Newest first, then by path so the order is total and a cursor always lands on exactly one position.
func sortPhotos(photos []Photo) {
	slices.SortFunc(photos, func(a, b Photo) int {
		if c := b.Date.Compare(a.Date); c != 0 {
			return c
		}

		return strings.Compare(a.Path, b.Path)
	})
}

func photoCursor(p Photo) string {
	return strconv.FormatInt(p.Date.Unix(), 10) + "|" + p.Path
}

func parsePhotoCursor(cursor string) (unix int64, path string, err error) {
	seconds, path, ok := strings.Cut(cursor, "|")
	if !ok {
		return 0, "", ErrInvalidPath
	}
	unix, err = strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return 0, "", ErrInvalidPath
	}

	return unix, path, nil
}

// pagePhotos sorts photos and returns the limit entries that follow the cursor.
func pagePhotos(photos []Photo, after string, limit int) (PhotoPage, error) {
	limit = min(max(limit, 1), MaxPhotoPage)
	sortPhotos(photos)

	start := 0
	if after != "" {
		unix, path, err := parsePhotoCursor(after)
		if err != nil {
			return PhotoPage{}, err
		}
		start = len(photos)
		if found := slices.IndexFunc(photos, func(p Photo) bool {
			return p.Date.Unix() < unix || (p.Date.Unix() == unix && p.Path > path)
		}); found >= 0 {
			start = found
		}
	}

	end := min(start+limit, len(photos))
	page := PhotoPage{Photos: photos[start:end:end]}
	if end < len(photos) && end > start {
		page.Next = photoCursor(photos[end-1])
	}

	return page, nil
}

func thumbKeyOf(name string) (string, bool) {
	match := thumbNamePattern.FindStringSubmatch(name)
	if match == nil {
		return "", false
	}

	return match[1], true
}
