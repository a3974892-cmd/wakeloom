package main

import (
	"io/fs"
	"mime"
	"net/http"
	"path"
)

// These are UI-format workflows: open them in ComfyUI, never submit them as API graphs.
func profileWorkflowFile(w http.ResponseWriter, r *http.Request) {
	p := localProfiles()[r.PathValue("profile")]
	if p == nil || !p.builtin {
		http.NotFound(w, r)
		return
	}
	for _, wf := range p.Workflows {
		if wf.ID != r.PathValue("workflow") {
			continue
		}
		if wf.File == "" || !fs.ValidPath(wf.File) || path.Ext(wf.File) != ".json" {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(p.fsys, path.Join(p.dir, wf.File))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": wf.ID + ".json"}))
		w.Write(data)
		return
	}
	http.NotFound(w, r)
}
