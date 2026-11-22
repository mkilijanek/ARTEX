package server

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Workspace file manager — browse / view / edit / download / upload / delete the
// shared work dir (s.m.dir), where all agents write their artifacts. Every path is
// confined to the work dir root (traversal via ".." is neutralised) and every operation
// goes through os.Root without following links: a link planted by a tool answers 400 and
// is left in place. Tools can change the tree at any moment, so a path is opened one
// component at a time, each compared with what was looked at, and a file is opened in a way
// that cannot block (a FIFO or a device is refused). All routes sit behind requireAuth (see
// Handler()).

const (
	maxWorkspaceRead   = 2 << 20   // 2 MiB: files bigger than this aren't inlined for view/edit (download instead)
	maxWorkspaceUpload = 512 << 20 // 512 MiB per upload request
)

// errWsLink is returned when a path goes through a symbolic link. The workspace is a tree
// that tool processes write into, so a link planted there must never steer the server (which
// may run with more rights than the tools) to a file outside it.
var errWsLink = errors.New("path goes through a link")

// wsClean maps a user-supplied relative path to a cleaned path relative to the work dir
// ("." for the root itself). It returns ok=false if the path would escape the root.
// filepath.Clean on a rooted copy collapses any ".." so nothing can climb above the root.
// The lexical check is only the first line: every operation then runs through os.Root.
func wsClean(rel string) (string, bool) {
	rel = strings.TrimPrefix(strings.TrimSpace(rel), "/")
	clean := filepath.Clean("/" + rel) // e.g. "/a/../../etc" → "/etc" (still rooted at "/")
	clean = strings.TrimPrefix(clean, string(os.PathSeparator))
	if clean == "" {
		return ".", true
	}
	if strings.ContainsRune(clean, 0) {
		return "", false
	}
	return clean, true
}

// wsRoot opens the work dir as an os.Root: nothing opened through it can leave the directory.
func (s *Server) wsRoot() (*os.Root, error) {
	return os.OpenRoot(s.m.dir)
}

// wsWalkHook, when set (tests only), runs after a component of a path was looked at and before
// it is opened, so a test can replace the component there.
var wsWalkHook func(rel string)

var (
	// errWsChanged: a component was replaced between the look and the open.
	errWsChanged = errors.New("path changed while it was being opened")
	// errWsNotDir: a component that must be a directory is something else.
	errWsNotDir = errors.New("not a directory")
	// errWsHardLink: the file has more than one name; the other may be a file of the server's.
	errWsHardLink = errors.New("file has more than one name")
)

// wsOpenDir opens the directory rel ("." for the work dir itself) one component at a time: each is
// looked at with Lstat (a link, or anything but a directory, is refused), opened relative to its
// parent and compared with what Lstat saw. os.Root alone follows a link that stays inside the
// root, so this is what keeps the API off every link. With create, missing components are made.
// The caller closes the result.
func wsOpenDir(root *os.Root, rel string, create bool) (*os.Root, error) {
	cur, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return cur, nil
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	for i, name := range parts {
		next, err := wsStepDir(cur, name, strings.Join(parts[:i+1], string(os.PathSeparator)), create)
		cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

func wsStepDir(parent *os.Root, name, rel string, create bool) (*os.Root, error) {
	if create {
		if err := parent.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	lfi, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if lfi.Mode()&fs.ModeSymlink != 0 {
		return nil, errWsLink
	}
	if !lfi.IsDir() {
		return nil, errWsNotDir
	}
	if wsWalkHook != nil {
		wsWalkHook(rel)
	}
	// name+"/." makes os.Root open name as an intermediate component, with O_DIRECTORY: a FIFO put
	// there since the Lstat fails instead of blocking the open.
	next, err := parent.OpenRoot(name + string(os.PathSeparator) + ".")
	if err != nil {
		return nil, err
	}
	nfi, err := next.Stat(".")
	if err != nil || !os.SameFile(lfi, nfi) {
		next.Close()
		return nil, errWsChanged
	}
	return next, nil
}

// wsSplit splits a cleaned path into its directory and its last component.
func wsSplit(rel string) (dir, name string) {
	return filepath.Dir(rel), filepath.Base(rel)
}

// wsOpenEntry opens the existing regular file name in dir with flag: Lstat, then an open that
// does not follow a link and cannot block (O_NONBLOCK, cleared again on the open descriptor),
// then a comparison of the open file with what Lstat saw. A link, a FIFO, a device, a socket, a
// directory and a file with more than one name are refused. The file is not truncated here.
func wsOpenEntry(dir *os.Root, name, rel string, flag int) (*os.File, fs.FileInfo, error) {
	lfi, err := dir.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case lfi.Mode()&fs.ModeSymlink != 0:
		return nil, nil, errWsLink
	case lfi.IsDir():
		return nil, nil, errWsIsDir
	case !lfi.Mode().IsRegular():
		return nil, nil, errWsNotFile
	}
	if wsWalkHook != nil {
		wsWalkHook(rel)
	}
	f, err := dir.OpenFile(name, flag|noFollowFlag|nonBlockFlag, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(lfi, fi) {
		f.Close()
		return nil, nil, errWsChanged
	}
	if n, ok := linkCount(fi); ok && n > 1 {
		f.Close()
		return nil, nil, errWsHardLink
	}
	if err := clearNonblock(f); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}

// wsReplyErr answers for an error from the helpers above or from an os.Root call.
func wsReplyErr(w http.ResponseWriter, err error, notFound string) {
	switch {
	case errors.Is(err, errWsLink), errors.Is(err, errWsChanged), errors.Is(err, errWsNotDir), errors.Is(err, errWsHardLink):
		writeErr(w, 400, "invalid path")
	case errors.Is(err, fs.ErrNotExist):
		writeErr(w, 404, notFound)
	default:
		writeErr(w, 500, err.Error())
	}
}

// wsOpenFile opens an existing regular file for reading (see wsOpenEntry).
func wsOpenFile(root *os.Root, rel string) (*os.File, fs.FileInfo, error) {
	d, name := wsSplit(rel)
	dir, err := wsOpenDir(root, d, false)
	if err != nil {
		return nil, nil, err
	}
	defer dir.Close()
	f, fi, err := wsOpenEntry(dir, name, rel, os.O_RDONLY)
	if errors.Is(err, errWsIsDir) {
		err = errWsNotFile
	}
	return f, fi, err
}

var errWsNotFile = errors.New("not a regular file")

type wsEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix millis
}

// wsOut renders a cleaned relative path for the API ("" for the root).
func wsOut(rel string) string {
	if rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// GET /api/workspace/list?path=<rel>
func (s *Server) wsList(w http.ResponseWriter, r *http.Request) {
	rel, ok := wsClean(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "invalid path")
		return
	}
	root, err := s.wsRoot()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	dir, err := wsOpenDir(root, rel, false)
	if errors.Is(err, errWsNotDir) {
		writeErr(w, 400, "not a directory")
		return
	}
	if err != nil {
		wsReplyErr(w, err, "path does not exist")
		return
	}
	defer dir.Close()
	d, err := dir.Open(".")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]wsEntry, 0, len(ents))
	for _, e := range ents {
		info, err := e.Info() // lstat semantics: a link is listed as itself, never followed
		if err != nil {
			continue
		}
		out = append(out, wsEntry{
			Name:  e.Name(),
			Path:  wsOut(filepath.Join(rel, e.Name())),
			Dir:   e.IsDir(),
			Size:  info.Size(),
			MTime: info.ModTime().UnixMilli(),
		})
	}
	// Directories first, each sorted by name.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, 200, map[string]any{"path": wsOut(rel), "entries": out})
}

// GET /api/workspace/read?path=<rel> — inline text for view/edit. Binary or oversize
// files return {binary:true}/{too_large:true} with no content (use download instead).
func (s *Server) wsRead(w http.ResponseWriter, r *http.Request) {
	rel, ok := wsClean(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "invalid path")
		return
	}
	root, err := s.wsRoot()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	f, fi, err := wsOpenFile(root, rel)
	if errors.Is(err, errWsNotFile) {
		writeErr(w, 400, "is a directory or not a regular file, cannot be read as a file")
		return
	}
	if err != nil {
		wsReplyErr(w, err, "file does not exist")
		return
	}
	defer f.Close()
	if fi.Size() > maxWorkspaceRead {
		writeJSON(w, 200, map[string]any{"path": wsOut(rel), "size": fi.Size(), "too_large": true, "binary": true})
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, maxWorkspaceRead+1))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if len(data) > maxWorkspaceRead { // grew after the size check
		writeJSON(w, 200, map[string]any{"path": wsOut(rel), "size": int64(len(data)), "too_large": true, "binary": true})
		return
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		writeJSON(w, 200, map[string]any{"path": wsOut(rel), "size": fi.Size(), "binary": true})
		return
	}
	writeJSON(w, 200, map[string]any{"path": wsOut(rel), "size": fi.Size(), "binary": false, "content": string(data)})
}

// wsCreateFile creates or truncates the regular file rel through the root. The directories on the
// way are made and opened as wsOpenDir does; the file is then made by wsCreateIn.
func wsCreateFile(root *os.Root, rel string) (*os.File, error) {
	d, name := wsSplit(rel)
	dir, err := wsOpenDir(root, d, true)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return wsCreateIn(dir, name, rel)
}

// wsCreateIn creates or truncates the regular file name in dir. A new file is created with O_EXCL,
// which never opens an existing entry of any kind; an existing one is opened as wsOpenEntry does
// and truncated only once it is known to be the file that was looked at. A directory, a link or
// any other entry at the name is refused.
func wsCreateIn(dir *os.Root, name, rel string) (*os.File, error) {
	f, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag, 0o644)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	f, _, err = wsOpenEntry(dir, name, rel, os.O_WRONLY)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

var errWsIsDir = errors.New("target is a directory")

// POST /api/workspace/write  {path, content} — create/overwrite a text file.
func (s *Server) wsWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rel, ok := wsClean(req.Path)
	if !ok || rel == "." {
		writeErr(w, 400, "invalid path")
		return
	}
	root, err := s.wsRoot()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	f, err := wsCreateFile(root, rel)
	if errors.Is(err, errWsIsDir) {
		writeErr(w, 400, "target is a directory")
		return
	}
	if errors.Is(err, errWsNotFile) {
		writeErr(w, 400, "invalid path")
		return
	}
	if err != nil {
		wsReplyErr(w, err, "path does not exist")
		return
	}
	if _, err := f.WriteString(req.Content); err != nil {
		f.Close()
		writeErr(w, 500, err.Error())
		return
	}
	if err := f.Close(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": wsOut(rel)})
}

// POST /api/workspace/mkdir  {path}
func (s *Server) wsMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rel, ok := wsClean(req.Path)
	if !ok || rel == "." {
		writeErr(w, 400, "invalid path")
		return
	}
	root, err := s.wsRoot()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	dir, err := wsOpenDir(root, rel, true)
	if err != nil {
		wsReplyErr(w, err, "path does not exist")
		return
	}
	dir.Close()
	writeJSON(w, 200, map[string]any{"ok": true, "path": wsOut(rel)})
}

// DELETE /api/workspace/delete?path=<rel> — removes a file or a directory tree
// (confined to the work dir; the root itself can't be deleted). A link as the last
// component is removed as a link; a link in the middle of the path is refused.
func (s *Server) wsDelete(w http.ResponseWriter, r *http.Request) {
	rel, ok := wsClean(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "invalid path")
		return
	}
	if rel == "." {
		writeErr(w, 400, "cannot delete the workspace root directory")
		return
	}
	root, err := s.wsRoot()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	d, name := wsSplit(rel)
	dir, err := wsOpenDir(root, d, false)
	if err != nil {
		wsReplyErr(w, err, "path does not exist")
		return
	}
	defer dir.Close()
	if _, err := dir.Lstat(name); err != nil {
		wsReplyErr(w, err, "path does not exist")
		return
	}
	if wsWalkHook != nil {
		wsWalkHook(rel)
	}
	// name is one component of a directory that was opened and checked: RemoveAll removes a link
	// there as a link and does not traverse links below it.
	if err := dir.RemoveAll(name); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /api/workspace/download?path=<rel> — stream a file as an attachment.
func (s *Server) wsDownload(w http.ResponseWriter, r *http.Request) {
	rel, ok := wsClean(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "invalid path")
		return
	}
	root, err := s.wsRoot()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	f, fi, err := wsOpenFile(root, rel)
	if errors.Is(err, errWsLink) || errors.Is(err, errWsChanged) || errors.Is(err, errWsHardLink) {
		writeErr(w, 400, "invalid path")
		return
	}
	if err != nil {
		writeErr(w, 404, "file does not exist")
		return
	}
	defer f.Close()
	name := filepath.Base(rel)
	// RFC 5987 filename* keeps non-ASCII names intact; plain filename is the fallback.
	w.Header().Set("Content-Disposition", "attachment; filename=\""+sanitizeFilename(name)+"\"; filename*=UTF-8''"+url.PathEscape(name))
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// POST /api/workspace/upload?path=<dir> — multipart form field "file" (one or more).
func (s *Server) wsUpload(w http.ResponseWriter, r *http.Request) {
	dirRel, ok := wsClean(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "invalid path")
		return
	}
	root, err := s.wsRoot()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()
	dir, err := wsOpenDir(root, dirRel, false)
	if errors.Is(err, errWsLink) || errors.Is(err, errWsChanged) {
		writeErr(w, 400, "invalid path")
		return
	}
	if err != nil {
		writeErr(w, 400, "target directory does not exist")
		return
	}
	defer dir.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkspaceUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "failed to parse upload or size limit exceeded: "+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, 400, "missing upload file (form field file)")
		return
	}
	saved := 0
	for _, hdr := range files {
		name := filepath.Base(hdr.Filename) // strip any path component
		if name == "" || name == "." || name == ".." || name == string(os.PathSeparator) {
			continue
		}
		if err := saveUploadRoot(hdr, dir, name, filepath.Join(dirRel, name)); err != nil {
			if errors.Is(err, errWsLink) || errors.Is(err, errWsIsDir) || errors.Is(err, errWsNotFile) || errors.Is(err, errWsChanged) || errors.Is(err, errWsHardLink) {
				writeErr(w, 400, "invalid path")
				return
			}
			writeErr(w, 500, err.Error())
			return
		}
		saved++
	}
	writeJSON(w, 200, map[string]any{"uploaded": saved})
}

// saveUploadRoot writes one uploaded file to name in the checked directory dir, never following a
// link. rel is the path of the file in the work dir.
func saveUploadRoot(hdr *multipart.FileHeader, dir *os.Root, name, rel string) error {
	src, err := hdr.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := wsCreateIn(dir, name, rel)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// sanitizeFilename strips characters unsafe for a Content-Disposition filename token.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\"", "")
	name = strings.ReplaceAll(name, "\\", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.ReplaceAll(name, "\r", "")
	return name
}
