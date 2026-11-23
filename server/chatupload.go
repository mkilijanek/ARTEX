package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/sandbox"
)

// maxChatUpload caps a single chat-attachment upload request (memory + spill).
const maxChatUpload = 128 << 20 // 128 MiB

// safeChatID guards the {id} path segment against traversal — task ids are numeric,
// session ids are alnum/_/- ; anything with "/" or ".." is rejected.
var safeChatID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// chatAttachment is one uploaded file as the frontend + agent see it. Path is relative
// to the chat's working dir (e.g. "uploads/report.txt"), which is the agent's CWD, so
// it can Read/Bash the file directly; Name/Size drive the UI card.
type chatAttachment struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Abs is the absolute on-disk path (m.dir is already absolute). For pre-task staging
	// (scope=staging) the frontend uses it to write the prompt into the description; task/session use composeAgentMessage, which builds the path in the backend.
	Abs string `json:"abs,omitempty"`
}

// chatUpload implements method-1 file support: it saves one or more files into a chat's
// working dir under uploads/, so the agent opens them with its existing Read/Bash tools
// and the sent message carries their paths. No LLM-layer change, no multimodal.
//
// POST /api/chat/upload?scope=task|session|staging&id=<id>, multipart field "file"
// (repeatable). Returns {attachments:[{name,path,size,abs}]}. Target dir mirrors the
// agent CWD layout:
//
//	scope=task    → <workDir>/tasks/<id>/uploads/
//	scope=session → <workDir>/sessions/<id>/uploads/
//	scope=staging → <workDir>/drafts/<id>/uploads/   (pre-task staging: the task has no ID yet;
//	                files land here first and the frontend writes their abs paths into the task description)
func (s *Server) chatUpload(w http.ResponseWriter, r *http.Request) {
	var sub string
	taskScoped := false
	switch r.URL.Query().Get("scope") {
	case "task":
		sub = "tasks"
		taskScoped = true
	case "session":
		sub = "sessions"
	case "staging":
		sub = "drafts"
	default:
		writeErr(w, 400, "scope must be task / session / staging")
		return
	}
	id := r.URL.Query().Get("id")
	if !safeChatID.MatchString(id) {
		writeErr(w, 400, "invalid id")
		return
	}
	if taskScoped {
		if s.m.ResolveTask(id) == nil {
			writeErr(w, 404, "task not found")
			return
		}
		if !s.engine.beginTaskOperation(id) {
			writeErr(w, http.StatusConflict, "Task is being deleted; cannot upload attachments")
			return
		}
		defer s.engine.decInflight(id)
	}
	up, err := s.openUploadDir(sub, id)
	if err != nil {
		writeErr(w, 500, "failed to create directory: "+err.Error())
		return
	}
	defer up.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxChatUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "failed to parse the upload or size limit exceeded: "+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeErr(w, 400, "missing upload file (form field file)")
		return
	}
	handToTools := sub != "drafts" && sandbox.Current().On() && os.Geteuid() == 0
	out := make([]chatAttachment, 0, len(files))
	for _, hdr := range files {
		name := filepath.Base(hdr.Filename) // strip any path component
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			continue
		}
		base, err := saveUpload(hdr, up, name, handToTools)
		if err != nil {
			writeErr(w, 500, "Save failed: "+err.Error())
			return
		}
		out = append(out, chatAttachment{Name: base, Path: "uploads/" + base, Size: hdr.Size, Abs: filepath.Join(s.m.dir, sub, id, "uploads", base)})
	}
	writeJSON(w, 200, map[string]any{"attachments": out})
}

// openUploadDir opens <data>/<sub>/<id>/uploads as a root, making what is missing without
// following a link. A task or session directory is the tool user's workspace (0700, tool-owned
// when isolation is on), so the tools can use what is uploaded. A draft directory is the server's
// (0755, below a 0711 drafts directory): the model reads the files but cannot change, replace or
// remove them.
func (s *Server) openUploadDir(sub, id string) (*os.Root, error) {
	if sub == "drafts" {
		top, err := sandbox.OpenServerDir(s.m.dir, 0o711, sub)
		if err != nil {
			return nil, err
		}
		top.Close()
		return sandbox.OpenServerDir(filepath.Join(s.m.dir, sub), 0o755, id, "uploads")
	}
	top, err := sandbox.OpenServerDir(s.m.dir, 0o711, sub)
	if err != nil {
		return nil, err
	}
	top.Close()
	return sandbox.Current().OpenWorkspace(filepath.Join(s.m.dir, sub), id, "uploads")
}

// maxUploadNames bounds the search for a free name.
const maxUploadNames = 10000

// saveUpload writes one uploaded file into up under name, or name-1, name-2... when that exists,
// so a re-upload never clobbers an earlier attachment. Every candidate is created exclusively, so
// an existing entry of any kind, a link planted by a tool included, is skipped and never written
// through. It returns the name used.
func saveUpload(hdr *multipart.FileHeader, up *os.Root, name string, handToTools bool) (string, error) {
	src, err := hdr.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	var out *os.File
	used := name
	for i := 0; ; i++ {
		if i >= maxUploadNames {
			return "", errors.New("no unused file name")
		}
		used = name
		if i > 0 {
			used = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		out, err = createExcl(up, used)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	fail := func(err error) (string, error) {
		out.Close()
		_ = up.Remove(used)
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		return fail(err)
	}
	if handToTools {
		if err := out.Chown(sandbox.ToolUID, sandbox.ToolGID); err != nil {
			return fail(err)
		}
	}
	if err := out.Close(); err != nil {
		_ = up.Remove(used)
		return "", err
	}
	return used, nil
}

// composeAgentMessage appends an attachment manifest to the user's message so the agent
// knows which files were uploaded and where to Read them. baseDir is the agent's working
// dir (its CWD); we emit ABSOLUTE paths (baseDir + relative) so the agent can Read/Bash
// them unambiguously regardless of how it interprets relative paths.
func composeAgentMessage(msg string, atts []chatAttachment, baseDir string) string {
	if len(atts) == 0 {
		return msg
	}
	var b strings.Builder
	b.WriteString(msg)
	b.WriteString("\n\n[User-uploaded attachments] (absolute paths; use Read/Bash to view them):")
	for _, a := range atts {
		fmt.Fprintf(&b, "\n- %s (%s)", filepath.Join(baseDir, a.Path), humanBytes(a.Size))
	}
	return b.String()
}

// userActivityWithAttachments builds the persisted 'user' activity. With attachments,
// Detail holds JSON {text, attachments} so the transcript renders text + attachment
// cards; Summary stays the plain text (the list payload omits Detail, lazy-loaded).
func userActivityWithAttachments(worker, text string, atts []chatAttachment) db.Activity {
	a := db.Activity{Worker: worker, Kind: "user", Summary: text}
	if len(atts) > 0 {
		blob, _ := json.Marshal(map[string]any{"text": text, "attachments": atts})
		a.Detail = string(blob)
	}
	return a
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
