// blobstore simulates a storage account on the filesystem:
//
//	Storage account  ->  /data/stlocal
//	Container        ->  /data/stlocal/u-alice
//	Blob             ->  /data/stlocal/u-alice/<artifact-id>/v1/index.html
//
// As in flat-namespace Azure storage, the API has no real folders, only blob names containing "/".
// The service knows nothing about users or artifacts, only containers and blobs.
package main

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const root = "/data/stlocal"

var validContainer = regexp.MustCompile(`^[a-z0-9-]{3,63}$`)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /{container}", createContainer)
	mux.HandleFunc("GET /{container}", listBlobs)
	mux.HandleFunc("DELETE /{container}", deletePrefix)
	mux.HandleFunc("PUT /{container}/{blob...}", putBlob)
	mux.HandleFunc("GET /{container}/{blob...}", getBlob)

	log.Println("blobstore listening on :9000")
	log.Fatal(http.ListenAndServe(":9000", mux))
}

// resolve builds the file path and rejects anything that would leave the container.
func resolve(container, name string) (string, bool) {
	if !validContainer.MatchString(container) || !filepath.IsLocal(name) {
		return "", false
	}
	return filepath.Join(root, container, name), true
}

func createContainer(w http.ResponseWriter, r *http.Request) {
	dir, ok := resolve(r.PathValue("container"), ".")
	if !ok {
		http.Error(w, "invalid container name", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func putBlob(w http.ResponseWriter, r *http.Request) {
	container, name := r.PathValue("container"), r.PathValue("blob")
	path, ok := resolve(container, name)
	if !ok {
		http.Error(w, "invalid blob name", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(filepath.Join(root, container)); err != nil {
		http.Error(w, "container not found", http.StatusNotFound)
		return
	}
	if err := writeAtomic(path, r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// writeAtomic writes to a temp file first so a half-written blob is never readable.
func writeAtomic(path string, body io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func getBlob(w http.ResponseWriter, r *http.Request) {
	path, ok := resolve(r.PathValue("container"), r.PathValue("blob"))
	if !ok {
		http.Error(w, "invalid blob name", http.StatusBadRequest)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	// ServeContent derives the Content-Type from the file extension.
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

// listBlobs returns all blob names below ?prefix= as a JSON array.
func listBlobs(w http.ResponseWriter, r *http.Request) {
	container := r.PathValue("container")
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		prefix = "."
	}
	dir, ok := resolve(container, prefix)
	if !ok {
		http.Error(w, "invalid prefix", http.StatusBadRequest)
		return
	}
	names := []string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !strings.HasPrefix(d.Name(), ".upload-") {
			rel, err := filepath.Rel(filepath.Join(root, container), path)
			if err != nil {
				return err
			}
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(names)
}

// deletePrefix deletes a whole "folder". It is idempotent, a missing folder is not an error.
func deletePrefix(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	dir, ok := resolve(r.PathValue("container"), prefix)
	if !ok || prefix == "" || filepath.Clean(prefix) == "." {
		http.Error(w, "a non-empty prefix is required", http.StatusBadRequest)
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
