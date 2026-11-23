package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// AC31: every filesystem call made through package os (or filepath.Walk*, http.ServeFile) in a
// file that defines model-callable tools is on a reviewed list. A model-callable tool runs in the
// server, with the server's rights, on paths the model or a tool process can influence; a new
// call in such a file must be read by hand and added here with the reason it is safe. The check
// is by (file, function, call), not by line, so editing a function does not move entries.
//
// Calls through an os.Root are methods on a root and are not listed: they are the confined form.
// os.OpenRoot itself is listed because it opens a path.

// fsCalls are the package-level functions that open, create, change or look at a path.
var fsCalls = map[string]map[string]bool{
	"os": {"Open": true, "OpenFile": true, "Create": true, "CreateTemp": true, "MkdirTemp": true, "Mkdir": true,
		"MkdirAll": true, "ReadFile": true, "WriteFile": true, "ReadDir": true, "Remove": true, "RemoveAll": true,
		"Rename": true, "Stat": true, "Lstat": true, "Chmod": true, "Chown": true, "Lchown": true, "Chtimes": true,
		"Symlink": true, "Link": true, "Readlink": true, "Truncate": true, "DirFS": true, "OpenRoot": true,
		"TempDir": true, "Getwd": true},
	"filepath": {"Walk": true, "WalkDir": true, "Glob": true, "EvalSymlinks": true},
	"http":     {"ServeFile": true},
}

// toolDefiners are the calls that make a file one that defines model-callable tools.
var toolDefiners = map[string]bool{"wrTool": true, "roTool": true, "Build": true, "NewBash": true}

// Directories of the repository that are not scanned: the SDK is provenance-pinned and its file
// tools run in the helper process (AC8-AC10); sandbox and cmd are the isolation itself; the rest
// is not Go code of the server.
var fsArchSkip = map[string]bool{"third_party": true, "sandbox": true, "cmd": true, "web": true, "scripts": true,
	"docs": true, "extension": true, "screenshots": true, "skills": true, ".git": true, ".github": true, "docker": true, "node_modules": true}

type fsHit struct{ key, pos string }

func scanToolFiles(t *testing.T, root string) (hits []fsHit, files []string) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && fsArchSkip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		defines := false
		ast.Inspect(file, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				switch f := c.Fun.(type) {
				case *ast.Ident:
					defines = defines || toolDefiners[f.Name]
				case *ast.SelectorExpr:
					if id, ok := f.X.(*ast.Ident); ok && (id.Name == "actool" || id.Name == "tool") {
						defines = defines || toolDefiners[f.Sel.Name]
					}
				}
			}
			return true
		})
		if !defines {
			return nil
		}
		files = append(files, rel)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				name = recvName(fn.Recv.List[0].Type) + "." + name
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || !fsCalls[id.Name][sel.Sel.Name] {
					return true
				}
				hits = append(hits, fsHit{rel + ":" + name + ":" + id.Name + "." + sel.Sel.Name, fset.Position(c.Pos()).String()})
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return hits, files
}

func recvName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return recvName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return recvName(x.X)
	}
	return "?"
}

func TestToolFilesUseOnlyReviewedFilesystemCalls(t *testing.T) {
	hits, files := scanToolFiles(t, "..")
	if len(files) < 5 {
		t.Fatalf("only %d tool-defining files found %v: the scan lost its criterion", len(files), files)
	}
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.key] = true
		if reviewedFS[h.key] == "" {
			t.Errorf("%s: %s is not on the reviewed list (add it to reviewedFS with the reason it is safe, or make it an os.Root call)", h.pos, h.key)
		}
	}
	for k, why := range reviewedFS {
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s: no reason given", k)
		}
		if !seen[k] {
			t.Errorf("%s is on the reviewed list but the call is gone: remove the entry", k)
		}
	}
}

// The scanner must catch what it is for.
func TestToolFilesScannerFindsAnUnreviewedCall(t *testing.T) {
	dir := t.TempDir()
	src := "package x\nimport (\"os\"\nactool \"github.com/Autumn-27/norma/tool\")\nfunc a() { _ = actool.Build(actool.Spec{}) }\nfunc b() { os.ReadFile(\"/etc/passwd\"); os.OpenRoot(\"x\") }\nfunc (s *S) c() { os.Create(\"y\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "tool.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	other := "package x\nimport \"os\"\nfunc d() { os.ReadFile(\"/etc/passwd\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "plain.go"), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, files := scanToolFiles(t, dir)
	if len(files) != 1 || files[0] != "tool.go" {
		t.Fatalf("files %v", files)
	}
	var keys []string
	for _, h := range hits {
		keys = append(keys, h.key)
	}
	want := "tool.go:b:os.OpenRoot tool.go:b:os.ReadFile tool.go:S.c:os.Create"
	if got := strings.Join(keys, " "); got != want && !sameSet(keys, strings.Fields(want)) {
		t.Errorf("hits %v", keys)
	}
}

func sameSet(a, b []string) bool {
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, " ") == strings.Join(b, " ")
}

// reviewedFS is the list. Each entry was read by hand against the code it names.
var reviewedFS = map[string]string{
	"server/ask_operator.go:stageAskOperatorImage:os.Lstat":           "the task directory of this run, a server-built path (data/tasks/<id>); refused unless it is a real directory, and everything below it is opened through os.Root",
	"server/ask_operator.go:stageAskOperatorImage:os.OpenRoot":        "the confined form: the source and the copy are both opened relative to this root (AC29)",
	"server/customtool.go:Server.httpProxyTransport:os.ReadFile":      "reads the CA certificate the server itself named (ProxyCACert, the published copy under data/public); read-only, never a model path",
	"server/platform_tools.go:Server.openSkillDir:os.OpenRoot":        "the confined form: every skill path is resolved inside the skill's own directory (AC40)",
	"server/platform_tools.go:Server.toolCreateSkill:os.MkdirAll":     "makes the server-chosen skills directory itself (s.skillDir, not a model value); the model's skill name is created through the root below it",
	"server/platform_tools.go:Server.toolCreateSkill:os.OpenRoot":     "the confined form on the skills directory",
	"traffic/traffic.go:Open:os.MkdirAll":                             "the store's own directories at start (traffic, _index, _blobs, _ca): fixed names under the root-only data/traffic",
	"traffic/traffic.go:Traffic.Blob:os.Open":                         "path from blobPath: the hash is validated as 64 hex digits before any filesystem access, so it cannot leave _blobs; the store is root-only (AC4)",
	"traffic/traffic.go:Traffic.blobPath:os.Stat":                     "probes the two bucket layouts of a hash validated as 64 hex digits",
	"traffic/traffic.go:Traffic.Get:os.ReadFile":                      "legacy exchanges only: the relative path comes from the store's own SQLite index, not from the tool input (which is an exchange id)",
	"traffic/traffic.go:Traffic.RecoverHostDeleteStages:os.Lstat":     "start-up recovery of the store's own host-delete staging journal; server data, no model input",
	"traffic/traffic.go:Traffic.RecoverHostDeleteStages:os.ReadDir":   "start-up recovery of the store's own host-delete staging journal; server data, no model input",
	"traffic/traffic.go:Traffic.RecoverHostDeleteStages:os.ReadFile":  "start-up recovery of the store's own host-delete staging journal; server data, no model input",
	"traffic/traffic.go:Traffic.RecoverHostDeleteStages:os.RemoveAll": "start-up recovery of the store's own host-delete staging journal; server data, no model input",
	"traffic/traffic.go:Traffic.allHostTrees:os.ReadDir":              "lists the store directory; entries that are not host trees are skipped by the storeDirs and host-name checks",
	"traffic/traffic.go:Traffic.gcBlobs:filepath.WalkDir":             "blob garbage collection inside _blobs, started by the server",
	"traffic/traffic.go:Traffic.gcBlobs:os.Remove":                    "blob garbage collection inside _blobs, started by the server",
	"traffic/traffic.go:Traffic.indexBytes:os.Stat":                   "size of the store's own index file",
	"traffic/traffic.go:Traffic.legacyBlobRefs:filepath.WalkDir":      "reads the store's legacy per-host trees for blob references; started by the server",
	"traffic/traffic.go:Traffic.legacyBlobRefs:os.ReadFile":           "reads the store's legacy per-host trees for blob references; started by the server",
	"traffic/traffic.go:Traffic.reapStage:os.RemoveAll":               "removes a staging directory the store made",
	"traffic/traffic.go:Traffic.spill:os.MkdirAll":                    "writes a recorded body to _blobs/sha256/<2 hex>/<64 hex>.bin: the name is the body's SHA-256 and the directory is fixed, so the content (target data) cannot choose the path",
	"traffic/traffic.go:Traffic.spill:os.Stat":                        "same blob path as above",
	"traffic/traffic.go:Traffic.spill:os.WriteFile":                   "same blob path as above",
	"traffic/traffic.go:Traffic.stageTreesForArchive:os.Lstat":        "host archive staging inside the store; host directory names validated (legacyHostDir), journal created with O_EXCL",
	"traffic/traffic.go:Traffic.stageTreesForArchive:os.MkdirAll":     "host archive staging inside the store; host directory names validated (legacyHostDir), journal created with O_EXCL",
	"traffic/traffic.go:Traffic.stageTreesForArchive:os.MkdirTemp":    "host archive staging inside the store; host directory names validated (legacyHostDir), journal created with O_EXCL",
	"traffic/traffic.go:Traffic.stageTreesForArchive:os.RemoveAll":    "host archive staging inside the store; host directory names validated (legacyHostDir), journal created with O_EXCL",
	"traffic/traffic.go:Traffic.stageTreesForArchive:os.Rename":       "host archive staging inside the store; host directory names validated (legacyHostDir), journal created with O_EXCL",
	"traffic/traffic.go:restoreTrees:os.Lstat":                        "puts staged host trees back inside the store after a failed delete; names from the journal the store wrote",
	"traffic/traffic.go:restoreTrees:os.RemoveAll":                    "puts staged host trees back inside the store after a failed delete; names from the journal the store wrote",
	"traffic/traffic.go:restoreTrees:os.Rename":                       "puts staged host trees back inside the store after a failed delete; names from the journal the store wrote",
	"traffic/traffic.go:writeHostDeleteStageJournal:os.OpenFile":      "the store's own journal file, O_EXCL, mode 0600, in a directory it just made",
}
