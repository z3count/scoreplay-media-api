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
	"testing"
	"time"

	minioclient "github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/scoreplay/media-api/internal/adapter/http/handler"
	pgadapter "github.com/scoreplay/media-api/internal/adapter/postgres"
	appserver "github.com/scoreplay/media-api/internal/adapter/server"
	s3storage "github.com/scoreplay/media-api/internal/adapter/storage/s3"
	"github.com/scoreplay/media-api/internal/service"
)

// TestE2E_S3Backend validates the full media lifecycle using MinIO as an
// S3-compatible storage backend. This proves the S3 adapter is a working
// drop-in replacement for local storage via the port.FileStorage interface.
func TestE2E_S3Backend(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// --- Step 1: Start MinIO container ---
	minioContainer, err := tcminio.Run(ctx, "minio/minio:RELEASE.2024-01-16T16-07-38Z",
		tcminio.WithUsername("minioadmin"),
		tcminio.WithPassword("minioadmin"),
	)
	if err != nil {
		t.Fatalf("start minio container: %v", err)
	}
	t.Cleanup(func() { minioContainer.Terminate(context.Background()) })

	minioEndpoint, err := minioContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("get minio endpoint: %v", err)
	}

	// --- Step 2: Create the test bucket using minio-go client ---
	const testBucket = "test-media"
	mc, err := minioclient.New(minioEndpoint, &minioclient.Options{
		Creds:  miniocreds.NewStaticV4("minioadmin", "minioadmin", ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("create minio client: %v", err)
	}
	if err := mc.MakeBucket(ctx, testBucket, minioclient.MakeBucketOptions{}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// --- Step 3: Start PostgreSQL (reuse existing setup for DB + migrations) ---
	pgEnv := setupTestEnv(t)

	// --- Step 4: Initialize S3 storage adapter pointing to MinIO ---
	s3Store, err := s3storage.NewStorage(ctx, s3storage.Config{
		Bucket:    testBucket,
		Region:    "us-east-1",
		Endpoint:  "http://" + minioEndpoint,
		AccessKey: "minioadmin",
		SecretKey: "minioadmin",
	})
	if err != nil {
		t.Fatalf("init s3 storage: %v", err)
	}

	// --- Step 5: Wire application with S3 storage instead of local ---
	tagRepo := pgadapter.NewTagRepo(pgEnv.db)
	mediaRepo := pgadapter.NewMediaRepo(pgEnv.db)
	tagSvc := service.NewTagService(tagRepo)
	mediaSvc := service.NewMediaService(mediaRepo, tagRepo, s3Store, "http://localhost:0", logger)
	tagHandler := handler.NewTagHandler(tagSvc, logger)
	mediaHandler := handler.NewMediaHandler(mediaSvc, logger, 10*1024*1024)
	healthHandler := handler.NewHealthHandler(pgEnv.db)
	idempotencyStore := pgadapter.NewIdempotencyStore(pgEnv.db)
	router := appserver.NewRouter(healthHandler, tagHandler, mediaHandler, t.TempDir(), testAPIKey, nil, 30*time.Second, 100, 200, idempotencyStore, logger)

	srv := &http.Server{Handler: router}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	baseURL := fmt.Sprintf("http://%s", ln.Addr().String())
	client := &http.Client{
		Transport: &authTransport{apiKey: testAPIKey, base: http.DefaultTransport},
	}

	// ===== Test 1: Create a tag =====
	tagBody := `{"name": "S3 Test Tag"}`
	tagResp, err := client.Post(baseURL+"/api/v1/tags", "application/json",
		bytes.NewReader([]byte(tagBody)))
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	defer tagResp.Body.Close()
	if tagResp.StatusCode != http.StatusCreated && tagResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(tagResp.Body)
		t.Fatalf("create tag: expected 200/201, got %d: %s", tagResp.StatusCode, body)
	}

	var tagEnvelope struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.NewDecoder(tagResp.Body).Decode(&tagEnvelope)
	tagID := tagEnvelope.Data.ID

	// ===== Test 2: Upload a media file (stored in MinIO) =====
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("name", "S3 Upload Test")
	w.WriteField("tags", tagID)
	part, _ := w.CreateFormFile("file", "photo.jpg")
	part.Write(generateJPEG(t))
	w.Close()

	mediaResp, err := client.Post(baseURL+"/api/v1/media", w.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("upload media: %v", err)
	}
	defer mediaResp.Body.Close()
	if mediaResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(mediaResp.Body)
		t.Fatalf("upload media: expected 201, got %d: %s", mediaResp.StatusCode, body)
	}

	var mediaEnvelope struct {
		Data struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			FileURL string `json:"fileUrl"`
			Tags    []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"tags"`
		} `json:"data"`
	}
	json.NewDecoder(mediaResp.Body).Decode(&mediaEnvelope)

	if mediaEnvelope.Data.Name != "S3 Upload Test" {
		t.Errorf("expected name 'S3 Upload Test', got %q", mediaEnvelope.Data.Name)
	}
	if len(mediaEnvelope.Data.Tags) != 1 {
		t.Errorf("expected 1 tag, got %d", len(mediaEnvelope.Data.Tags))
	}
	if mediaEnvelope.Data.FileURL == "" {
		t.Error("expected non-empty file URL from S3 storage")
	}

	// ===== Test 3: Retrieve the media by ID =====
	getResp, err := client.Get(baseURL + "/api/v1/media/" + mediaEnvelope.Data.ID)
	if err != nil {
		t.Fatalf("get media: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get media: expected 200, got %d", getResp.StatusCode)
	}

	// ===== Test 4: Verify the file exists in MinIO bucket =====
	time.Sleep(200 * time.Millisecond) // allow MinIO consistency

	objects := mc.ListObjects(ctx, testBucket, minioclient.ListObjectsOptions{Recursive: true})
	count := 0
	for obj := range objects {
		if obj.Err != nil {
			t.Fatalf("list objects: %v", obj.Err)
		}
		count++
		t.Logf("S3 object: key=%s size=%d", obj.Key, obj.Size)
	}
	if count != 1 {
		t.Fatalf("expected 1 object in S3 bucket, got %d", count)
	}

	t.Log("S3 backend E2E passed: tag created, media uploaded to MinIO, retrieved, object verified")

	// ===== Test 5: Presigned URL grants short-lived read access =====
	// Find the object we just uploaded. ListObjects streams to a channel;
	// we only need the first entry, so pull one without a loop (avoids
	// staticcheck SA4004 "loop unconditionally terminated").
	obj, ok := <-mc.ListObjects(ctx, testBucket, minioclient.ListObjectsOptions{Recursive: true})
	if !ok {
		t.Fatal("no object found in bucket for presign test")
	}
	if obj.Err != nil {
		t.Fatalf("list objects for presign: %v", obj.Err)
	}
	key := obj.Key

	signed, err := s3Store.URLWithExpiry(ctx, "", key, 1*time.Minute)
	if err != nil {
		t.Fatalf("URLWithExpiry: %v", err)
	}
	if signed == "" {
		t.Fatal("presigned URL is empty")
	}

	// The presigned URL embeds AWS SigV4 query params. SigV4 is the AWS standard.
	for _, want := range []string{"X-Amz-Signature", "X-Amz-Expires", "X-Amz-Credential"} {
		if !bytes.Contains([]byte(signed), []byte(want)) {
			t.Errorf("presigned URL missing expected query param %q\nurl=%s", want, signed)
		}
	}

	// And it must actually authorize a GET against MinIO (the bucket is
	// otherwise unauthenticated for the test client, but we go through the
	// presigned URL only).
	resp, err := http.Get(signed)
	if err != nil {
		t.Fatalf("GET presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET: expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("presigned GET returned empty body")
	}
}
