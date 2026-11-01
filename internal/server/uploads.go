package server

import (
	"archive/zip"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

const (
	maxChunkBytes  = 64 << 20
	maxExistsPaths = 5000
	maxExistsBody  = 4 << 20
	zipCopyBuffer  = 256 << 10
)

var contentRangePattern = regexp.MustCompile(`^bytes (\d{1,18})-(\d{1,18})/(\d{1,18})$`)

type createUploadBody struct {
	Path      string `json:"path"`
	Size      *int64 `json:"size"`
	Overwrite bool   `json:"overwrite"`
}

type existsBody struct {
	Paths []string `json:"paths"`
}

type existingPath struct {
	Path     string     `json:"path"`
	IsDir    bool       `json:"isDir"`
	Size     *int64     `json:"size,omitempty"`
	Modified *time.Time `json:"modified,omitempty"`
}

func existingEntry(path string, info system.FileInfo) existingPath {
	entry := existingPath{Path: path, IsDir: info.IsDir}
	if !info.IsDir {
		entry.Size = &info.Size
		entry.Modified = &info.Modified
	}

	return entry
}

func uploadJSON(info system.UploadInfo) map[string]any {
	return map[string]any{"id": info.ID, "path": info.Path, "size": info.Size, "offset": info.Offset}
}

func (a fileHandlers) createUpload(w http.ResponseWriter, r *http.Request) {
	var body createUploadBody
	if !decodeJSON(w, r, &body) {
		return
	}
	switch {
	case body.Size == nil || *body.Size < 0:
		writeCoded(w, http.StatusBadRequest, codeUploadInvalid, "size must be a number of bytes")
		return
	case *body.Size > a.maxUpload:
		writeCoded(w, http.StatusRequestEntityTooLarge, codeUploadTooLarge, "the file is larger than the upload limit")
		return
	}
	if a.refuseWhenFull(w) {
		return
	}

	info, err := a.files.CreateUpload(body.Path, *body.Size, body.Overwrite)
	if err != nil {
		fileError(w, "create upload", body.Path, err)
		return
	}
	writeJSON(w, http.StatusCreated, uploadJSON(info))
}

func (a fileHandlers) uploadStatus(w http.ResponseWriter, r *http.Request) {
	info, err := a.files.UploadStatus(r.PathValue("id"))
	if err != nil {
		fileError(w, "upload status", r.PathValue("id"), err)
		return
	}
	writeJSON(w, http.StatusOK, uploadJSON(info))
}

func (a fileHandlers) appendChunk(w http.ResponseWriter, r *http.Request) {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/octet-stream" {
		writeError(w, http.StatusUnsupportedMediaType, "expected application/octet-stream")
		return
	}
	start, end, total, ok := parseContentRange(r.Header.Get("Content-Range"))
	if !ok {
		writeCoded(w, http.StatusBadRequest, codeUploadInvalid, "invalid Content-Range")
		return
	}
	id := r.PathValue("id")
	info, err := a.files.UploadStatus(id)
	if err != nil {
		fileError(w, "upload chunk", id, err)
		return
	}

	length := end - start + 1
	switch {
	case info.Size > a.maxUpload:
		writeCoded(w, http.StatusRequestEntityTooLarge, codeUploadTooLarge, "the file is larger than the upload limit")
	case total != info.Size || end >= total:
		writeOffsetError(w, http.StatusBadRequest, codeUploadInvalid, "the range does not fit the size declared for this upload", info.Offset)
	case start != info.Offset:
		writeOffsetError(w, http.StatusConflict, codeUploadOutOfSync, "the chunk does not start where the server left off", info.Offset)
	case length > maxChunkBytes:
		writeCoded(w, http.StatusRequestEntityTooLarge, codeUploadInvalid, "the chunk is too large")
	case r.ContentLength >= 0 && r.ContentLength != length:
		writeOffsetError(w, http.StatusBadRequest, codeUploadInvalid, "the chunk is not as long as its range", info.Offset)
	default:
		a.storeChunk(w, r, id, start, length)
	}
}

func (a fileHandlers) storeChunk(w http.ResponseWriter, r *http.Request, id string, start, length int64) {
	rc := http.NewResponseController(w)
	body := &trackedReader{r: &deadlineReader{r: r.Body, rc: rc}}
	info, err := a.files.AppendUpload(id, start, length, body)
	_ = rc.SetWriteDeadline(time.Now().Add(transferIdle))

	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"offset": info.Offset})
	case errors.Is(err, system.ErrUploadOffset):
		writeOffsetError(w, http.StatusConflict, codeUploadOutOfSync, "the chunk does not start where the server left off", info.Offset)
	case errors.Is(err, system.ErrUploadTooLarge):
		writeOffsetError(w, http.StatusBadRequest, codeUploadInvalid, "the chunk is longer than its range", info.Offset)
	case errors.Is(err, system.ErrUploadShort):
		if body.err != nil {
			log.Printf("upload chunk %s interrupted: %v", id, body.err)
		}
		writeOffsetError(w, http.StatusBadRequest, codeUploadInvalid, "the chunk ended before its range did", info.Offset)
	default:
		fileError(w, "upload chunk", id, err)
	}
}

func (a fileHandlers) finishUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := a.files.FinishUpload(id)
	if errors.Is(err, system.ErrUploadIncomplete) {
		info, _ := a.files.UploadStatus(id)
		writeOffsetError(w, http.StatusConflict, codeUploadOutOfSync, "the upload is not complete", info.Offset)
		return
	}
	if err != nil {
		fileError(w, "finish upload", id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a fileHandlers) checkExists(w http.ResponseWriter, r *http.Request) {
	var body existsBody
	if !decodeJSONMax(w, r, &body, maxExistsBody) {
		return
	}
	if len(body.Paths) > maxExistsPaths {
		writeError(w, http.StatusRequestEntityTooLarge, "too many paths in one check")
		return
	}

	existing := []existingPath{}
	seen := make(map[string]bool, len(body.Paths))
	for _, path := range body.Paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		info, err := a.files.Stat(path)
		switch {
		case err == nil:
			existing = append(existing, existingEntry(path, info))
		case errors.Is(err, system.ErrNotFound), errors.Is(err, system.ErrNotDir):
		default:
			fileError(w, "exists check", path, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"existing": existing})
}

func (a fileHandlers) downloadZip(w http.ResponseWriter, r *http.Request) {
	name, ok := queryValue(w, r, "path")
	if !ok {
		return
	}
	name, err := system.CleanPath(name)
	if err != nil {
		fileError(w, "zip", name, err)
		return
	}
	isFile, err := a.isFile(name)
	if err != nil {
		fileError(w, "zip", name, err)
		return
	}

	archiveName := baseName(name)
	if archiveName == "" {
		archiveName = "files"
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": archiveName + ".zip"})
	if disposition == "" {
		disposition = "attachment"
	}
	header := w.Header()
	header.Set("Content-Type", "application/zip")
	header.Set("Content-Disposition", disposition)
	header.Set("X-Content-Type-Options", "nosniff")

	out := &deadlineWriter{ResponseWriter: w, rc: http.NewResponseController(w)}
	z := &zipper{files: a.files, zw: zip.NewWriter(out), buf: make([]byte, zipCopyBuffer), request: r}
	if isFile {
		err = z.addFile(name, archiveName)
	} else {
		err = z.addTree(name, archiveName, a.folderModified(name))
	}
	if err == nil {
		err = z.zw.Close()
	}
	if err != nil {
		log.Printf("zip: %v", logCause(err))
		panic(http.ErrAbortHandler)
	}
}

func (a fileHandlers) isFile(name string) (bool, error) {
	_, err := a.files.List(name)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, system.ErrNotDir):
		return true, nil
	}

	return false, err
}

func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}

	return path
}

type zipper struct {
	files   system.Files
	zw      *zip.Writer
	buf     []byte
	request *http.Request
}

func (a fileHandlers) folderModified(name string) time.Time {
	if info, err := a.files.Stat(name); name != "" && err == nil {
		return info.Modified
	}

	return time.Now()
}

func (z *zipper) addTree(dir, prefix string, modified time.Time) error {
	if prefix != "" {
		header := &zip.FileHeader{Name: prefix + "/", Method: zip.Store, Modified: modified}
		header.SetMode(0o755 | fs.ModeDir)
		if _, err := z.zw.CreateHeader(header); err != nil {
			return err
		}
	}
	entries, err := z.files.List(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := z.request.Context().Err(); err != nil {
			return err
		}
		if entry.Symlink {
			continue
		}
		if _, err := system.CleanPath(entry.Name); err != nil {
			log.Printf("zip: skipping an entry whose name the file browser cannot address")
			continue
		}
		path, entryName := joinPath(dir, entry.Name), joinPath(prefix, entry.Name)
		if entry.IsDir {
			err = z.addTree(path, entryName, entry.Modified)
		} else {
			err = z.addFile(path, entryName)
		}
		if err != nil {
			return err
		}
	}

	return nil
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}

	return parent + "/" + name
}

func (z *zipper) addFile(path, entryName string) error {
	file, info, err := z.files.Open(path)
	if errors.Is(err, system.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()

	header := &zip.FileHeader{Name: entryName, Method: zip.Store, Modified: info.Modified}
	header.SetMode(0o644)
	entry, err := z.zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.CopyBuffer(entry, file, z.buf)

	return err
}

func parseContentRange(value string) (start, end, total int64, ok bool) {
	match := contentRangePattern.FindStringSubmatch(value)
	if match == nil {
		return 0, 0, 0, false
	}
	numbers := make([]int64, 3)
	for i := range numbers {
		n, err := strconv.ParseInt(match[i+1], 10, 64)
		if err != nil {
			return 0, 0, 0, false
		}
		numbers[i] = n
	}
	if numbers[0] > numbers[1] {
		return 0, 0, 0, false
	}

	return numbers[0], numbers[1], numbers[2], true
}

func writeOffsetError(w http.ResponseWriter, status int, code, message string, offset int64) {
	writeJSON(w, status, map[string]any{"error": message, "code": code, "offset": offset})
}
