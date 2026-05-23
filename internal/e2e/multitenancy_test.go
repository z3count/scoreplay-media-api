package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scoreplay/media-api/internal/adapter/http/handler"
	pgadapter "github.com/scoreplay/media-api/internal/adapter/postgres"
	appserver "github.com/scoreplay/media-api/internal/adapter/server"
	"github.com/scoreplay/media-api/internal/adapter/storage/local"
	"github.com/scoreplay/media-api/internal/service"
)

// TestE2E_TenantIsolation is the load-bearing test for the multi-tenancy
// implementation. It provisions two distinct tenants in the same database
// + storage + HTTP server, then proves that a request authenticated as
// tenant A cannot see, modify, or even enumerate tenant B's data.
//
// Coverage:
//   - List endpoints scope by tenant (no rows leak across).
//   - Get-by-ID across tenants returns 404 (not 200 or 403 — 404 prevents
//     enumeration of "this id exists in some tenant").
//   - Mutations against another tenant's row return 404 / 400.
//   - File storage uses the per-tenant key prefix.
//   - Same tag name in two tenants is allowed (no global UNIQUE).
//   - Same Idempotency-Key value in two tenants is allowed.
func TestE2E_TenantIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	env := setupMultiTenantEnv(t)

	// ----- Provision two tenants, each with one full-privilege key -----
	keyA := mustCreateTenant(t, env, "alpha-club")
	keyB := mustCreateTenant(t, env, "beta-club")

	clientA := newClient(env.baseURL, keyA)
	clientB := newClient(env.baseURL, keyB)

	// ----- Same tag name in both tenants is allowed -----
	tagAID := mustCreateTag(t, clientA, "Mbappé")
	tagBID := mustCreateTag(t, clientB, "Mbappé")
	if tagAID == tagBID {
		t.Fatal("expected distinct tag ids across tenants for the same name")
	}

	// ----- Each tenant uploads one media -----
	mediaAID, mediaAPath := mustUploadMedia(t, clientA, "Goal A", tagAID)
	mediaBID, mediaBPath := mustUploadMedia(t, clientB, "Goal B", tagBID)

	// File paths must be tenant-prefixed and distinct.
	if !strings.Contains(mediaAPath, "/") {
		t.Fatalf("tenant-A media path missing prefix: %q", mediaAPath)
	}
	if !strings.Contains(mediaBPath, "/") {
		t.Fatalf("tenant-B media path missing prefix: %q", mediaBPath)
	}
	if strings.Split(mediaAPath, "/")[0] == strings.Split(mediaBPath, "/")[0] {
		t.Fatalf("expected distinct tenant prefixes, got same: %q vs %q", mediaAPath, mediaBPath)
	}

	// ----- LIST: each tenant only sees its own rows -----
	tagsA := mustListTagNames(t, clientA)
	tagsB := mustListTagNames(t, clientB)
	if len(tagsA) != 1 || tagsA[0] != "Mbappé" {
		t.Errorf("tenant A list: want [Mbappé], got %v", tagsA)
	}
	if len(tagsB) != 1 || tagsB[0] != "Mbappé" {
		t.Errorf("tenant B list: want [Mbappé], got %v", tagsB)
	}

	mediaA := mustListMedia(t, clientA)
	mediaB := mustListMedia(t, clientB)
	if len(mediaA) != 1 || mediaA[0] != mediaAID {
		t.Errorf("tenant A media list: want [%s], got %v", mediaAID, mediaA)
	}
	if len(mediaB) != 1 || mediaB[0] != mediaBID {
		t.Errorf("tenant B media list: want [%s], got %v", mediaBID, mediaB)
	}

	// ----- GET-BY-ID: cross-tenant access is 404 (enumeration-safe) -----
	if got := getStatus(t, clientA, "/api/v1/media/"+mediaBID); got != http.StatusNotFound {
		t.Errorf("A→B media GET: want 404, got %d", got)
	}
	if got := getStatus(t, clientB, "/api/v1/media/"+mediaAID); got != http.StatusNotFound {
		t.Errorf("B→A media GET: want 404, got %d", got)
	}

	// ----- DELETE across tenants is 404 -----
	if got := deleteStatus(t, clientA, "/api/v1/media/"+mediaBID); got != http.StatusNotFound {
		t.Errorf("A→B media DELETE: want 404, got %d", got)
	}
	// And B's media is still there afterwards.
	if got := getStatus(t, clientB, "/api/v1/media/"+mediaBID); got != http.StatusOK {
		t.Errorf("B media still readable after A's failed delete: want 200, got %d", got)
	}

	// ----- ATTACH a tag across tenants is 404 (media not found in A's tenant) -----
	body := bytes.NewBufferString(`{"tags":["` + tagBID + `"]}`)
	req, _ := http.NewRequest(http.MethodPost, env.baseURL+"/api/v1/media/"+mediaBID+"/tags", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := clientA.Do(req)
	if err != nil {
		t.Fatalf("attach A→B: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("A attaching to B's media: want 404, got %d", resp.StatusCode)
	}

	// ----- Idempotency-Key namespace is per-tenant -----
	// Both tenants use the same key value. Each must see its own cached
	// response, NOT the other tenant's.
	const sharedKey = "abc-shared-idempotency-key"
	idemA1ID, _ := mustUploadMediaWithIdempotency(t, clientA, "Idem A", sharedKey)
	idemB1ID, _ := mustUploadMediaWithIdempotency(t, clientB, "Idem B", sharedKey)
	if idemA1ID == idemB1ID {
		t.Fatalf("idempotency key collision across tenants: %s == %s", idemA1ID, idemB1ID)
	}
	// Replay A's request with the same key — must return A's cached row,
	// not B's. Verifies the idempotency cache is partitioned by tenant.
	idemA2ID, _ := mustUploadMediaWithIdempotency(t, clientA, "Idem A again", sharedKey)
	if idemA2ID != idemA1ID {
		t.Errorf("A idempotency replay: want %s, got %s", idemA1ID, idemA2ID)
	}
}

// --- helpers -----------------------------------------------------------

type tenantTestEnv struct {
	baseURL      string
	authVerifier *pgadapter.AuthVerifier
}

// setupMultiTenantEnv builds an HTTP server that uses real Postgres +
// real local storage, with no dev-mode bypass — every request must
// present a valid credential.
func setupMultiTenantEnv(t *testing.T) *tenantTestEnv {
	t.Helper()
	pgEnv := setupTestEnv(t) // reuses postgres container + migrations

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	uploadDir := t.TempDir()
	storage, err := local.NewStorage(uploadDir)
	if err != nil {
		t.Fatalf("init storage: %v", err)
	}

	tagRepo := pgadapter.NewTagRepo(pgEnv.db)
	mediaRepo := pgadapter.NewMediaRepo(pgEnv.db)
	tagSvc := service.NewTagService(tagRepo)
	mediaSvc := service.NewMediaService(mediaRepo, tagRepo, storage, "http://localhost:0", logger)
	tagHandler := handler.NewTagHandler(tagSvc, logger)
	mediaHandler := handler.NewMediaHandler(mediaSvc, logger, 10*1024*1024)
	healthHandler := handler.NewHealthHandler(pgEnv.db)
	idempotencyStore := pgadapter.NewIdempotencyStore(pgEnv.db)

	authVerifier := pgadapter.NewAuthVerifier(pgEnv.db, 0)
	if err := authVerifier.EnsureLegacyTenant(context.Background(), ""); err != nil {
		t.Fatalf("ensure legacy tenant: %v", err)
	}

	router := appserver.NewRouter(healthHandler, tagHandler, mediaHandler, uploadDir, authVerifier, nil, nil, 30*time.Second, 100, 200, idempotencyStore, logger)
	srv := &http.Server{Handler: router}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	env := &tenantTestEnv{
		baseURL:      fmt.Sprintf("http://%s", ln.Addr().String()),
		authVerifier: authVerifier,
	}
	testBaseURL = env.baseURL
	return env
}

// mustCreateTenant provisions a brand-new tenant with one admin:* key
// and returns the raw key value (shown once at creation time).
func mustCreateTenant(t *testing.T, env *tenantTestEnv, name string) string {
	t.Helper()
	ctx := context.Background()
	tenantID, err := env.authVerifier.CreateTenant(ctx, name)
	if err != nil {
		t.Fatalf("create tenant %s: %v", name, err)
	}
	_, rawKey, _, err := env.authVerifier.CreateAPIKey(ctx, tenantID, name+"-key", []string{"admin:*"})
	if err != nil {
		t.Fatalf("create api key for %s: %v", name, err)
	}
	return rawKey
}

// newClient returns an http.Client that auto-injects the X-API-Key.
func newClient(_ string, apiKey string) *http.Client {
	return &http.Client{
		Transport: &authTransport{apiKey: apiKey, base: http.DefaultTransport},
	}
}

func mustCreateTag(t *testing.T, c *http.Client, name string) string {
	t.Helper()
	body := bytes.NewBufferString(`{"name":"` + name + `"}`)
	resp, err := postJSON(c, mustBaseURL(t, c)+"/api/v1/tags", body)
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		dump, _ := io.ReadAll(resp.Body)
		t.Fatalf("create tag: status %d, body=%s", resp.StatusCode, dump)
	}
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&env)
	return env.Data.ID
}

func mustUploadMedia(t *testing.T, c *http.Client, name string, tagID string) (mediaID, filePath string) {
	t.Helper()
	return uploadMediaWithIdempotency(t, c, name, tagID, "")
}

func mustUploadMediaWithIdempotency(t *testing.T, c *http.Client, name, idemKey string) (mediaID, filePath string) {
	t.Helper()
	return uploadMediaWithIdempotency(t, c, name, "", idemKey)
}

func uploadMediaWithIdempotency(t *testing.T, c *http.Client, name, tagID, idemKey string) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", name)
	if tagID != "" {
		_ = mw.WriteField("tags", tagID)
	}
	part, _ := mw.CreateFormFile("file", "photo.jpg")
	// Minimal JPEG magic + padding so the content-type sniffer accepts it.
	part.Write(append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte("x"), 600)...))
	mw.Close()

	req, _ := http.NewRequest(http.MethodPost, mustBaseURL(t, c)+"/api/v1/media", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("upload media: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		dump, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload media: status %d, body=%s", resp.StatusCode, dump)
	}
	var env struct {
		Data struct {
			ID       string `json:"id"`
			FileURL  string `json:"fileUrl"`
			FilePath string `json:"filePath"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&env)
	// FilePath is the relative path under /uploads/. The server returns
	// FileURL = baseURL + /uploads/ + path, so extract the path back out.
	path := env.Data.FilePath
	if path == "" && env.Data.FileURL != "" {
		const sep = "/uploads/"
		if idx := strings.Index(env.Data.FileURL, sep); idx >= 0 {
			path = env.Data.FileURL[idx+len(sep):]
		}
	}
	return env.Data.ID, path
}

func mustListTagNames(t *testing.T, c *http.Client) []string {
	t.Helper()
	resp, err := c.Get(mustBaseURL(t, c) + "/api/v1/tags?limit=100")
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list tags: status %d", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Tags []struct {
				Name string `json:"name"`
			} `json:"tags"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&env)
	names := make([]string, len(env.Data.Tags))
	for i, t := range env.Data.Tags {
		names[i] = t.Name
	}
	return names
}

func mustListMedia(t *testing.T, c *http.Client) []string {
	t.Helper()
	resp, err := c.Get(mustBaseURL(t, c) + "/api/v1/media?limit=100")
	if err != nil {
		t.Fatalf("list media: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list media: status %d", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Media []struct {
				ID string `json:"id"`
			} `json:"media"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&env)
	ids := make([]string, len(env.Data.Media))
	for i, m := range env.Data.Media {
		ids[i] = m.ID
	}
	return ids
}

func postJSON(c *http.Client, url string, body io.Reader) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", "application/json")
	return c.Do(req)
}

func getStatus(t *testing.T, c *http.Client, path string) int {
	t.Helper()
	resp, err := c.Get(mustBaseURL(t, c) + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func deleteStatus(t *testing.T, c *http.Client, path string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, mustBaseURL(t, c)+path, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// mustBaseURL is a placeholder so the test helpers can take a *http.Client
// without threading the env URL through every call. The two tenant
// clients in this test share the same baseURL — see setupMultiTenantEnv.
// We stash it in a package var set at the top of the test.
var testBaseURL string

func mustBaseURL(t *testing.T, _ *http.Client) string {
	t.Helper()
	if testBaseURL == "" {
		t.Fatal("testBaseURL not set; call setupMultiTenantEnv first")
	}
	return testBaseURL
}
