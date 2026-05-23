package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	// image codecs used by generateJPEG from functional_test.go
	_ "image/jpeg"
)

// apiResponse is a generic envelope for parsing JSON API responses.
type apiResponse struct {
	Data  json.RawMessage        `json:"data"`
	Error map[string]interface{} `json:"error"`
}

// tagResponse matches the tag JSON structure.
type tagResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// mediaResponse matches the media JSON structure.
type mediaResponse struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Type         string        `json:"type"`
	OriginalName string        `json:"originalName"`
	FileSize     int64         `json:"fileSize"`
	FileURL      string        `json:"fileUrl"`
	Tags         []tagResponse `json:"tags"`
}

// listTagsData matches the cursor-paginated tag listing structure.
type listTagsData struct {
	Tags       []tagResponse `json:"tags"`
	Pagination struct {
		Limit      int    `json:"limit"`
		NextCursor string `json:"nextCursor"`
		HasMore    bool   `json:"hasMore"`
	} `json:"pagination"`
}

// ==========================================================================
// Full Flow E2E Tests
// ==========================================================================

// TestE2E_FullFlow exercises the complete API lifecycle:
// 1. Create tags → 2. List tags → 3. Create media with tags → 4. Get media by ID
// 5. Verify file is accessible → 6. Test idempotent tag creation
//
// This is the single most important test: if this passes, the core business
// requirements are met.
func TestE2E_FullFlow(t *testing.T) {
	env := setupTestEnv(t)

	// --- Step 1: Create tags ---
	tag1 := createTag(t, env, "Mbappé")
	if tag1.Name != "Mbappé" {
		t.Fatalf("expected tag name 'Mbappé', got %q", tag1.Name)
	}
	if tag1.ID == "" {
		t.Fatal("expected non-empty tag ID")
	}

	tag2 := createTag(t, env, "Ligue 1")
	if tag2.ID == tag1.ID {
		t.Fatal("expected different IDs for different tags")
	}

	// --- Step 2: Verify idempotent tag creation ---
	tag1Dup := createTagExpectStatus(t, env, "Mbappé", http.StatusOK)
	if tag1Dup.ID != tag1.ID {
		t.Fatalf("idempotent create should return same ID: got %s, want %s", tag1Dup.ID, tag1.ID)
	}

	// --- Step 3: List tags ---
	listData := listTags(t, env, 50, "")
	if len(listData.Tags) != 2 {
		t.Fatalf("expected 2 tags in response, got %d", len(listData.Tags))
	}
	if listData.Pagination.HasMore {
		t.Fatalf("expected hasMore=false for a fully-returned page, got true (nextCursor=%q)", listData.Pagination.NextCursor)
	}

	// --- Step 4: Create media with tags ---
	jpegData := generateJPEG(t)
	media := createMediaWithBytes(t, env, "Goal celebration", []string{tag1.ID, tag2.ID}, "photo.jpg", jpegData)
	if media.ID == "" {
		t.Fatal("expected non-empty media ID")
	}
	if media.Name != "Goal celebration" {
		t.Fatalf("expected media name 'Goal celebration', got %q", media.Name)
	}
	if len(media.Tags) != 2 {
		t.Fatalf("expected 2 tags, got %d", len(media.Tags))
	}
	if media.FileURL == "" {
		t.Fatal("expected non-empty file URL")
	}
	if media.OriginalName != "photo.jpg" {
		t.Fatalf("expected originalName 'photo.jpg', got %q", media.OriginalName)
	}

	// --- Step 5: Get media by ID ---
	fetched := getMedia(t, env, media.ID)
	if fetched.ID != media.ID {
		t.Fatalf("expected media ID %s, got %s", media.ID, fetched.ID)
	}
	if fetched.Name != media.Name {
		t.Fatalf("expected media name %q, got %q", media.Name, fetched.Name)
	}
	if len(fetched.Tags) != 2 {
		t.Fatalf("expected 2 tags on fetched media, got %d", len(fetched.Tags))
	}

	// --- Step 6: Verify the file is downloadable ---
	fileURL := strings.Replace(fetched.FileURL, "http://localhost:0", env.baseURL, 1)
	resp, err := env.client.Get(fileURL)
	if err != nil {
		t.Fatalf("download file: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for file download, got %d", resp.StatusCode)
	}
	downloaded, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(jpegData, downloaded) {
		t.Error("downloaded file content doesn't match uploaded content")
	}
}

// TestE2E_CreateMediaWithoutTags verifies that media can be created without
// any tags (tags are optional per the spec).
func TestE2E_CreateMediaWithoutTags(t *testing.T) {
	env := setupTestEnv(t)

	media := createMediaWithBytes(t, env, "No tags photo", nil, "solo.jpg", generateJPEG(t))
	if len(media.Tags) != 0 {
		t.Fatalf("expected 0 tags, got %d", len(media.Tags))
	}
}

// TestE2E_CreateMediaDuplicateTags verifies that duplicate tag IDs in a single
// request are deduplicated (media is linked to the tag only once).
func TestE2E_CreateMediaDuplicateTags(t *testing.T) {
	env := setupTestEnv(t)

	tag := createTag(t, env, "Duplicate Test")
	media := createMediaWithBytes(t, env, "Dedup test", []string{tag.ID, tag.ID, tag.ID}, "photo.jpg", generateJPEG(t))
	if len(media.Tags) != 1 {
		t.Fatalf("expected 1 tag after dedup, got %d", len(media.Tags))
	}
}

// ==========================================================================
// Error Case Tests
// ==========================================================================

// TestE2E_TagErrors tests all tag-related error cases.
func TestE2E_TagErrors(t *testing.T) {
	env := setupTestEnv(t)

	t.Run("empty name", func(t *testing.T) {
		resp := doPost(t, env, env.baseURL+"/api/v1/tags", "application/json", strings.NewReader(`{"name":""}`))
		expectStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("whitespace only name", func(t *testing.T) {
		resp := doPost(t, env, env.baseURL+"/api/v1/tags", "application/json", strings.NewReader(`{"name":"   "}`))
		expectStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("invalid JSON", func(t *testing.T) {
		resp := doPost(t, env, env.baseURL+"/api/v1/tags", "application/json", strings.NewReader(`not json`))
		expectStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("name too long", func(t *testing.T) {
		longName := strings.Repeat("a", 256)
		body := fmt.Sprintf(`{"name":%q}`, longName)
		resp := doPost(t, env, env.baseURL+"/api/v1/tags", "application/json", strings.NewReader(body))
		expectStatus(t, resp, http.StatusBadRequest)
	})
}

// TestE2E_MediaGetErrors tests error cases for GET /media/{id}.
func TestE2E_MediaGetErrors(t *testing.T) {
	env := setupTestEnv(t)

	t.Run("invalid UUID", func(t *testing.T) {
		resp, err := env.client.Get(env.baseURL + "/api/v1/media/not-a-uuid")
		if err != nil {
			t.Fatal(err)
		}
		expectStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("not found", func(t *testing.T) {
		resp, err := env.client.Get(env.baseURL + "/api/v1/media/00000000-0000-0000-0000-000000000000")
		if err != nil {
			t.Fatal(err)
		}
		expectStatus(t, resp, http.StatusNotFound)
	})
}

// TestE2E_MediaCreateErrors tests error cases for POST /media.
func TestE2E_MediaCreateErrors(t *testing.T) {
	env := setupTestEnv(t)

	t.Run("missing name field", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		part, _ := w.CreateFormFile("file", "photo.jpg")
		part.Write([]byte("data"))
		w.Close()

		resp := doPost(t, env, env.baseURL+"/api/v1/media", w.FormDataContentType(), &buf)
		expectStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("missing file field", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		w.WriteField("name", "Test")
		w.Close()

		resp := doPost(t, env, env.baseURL+"/api/v1/media", w.FormDataContentType(), &buf)
		expectStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("invalid tag UUID", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		w.WriteField("name", "Test")
		w.WriteField("tags", "not-a-uuid")
		part, _ := w.CreateFormFile("file", "photo.jpg")
		part.Write([]byte("data"))
		w.Close()

		resp := doPost(t, env, env.baseURL+"/api/v1/media", w.FormDataContentType(), &buf)
		expectStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("non-existent tag ID", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		w.WriteField("name", "Test")
		w.WriteField("tags", "00000000-0000-0000-0000-000000000000")
		part, _ := w.CreateFormFile("file", "photo.jpg")
		part.Write([]byte("some image data here"))
		w.Close()

		resp := doPost(t, env, env.baseURL+"/api/v1/media", w.FormDataContentType(), &buf)
		if resp.StatusCode == http.StatusCreated {
			t.Error("expected error for non-existent tag, got 201")
		}
	})

	t.Run("not multipart request", func(t *testing.T) {
		resp := doPost(t, env, env.baseURL+"/api/v1/media", "application/json", strings.NewReader(`{}`))
		expectStatus(t, resp, http.StatusBadRequest)
	})
}

// TestE2E_ListTagsPagination walks the cursor through every page and verifies
// that all tags are returned exactly once with no overlap or gaps.
func TestE2E_ListTagsPagination(t *testing.T) {
	env := setupTestEnv(t)

	// Create 5 tags.
	for i := 0; i < 5; i++ {
		createTag(t, env, fmt.Sprintf("PaginationTag_%02d", i))
	}

	// Page 1: first call with empty cursor.
	page1 := listTags(t, env, 2, "")
	if len(page1.Tags) != 2 {
		t.Fatalf("expected 2 tags on page 1, got %d", len(page1.Tags))
	}
	if !page1.Pagination.HasMore || page1.Pagination.NextCursor == "" {
		t.Fatalf("expected more pages after page 1, got hasMore=%v cursor=%q",
			page1.Pagination.HasMore, page1.Pagination.NextCursor)
	}

	// Page 2: pass nextCursor from page 1.
	page2 := listTags(t, env, 2, page1.Pagination.NextCursor)
	if len(page2.Tags) != 2 {
		t.Fatalf("expected 2 tags on page 2, got %d", len(page2.Tags))
	}
	if !page2.Pagination.HasMore || page2.Pagination.NextCursor == "" {
		t.Fatalf("expected more pages after page 2")
	}

	// Page 3: only 1 remaining; this should be the final page.
	page3 := listTags(t, env, 2, page2.Pagination.NextCursor)
	if len(page3.Tags) != 1 {
		t.Fatalf("expected 1 tag on page 3, got %d", len(page3.Tags))
	}
	if page3.Pagination.HasMore {
		t.Fatalf("expected hasMore=false on final page, got cursor=%q", page3.Pagination.NextCursor)
	}

	// Ensure no overlap between pages.
	allIDs := make(map[string]bool)
	for _, tag := range page1.Tags {
		allIDs[tag.ID] = true
	}
	for _, tag := range page2.Tags {
		if allIDs[tag.ID] {
			t.Fatalf("tag %s appears on multiple pages", tag.ID)
		}
		allIDs[tag.ID] = true
	}
	for _, tag := range page3.Tags {
		if allIDs[tag.ID] {
			t.Fatalf("tag %s appears on multiple pages", tag.ID)
		}
	}
	if len(allIDs)+len(page3.Tags) != 5 {
		t.Fatalf("expected 5 unique tags across pages, got %d", len(allIDs)+len(page3.Tags))
	}
}

// TestE2E_ListTagsInvalidCursor verifies that a malformed cursor returns 400.
func TestE2E_ListTagsInvalidCursor(t *testing.T) {
	env := setupTestEnv(t)

	resp, err := env.client.Get(env.baseURL + "/api/v1/tags?cursor=!!!not-base64!!!")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed cursor, got %d", resp.StatusCode)
	}
}

// ==========================================================================
// Helper functions
// ==========================================================================

// createTag creates a tag and expects 201 Created.
func createTag(t *testing.T, env *testEnv, name string) tagResponse {
	t.Helper()
	return createTagExpectStatus(t, env, name, http.StatusCreated)
}

// createTagExpectStatus creates a tag and checks for the expected status code.
func createTagExpectStatus(t *testing.T, env *testEnv, name string, expectedStatus int) tagResponse {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q}`, name)
	resp := doPost(t, env, env.baseURL+"/api/v1/tags", "application/json", strings.NewReader(body))
	expectStatus(t, resp, expectedStatus)

	var envelope apiResponse
	decodeBody(t, resp, &envelope)

	var tag tagResponse
	if err := json.Unmarshal(envelope.Data, &tag); err != nil {
		t.Fatalf("unmarshal tag: %v", err)
	}
	return tag
}

// listTags fetches a page of tags using cursor pagination.
// Pass an empty cursor to fetch the first page.
func listTags(t *testing.T, env *testEnv, limit int, cursor string) listTagsData {
	t.Helper()
	url := fmt.Sprintf("%s/api/v1/tags?limit=%d", env.baseURL, limit)
	if cursor != "" {
		url += "&cursor=" + cursor
	}
	resp, err := env.client.Get(url)
	if err != nil {
		t.Fatalf("GET /tags: %v", err)
	}
	expectStatus(t, resp, http.StatusOK)

	var envelope apiResponse
	decodeBody(t, resp, &envelope)

	var data listTagsData
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	return data
}

// getMedia fetches a media item by ID.
func getMedia(t *testing.T, env *testEnv, id string) mediaResponse {
	t.Helper()
	resp, err := env.client.Get(env.baseURL + "/api/v1/media/" + id)
	if err != nil {
		t.Fatalf("GET /media/%s: %v", id, err)
	}
	expectStatus(t, resp, http.StatusOK)

	var envelope apiResponse
	decodeBody(t, resp, &envelope)

	var media mediaResponse
	if err := json.Unmarshal(envelope.Data, &media); err != nil {
		t.Fatalf("unmarshal media: %v", err)
	}
	return media
}

// doPost performs an HTTP POST using the env's authenticated client.
func doPost(t *testing.T, env *testEnv, url, contentType string, body io.Reader) *http.Response {
	t.Helper()
	resp, err := env.client.Post(url, contentType, body)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// expectStatus asserts the HTTP response status code.
func expectStatus(t *testing.T, resp *http.Response, expected int) {
	t.Helper()
	if resp.StatusCode != expected {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status %d, got %d; body: %s", expected, resp.StatusCode, string(body))
	}
}

// decodeBody reads and decodes the JSON response body.
func decodeBody(t *testing.T, resp *http.Response, v interface{}) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
