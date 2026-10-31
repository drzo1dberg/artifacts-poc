// api is the control plane: it decides who may do what and tracks artifacts and versions in
// Postgres. It stores content in the blobstore and never serves it to browsers, the front door
// does that on a separate origin.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

const (
	defaultRetentionDays = 30
	janitorInterval      = 10 * time.Second
	contentTokenTTL      = 15 * time.Minute
	maxRetentionDays     = 3650
	maxBodyBytes         = 10 << 20
	maxFiles             = 64
)

var (
	validUser = regexp.MustCompile(`^[a-z0-9]{1,20}$`)
	validID   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	validPath = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`)
)

type server struct {
	db          *pgxpool.Pool
	blob        *blobClient
	contentHost string
	tokenKey    []byte
}

type file struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type version struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type artifactSummary struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	LiveVersion int    `json:"live_version"`
}

type artifact struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	LiveVersion   int       `json:"live_version"`
	RetentionDays int       `json:"retention_days"`
	ExpiresAt     time.Time `json:"expires_at"`
	BlobPrefix    string    `json:"blob_prefix"`
	Versions      []version `json:"versions"`
}

func main() {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(ctx, schema); err != nil {
		log.Fatal("apply schema: ", err)
	}
	tokenKey := os.Getenv("CONTENT_TOKEN_KEY")
	if tokenKey == "" {
		log.Fatal("CONTENT_TOKEN_KEY is required")
	}

	s := &server{
		db:          db,
		blob:        newBlobClient(os.Getenv("BLOB_URL")),
		contentHost: os.Getenv("CONTENT_HOST"),
		tokenKey:    []byte(tokenKey),
	}
	go s.runJanitor(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.auth(s.me))
	mux.HandleFunc("GET /api/artifacts", s.auth(s.listArtifacts))
	mux.HandleFunc("POST /api/artifacts", s.auth(s.createArtifact))
	mux.HandleFunc("GET /api/artifacts/{id}", s.authArtifact(s.getArtifact))
	mux.HandleFunc("DELETE /api/artifacts/{id}", s.authArtifact(s.deleteArtifact))
	mux.HandleFunc("POST /api/artifacts/{id}/versions", s.authArtifact(s.publishVersion))
	mux.HandleFunc("GET /api/artifacts/{id}/versions/{number}/files", s.authArtifact(s.readFiles))
	mux.HandleFunc("POST /api/artifacts/{id}/expire-now", s.authArtifact(s.expireNow))
	// Outside /api/ on purpose: the front door only forwards /api/ from the outside.
	mux.HandleFunc("GET /internal/authorize-content", s.authorizeContent)

	srv := &http.Server{Addr: ":8081", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Println("api listening on :8081")
	log.Fatal(srv.ListenAndServe())
}

// --- Fake login: the cookie just holds the name. Good enough for the POC, see README. ---

type handler func(w http.ResponseWriter, r *http.Request, user string)

func (s *server) auth(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("user")
		if err != nil || !validUser.MatchString(c.Value) {
			writeError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		next(w, r, c.Value)
	}
}

// authArtifact also rejects a malformed ID, which would otherwise reach a uuid column and fail with a 500.
func (s *server) authArtifact(next func(w http.ResponseWriter, r *http.Request, user, id string)) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request, user string) {
		id := r.PathValue("id")
		if !validID.MatchString(id) {
			writeError(w, http.StatusNotFound, "artifact not found")
			return
		}
		next(w, r, user, id)
	})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !validUser.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "name must be 1-20 characters of a-z and 0-9")
		return
	}
	// Exactly one container per user, created on first login.
	if err := s.blob.createContainer(r.Context(), containerFor(req.Name)); err != nil {
		internalError(w, err)
		return
	}
	_, err := s.db.Exec(r.Context(), `INSERT INTO users (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`, req.Name)
	if err != nil {
		internalError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "user", Value: req.Name, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	s.me(w, r, req.Name)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "user", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) me(w http.ResponseWriter, r *http.Request, user string) {
	writeJSON(w, http.StatusOK, map[string]string{"name": user, "container": containerFor(user)})
}

// --- Artifacts ---

func (s *server) listArtifacts(w http.ResponseWriter, r *http.Request, user string) {
	rows, err := s.db.Query(r.Context(), `
		SELECT id, title, live_version
		FROM artifacts WHERE owner_id = $1 AND deleted_at IS NULL
		ORDER BY updated_at DESC`, user)
	if err != nil {
		internalError(w, err)
		return
	}
	result, err := pgx.CollectRows(rows, pgx.RowToStructByName[artifactSummary])
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) getArtifact(w http.ResponseWriter, r *http.Request, user, id string) {
	var a artifact
	err := s.db.QueryRow(r.Context(), `
		SELECT id, title, live_version, retention_days, expires_at
		FROM artifacts WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL`, id, user).
		Scan(&a.ID, &a.Title, &a.LiveVersion, &a.RetentionDays, &a.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	a.BlobPrefix = "stlocal/" + containerFor(user) + "/" + a.ID + "/"

	rows, err := s.db.Query(r.Context(), `
		SELECT number FROM artifact_versions WHERE artifact_id = $1 ORDER BY number DESC`, id)
	if err != nil {
		internalError(w, err)
		return
	}
	a.Versions, err = pgx.CollectRows(rows, pgx.RowToStructByNameLax[version])
	if err != nil {
		internalError(w, err)
		return
	}
	for i := range a.Versions {
		a.Versions[i].URL = s.contentURL(a.ID, user, a.Versions[i].Number)
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *server) createArtifact(w http.ResponseWriter, r *http.Request, user string) {
	var req struct {
		Title         string `json:"title"`
		RetentionDays *int   `json:"retention_days"`
		Files         []file `json:"files"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	retention := defaultRetentionDays
	if req.RetentionDays != nil {
		retention = *req.RetentionDays
	}
	if retention < 1 || retention > maxRetentionDays {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("retention_days must be between 1 and %d", maxRetentionDays))
		return
	}
	if msg := validateFiles(req.Files); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO artifacts (owner_id, title, live_version, retention_days, expires_at)
		VALUES ($1, $2, 1, $3, now() + make_interval(days => $3))
		RETURNING id`, user, req.Title, retention).Scan(&id)
	if err != nil {
		internalError(w, err)
		return
	}
	if err := s.storeVersion(ctx, tx, user, id, 1, req.Files); err != nil {
		internalError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "version": 1})
}

// publishVersion is the core of the system: the client names the version its change is based on
// (base_version). If a newer one went live in the meantime it gets a 409 instead of overwriting that work.
func (s *server) publishVersion(w http.ResponseWriter, r *http.Request, user, id string) {
	var req struct {
		BaseVersion int    `json:"base_version"`
		Files       []file `json:"files"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if msg := validateFiles(req.Files); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	defer tx.Rollback(ctx)

	// FOR UPDATE serializes parallel publishes, which makes the check below safe.
	var live int
	err = tx.QueryRow(ctx, `
		SELECT live_version FROM artifacts
		WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL AND expires_at > now()
		FOR UPDATE`, id, user).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if req.BaseVersion != live {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"Conflict: version %d is live now, your work is based on version %d. Reopen the artifact and redo your change there.",
			live, req.BaseVersion))
		return
	}

	next := live + 1
	if err := s.storeVersion(ctx, tx, user, id, next, req.Files); err != nil {
		internalError(w, err)
		return
	}
	// Every publish restarts retention: the clock counts from the last change.
	_, err = tx.Exec(ctx, `
		UPDATE artifacts
		SET live_version = $2, updated_at = now(), expires_at = now() + make_interval(days => retention_days)
		WHERE id = $1`, id, next)
	if err != nil {
		internalError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "version": next})
}

// storeVersion uploads the blobs, then records the version. It only goes live with the caller's commit.
// If something fails, at worst blobs remain that nothing points to, and the next attempt clears them
// with the deletePrefix at the start.
func (s *server) storeVersion(ctx context.Context, tx pgx.Tx, user, id string, number int, files []file) error {
	container := containerFor(user)
	prefix := versionPrefix(id, number)
	if err := s.blob.deletePrefix(ctx, container, prefix); err != nil {
		return err
	}
	for _, f := range files {
		if err := s.blob.put(ctx, container, prefix+"/"+f.Path, f.Content); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO artifact_versions (artifact_id, number) VALUES ($1, $2)`, id, number)
	return err
}

func (s *server) readFiles(w http.ResponseWriter, r *http.Request, user, id string) {
	number := r.PathValue("number")
	ctx := r.Context()
	var n int
	err := s.db.QueryRow(ctx, `
		SELECT v.number FROM artifact_versions v JOIN artifacts a ON a.id = v.artifact_id
		WHERE a.id = $1 AND a.owner_id = $2 AND a.deleted_at IS NULL AND v.number::text = $3`,
		id, user, number).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	container, prefix := containerFor(user), versionPrefix(id, n)
	names, err := s.blob.list(ctx, container, prefix)
	if err != nil {
		internalError(w, err)
		return
	}
	files := make([]file, 0, len(names))
	for _, name := range names {
		content, err := s.blob.get(ctx, container, name)
		if err != nil {
			internalError(w, err)
			return
		}
		files = append(files, file{Path: strings.TrimPrefix(name, prefix+"/"), Content: content})
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *server) deleteArtifact(w http.ResponseWriter, r *http.Request, user, id string) {
	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	var found string
	err = tx.QueryRow(ctx, `
		SELECT id FROM artifacts WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL FOR UPDATE`,
		id, user).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if err := s.purge(ctx, tx, id, containerFor(user), "deleted by owner"); err != nil {
		internalError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// expireNow exists for the demo only, so nobody has to wait 30 days for the retention job.
func (s *server) expireNow(w http.ResponseWriter, r *http.Request, user, id string) {
	tag, err := s.db.Exec(r.Context(), `
		UPDATE artifacts SET expires_at = now()
		WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL`, id, user)
	if err != nil {
		internalError(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authorizeContent is asked by the front door before every read, cache hits included. So a delete
// takes effect immediately, at the price that reads now depend on the API and the database.
func (s *server) authorizeContent(w http.ResponseWriter, r *http.Request) {
	claims, ok := verifyToken(s.tokenKey, r.Header.Get("X-Content-Token"), time.Now())
	if !ok || claims.Artifact != r.Header.Get("X-Artifact") || claims.Owner != r.Header.Get("X-Owner") ||
		fmt.Sprintf("v%d", claims.Version) != r.Header.Get("X-Version") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var alive bool
	err := s.db.QueryRow(r.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM artifacts
			WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL AND expires_at > now()
		)`, claims.Artifact, claims.Owner).Scan(&alive)
	if err != nil {
		internalError(w, err)
		return
	}
	if !alive {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Retention ---

// runJanitor stands in for a storage account lifecycle policy.
func (s *server) runJanitor(ctx context.Context) {
	for range time.Tick(janitorInterval) {
		if err := s.purgeExpired(ctx); err != nil {
			log.Println("retention:", err)
		}
	}
}

func (s *server) purgeExpired(ctx context.Context) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// SKIP LOCKED skips artifacts a publish is working on right now.
	rows, err := tx.Query(ctx, `
		SELECT id, owner_id FROM artifacts
		WHERE deleted_at IS NULL AND expires_at <= now()
		ORDER BY expires_at LIMIT 100
		FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return err
	}
	type expired struct {
		ID      string
		OwnerID string
	}
	expiredRows, err := pgx.CollectRows(rows, pgx.RowToStructByName[expired])
	if err != nil {
		return err
	}
	for _, e := range expiredRows {
		container := containerFor(e.OwnerID)
		if err := s.purge(ctx, tx, e.ID, container, "retention expired"); err != nil {
			return err
		}
		log.Printf("retention: removed %s/%s", container, e.ID)
	}
	return tx.Commit(ctx)
}

// purge deletes the blobs first, then writes the tombstone. If the process dies in between,
// the next attempt simply runs again, because deletePrefix is idempotent.
func (s *server) purge(ctx context.Context, tx pgx.Tx, id, container, reason string) error {
	if err := s.blob.deletePrefix(ctx, container, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE artifacts SET deleted_at = now(), delete_reason = $2 WHERE id = $1`, id, reason)
	return err
}

// --- Helpers ---

func containerFor(user string) string {
	return "u-" + user
}

func versionPrefix(id string, number int) string {
	return fmt.Sprintf("%s/v%d", id, number)
}

// contentURL builds the link the iframe loads, with a short-lived token for the logged-in owner.
// The front door maps the hostname back to container and folder.
func (s *server) contentURL(id, user string, number int) string {
	token := signToken(s.tokenKey, contentClaims{
		Artifact: id,
		Owner:    user,
		Version:  number,
		Expires:  time.Now().Add(contentTokenTTL).Unix(),
	})
	return fmt.Sprintf("http://%s--%s.%s/t/%s/v%d/index.html", id, user, s.contentHost, token, number)
}

func validateFiles(files []file) string {
	if len(files) > maxFiles {
		return fmt.Sprintf("at most %d files are allowed", maxFiles)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if !validPath.MatchString(f.Path) {
			return fmt.Sprintf("invalid file path %q", f.Path)
		}
		if seen[f.Path] {
			return fmt.Sprintf("duplicate file path %q", f.Path)
		}
		seen[f.Path] = true
	}
	if !seen["index.html"] {
		return "index.html is required"
	}
	return ""
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func internalError(w http.ResponseWriter, err error) {
	log.Println("error:", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}
