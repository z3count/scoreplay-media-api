package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// ==========================================================================
// Content Type Detection Tests
// ==========================================================================

// TestE2E_MediaContentTypeJPEG verifies that a real JPEG file is classified as "image".
func TestE2E_MediaContentTypeJPEG(t *testing.T) {
	env := setupTestEnv(t)

	jpegData := generateJPEG(t)
	media := createMediaWithBytes(t, env, "JPEG photo", nil, "photo.jpg", jpegData)

	if media.Type != "image" {
		t.Fatalf("expected type 'image', got %q", media.Type)
	}
	if media.FileSize != int64(len(jpegData)) {
		t.Fatalf("expected fileSize %d, got %d", len(jpegData), media.FileSize)
	}
}

// TestE2E_MediaContentTypePNG verifies that a real PNG file is classified as "image".
func TestE2E_MediaContentTypePNG(t *testing.T) {
	env := setupTestEnv(t)

	pngData := generatePNG(t)
	media := createMediaWithBytes(t, env, "PNG photo", nil, "photo.png", pngData)

	if media.Type != "image" {
		t.Fatalf("expected type 'image', got %q", media.Type)
	}
}

// TestE2E_MediaContentTypeOctetStream verifies that a file with unknown binary
// content but a known video extension is accepted as "video".
func TestE2E_MediaContentTypeVideoExtFallback(t *testing.T) {
	env := setupTestEnv(t)

	// Binary data that http.DetectContentType will classify as application/octet-stream.
	data := make([]byte, 512)
	data[0] = 0x00
	data[1] = 0x01
	data[2] = 0x02

	media := createMediaWithBytes(t, env, "Video fallback", nil, "clip.mp4", data)
	if media.Type != "video" {
		t.Fatalf("expected type 'video' via extension fallback, got %q", media.Type)
	}
}

// TestE2E_MediaUnsupportedType verifies that an unsupported file type is rejected.
func TestE2E_MediaUnsupportedType(t *testing.T) {
	env := setupTestEnv(t)

	// Plain text content → text/plain, which is not allowed.
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("name", "Bad file")
	part, _ := w.CreateFormFile("file", "readme.txt")
	part.Write([]byte("This is plain text content that will be detected as text/plain"))
	w.Close()

	resp := doPost(t, env, env.baseURL+"/api/v1/media", w.FormDataContentType(), &buf)
	expectStatus(t, resp, http.StatusUnsupportedMediaType)

	// Verify error format.
	var envelope apiResponse
	decodeBody(t, resp, &envelope)
	if envelope.Error == nil {
		t.Fatal("expected error in response")
	}
	if code, _ := envelope.Error["code"].(string); code != "UNSUPPORTED_MEDIA_TYPE" {
		t.Fatalf("expected error code UNSUPPORTED_MEDIA_TYPE, got %q", code)
	}
}

// ==========================================================================
// Error Response Format Tests
// ==========================================================================

// TestE2E_ErrorResponseFormat validates that all error responses follow the
// standard envelope: {"error": {"code": "...", "message": "..."}}.
func TestE2E_ErrorResponseFormat(t *testing.T) {
	env := setupTestEnv(t)

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		ct         string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "tag empty name",
			method:     "POST",
			path:       "/api/v1/tags",
			body:       `{"name":""}`,
			ct:         "application/json",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
		{
			name:       "tag invalid JSON",
			method:     "POST",
			path:       "/api/v1/tags",
			body:       `{bad`,
			ct:         "application/json",
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_JSON",
		},
		{
			name:       "media not found",
			method:     "GET",
			path:       "/api/v1/media/00000000-0000-0000-0000-000000000000",
			wantStatus: http.StatusNotFound,
			wantCode:   "NOT_FOUND",
		},
		{
			name:       "media invalid UUID",
			method:     "GET",
			path:       "/api/v1/media/xyz",
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_ERROR",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			var err error
			if tc.method == "POST" {
				resp = doPost(t, env, env.baseURL+tc.path, tc.ct, strings.NewReader(tc.body))
			} else {
				resp, err = env.client.Get(env.baseURL + tc.path)
				if err != nil {
					t.Fatal(err)
				}
			}
			expectStatus(t, resp, tc.wantStatus)

			var envelope apiResponse
			decodeBody(t, resp, &envelope)

			if envelope.Error == nil {
				t.Fatal("expected error object in response")
			}
			code, _ := envelope.Error["code"].(string)
			if code != tc.wantCode {
				t.Fatalf("expected error code %q, got %q", tc.wantCode, code)
			}
			msg, _ := envelope.Error["message"].(string)
			if msg == "" {
				t.Fatal("expected non-empty error message")
			}
		})
	}
}

// ==========================================================================
// Response Header Tests
// ==========================================================================

// TestE2E_ResponseHeaders verifies that API responses include correct headers.
func TestE2E_ResponseHeaders(t *testing.T) {
	env := setupTestEnv(t)

	resp, err := env.client.Get(env.baseURL + "/api/v1/tags")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected Content-Type application/json, got %q", ct)
	}

	// Verify security headers are present.
	secHeaders := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
	}
	for header, expected := range secHeaders {
		if got := resp.Header.Get(header); got != expected {
			t.Errorf("expected %s: %q, got %q", header, expected, got)
		}
	}
}

// ==========================================================================
// Tag Ordering Tests
// ==========================================================================

// TestE2E_TagListOrdering verifies tags are returned sorted alphabetically by name.
func TestE2E_TagListOrdering(t *testing.T) {
	env := setupTestEnv(t)

	// Create tags in non-alphabetical order.
	names := []string{"Zidane", "Mbappé", "Ansu Fati", "Bellingham"}
	for _, n := range names {
		createTag(t, env, n)
	}

	data := listTags(t, env, 50, "")
	if len(data.Tags) != 4 {
		t.Fatalf("expected 4 tags, got %d", len(data.Tags))
	}

	// Verify alphabetical order.
	for i := 1; i < len(data.Tags); i++ {
		if data.Tags[i-1].Name >= data.Tags[i].Name {
			t.Fatalf("tags not sorted: %q >= %q", data.Tags[i-1].Name, data.Tags[i].Name)
		}
	}
}

// ==========================================================================
// Shared Tags Tests
// ==========================================================================

// TestE2E_MultipleMediaShareTags verifies multiple media items can share the
// same tags, and each media correctly returns its own tag associations.
func TestE2E_MultipleMediaShareTags(t *testing.T) {
	env := setupTestEnv(t)

	tag := createTag(t, env, "SharedTag")
	jpegData := generateJPEG(t)

	media1 := createMediaWithBytes(t, env, "Photo 1", []string{tag.ID}, "a.jpg", jpegData)
	media2 := createMediaWithBytes(t, env, "Photo 2", []string{tag.ID}, "b.jpg", jpegData)

	if media1.ID == media2.ID {
		t.Fatal("expected different media IDs")
	}
	if len(media1.Tags) != 1 || media1.Tags[0].ID != tag.ID {
		t.Fatal("media1 should have the shared tag")
	}
	if len(media2.Tags) != 1 || media2.Tags[0].ID != tag.ID {
		t.Fatal("media2 should have the shared tag")
	}

	// Verify GET returns correct tags for each.
	fetched1 := getMedia(t, env, media1.ID)
	fetched2 := getMedia(t, env, media2.ID)
	if len(fetched1.Tags) != 1 || fetched1.Tags[0].ID != tag.ID {
		t.Fatal("GET media1 should return shared tag")
	}
	if len(fetched2.Tags) != 1 || fetched2.Tags[0].ID != tag.ID {
		t.Fatal("GET media2 should return shared tag")
	}
}

// ==========================================================================
// Concurrent Tag Creation Tests
// ==========================================================================

// TestE2E_ConcurrentTagCreation verifies that concurrent idempotent tag creation
// is safe and always returns the same tag ID.
func TestE2E_ConcurrentTagCreation(t *testing.T) {
	env := setupTestEnv(t)

	const concurrency = 10
	const tagName = "ConcurrentTestTag"

	ids := make([]string, concurrency)
	errs := make([]error, concurrency)

	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"name":%q}`, tagName)
			resp, err := env.client.Post(
				env.baseURL+"/api/v1/tags",
				"application/json",
				strings.NewReader(body),
			)
			if err != nil {
				errs[idx] = err
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
				errs[idx] = fmt.Errorf("unexpected status %d", resp.StatusCode)
				return
			}

			var envelope apiResponse
			if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
				errs[idx] = err
				return
			}
			var tag tagResponse
			if err := json.Unmarshal(envelope.Data, &tag); err != nil {
				errs[idx] = err
				return
			}
			ids[idx] = tag.ID
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d failed: %v", i, err)
		}
	}

	// All goroutines must return the same tag ID.
	for i := 1; i < concurrency; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("expected same ID across concurrent creates, got %s vs %s", ids[0], ids[i])
		}
	}
}

// ==========================================================================
// Pagination Edge Cases
// ==========================================================================

// TestE2E_PaginationEdgeCases tests cursor pagination with edge values.
func TestE2E_PaginationEdgeCases(t *testing.T) {
	env := setupTestEnv(t)

	// Create 3 tags.
	for i := 0; i < 3; i++ {
		createTag(t, env, fmt.Sprintf("EdgeTag_%02d", i))
	}

	t.Run("limit=0 uses default", func(t *testing.T) {
		// Service defaults limit=0 → 50, so all 3 should fit on one page.
		data := listTags(t, env, 0, "")
		if len(data.Tags) != 3 {
			t.Fatalf("expected 3 tags with default limit, got %d", len(data.Tags))
		}
		if data.Pagination.HasMore {
			t.Fatal("expected hasMore=false when all tags fit in one page")
		}
	})

	t.Run("limit larger than total returns all and hasMore=false", func(t *testing.T) {
		data := listTags(t, env, 100, "")
		if len(data.Tags) != 3 {
			t.Fatalf("expected 3 tags, got %d", len(data.Tags))
		}
		if data.Pagination.HasMore {
			t.Fatal("expected hasMore=false")
		}
		if data.Pagination.NextCursor != "" {
			t.Fatalf("expected empty nextCursor, got %q", data.Pagination.NextCursor)
		}
	})

	t.Run("no query params uses defaults", func(t *testing.T) {
		resp, err := env.client.Get(env.baseURL + "/api/v1/tags")
		if err != nil {
			t.Fatal(err)
		}
		expectStatus(t, resp, http.StatusOK)
	})

	t.Run("invalid cursor returns 400", func(t *testing.T) {
		resp, err := env.client.Get(env.baseURL + "/api/v1/tags?cursor=!!!not-base64!!!")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
	})
}

// ==========================================================================
// File Integrity Test
// ==========================================================================

// TestE2E_FileIntegrity verifies that the uploaded file content is preserved
// exactly after upload and download.
func TestE2E_FileIntegrity(t *testing.T) {
	env := setupTestEnv(t)

	jpegData := generateJPEG(t)
	media := createMediaWithBytes(t, env, "Integrity check", nil, "check.jpg", jpegData)

	// Download and compare — replace dummy base URL with actual test server.
	fileURL := strings.Replace(media.FileURL, "http://localhost:0", env.baseURL, 1)
	resp, err := env.client.Get(fileURL)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusOK)

	downloaded, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if !bytes.Equal(jpegData, downloaded) {
		t.Fatalf("file content mismatch: uploaded %d bytes, downloaded %d bytes", len(jpegData), len(downloaded))
	}
}

// ==========================================================================
// Media Name Validation
// ==========================================================================

// TestE2E_MediaNameValidation tests media name edge cases.
func TestE2E_MediaNameValidation(t *testing.T) {
	env := setupTestEnv(t)

	t.Run("whitespace-only name rejected", func(t *testing.T) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		w.WriteField("name", "   ")
		part, _ := w.CreateFormFile("file", "photo.jpg")
		part.Write(generateJPEG(t))
		w.Close()

		resp := doPost(t, env, env.baseURL+"/api/v1/media", w.FormDataContentType(), &buf)
		expectStatus(t, resp, http.StatusBadRequest)
	})

	t.Run("name is trimmed", func(t *testing.T) {
		jpegData := generateJPEG(t)
		media := createMediaWithBytes(t, env, "  trimmed name  ", nil, "trim.jpg", jpegData)
		if media.Name != "trimmed name" {
			t.Fatalf("expected trimmed name, got %q", media.Name)
		}
	})

	t.Run("name too long rejected", func(t *testing.T) {
		longName := strings.Repeat("x", 256)
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		w.WriteField("name", longName)
		part, _ := w.CreateFormFile("file", "photo.jpg")
		part.Write(generateJPEG(t))
		w.Close()

		resp := doPost(t, env, env.baseURL+"/api/v1/media", w.FormDataContentType(), &buf)
		expectStatus(t, resp, http.StatusBadRequest)
	})
}

// ==========================================================================
// Path Traversal Protection
// ==========================================================================

// TestE2E_PathTraversalProtection verifies that uploading a file with a
// path traversal filename stores only the base name.
func TestE2E_PathTraversalProtection(t *testing.T) {
	env := setupTestEnv(t)

	jpegData := generateJPEG(t)
	media := createMediaWithBytes(t, env, "traversal test", nil, "../../etc/passwd.jpg", jpegData)

	if media.OriginalName != "passwd.jpg" {
		t.Fatalf("expected sanitized originalName 'passwd.jpg', got %q", media.OriginalName)
	}
}

// ==========================================================================
// Multiple Tags on Media
// ==========================================================================

// TestE2E_MediaWithManyTags verifies creating media with several tags.
func TestE2E_MediaWithManyTags(t *testing.T) {
	env := setupTestEnv(t)

	tagIDs := make([]string, 10)
	for i := 0; i < 10; i++ {
		tag := createTag(t, env, fmt.Sprintf("MultiTag_%02d", i))
		tagIDs[i] = tag.ID
	}

	jpegData := generateJPEG(t)
	media := createMediaWithBytes(t, env, "Many tags", tagIDs, "multi.jpg", jpegData)
	if len(media.Tags) != 10 {
		t.Fatalf("expected 10 tags, got %d", len(media.Tags))
	}

	// Verify via GET.
	fetched := getMedia(t, env, media.ID)
	if len(fetched.Tags) != 10 {
		t.Fatalf("GET: expected 10 tags, got %d", len(fetched.Tags))
	}
}

// ==========================================================================
// Tag Idempotency with Whitespace Variations
// ==========================================================================

// TestE2E_TagIdempotencyWhitespace verifies that tags with leading/trailing
// whitespace are trimmed and treated as idempotent with the base name.
func TestE2E_TagIdempotencyWhitespace(t *testing.T) {
	env := setupTestEnv(t)

	tag1 := createTag(t, env, "WhitespaceTag")
	tag2 := createTagExpectStatus(t, env, "  WhitespaceTag  ", http.StatusOK)

	if tag1.ID != tag2.ID {
		t.Fatalf("trimmed name should match existing tag: %s != %s", tag1.ID, tag2.ID)
	}
}

// ==========================================================================
// GET Media Fields Completeness
// ==========================================================================

// TestE2E_GetMediaFieldsComplete verifies all fields are present in GET response.
func TestE2E_GetMediaFieldsComplete(t *testing.T) {
	env := setupTestEnv(t)

	tag := createTag(t, env, "FieldsTest")
	jpegData := generateJPEG(t)
	media := createMediaWithBytes(t, env, "Complete fields", []string{tag.ID}, "complete.jpg", jpegData)

	fetched := getMedia(t, env, media.ID)

	if fetched.ID == "" {
		t.Error("missing id")
	}
	if fetched.Name == "" {
		t.Error("missing name")
	}
	if fetched.Type == "" {
		t.Error("missing type")
	}
	if fetched.OriginalName == "" {
		t.Error("missing originalName")
	}
	if fetched.FileSize == 0 {
		t.Error("missing fileSize")
	}
	if fetched.FileURL == "" {
		t.Error("missing fileUrl")
	}
	if len(fetched.Tags) == 0 {
		t.Error("missing tags")
	}
	// Verify tag fields.
	if fetched.Tags[0].ID == "" {
		t.Error("missing tag id")
	}
	if fetched.Tags[0].Name == "" {
		t.Error("missing tag name")
	}
}

// ==========================================================================
// 404 for Unknown Routes
// ==========================================================================

// TestE2E_UnknownRoutes verifies that unknown API routes return 404/405.
func TestE2E_UnknownRoutes(t *testing.T) {
	env := setupTestEnv(t)

	resp, err := env.client.Get(env.baseURL + "/api/v1/unknown")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 404 or 405, got %d", resp.StatusCode)
	}
}

// ==========================================================================
// Helpers
// ==========================================================================

// generateJPEG creates a minimal valid JPEG image in memory.
func generateJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// generatePNG creates a minimal valid PNG image in memory.
func generatePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{G: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// createMediaWithBytesAndNames uploads media with raw byte content and the
// new repeated tag_names form field. Used by the auto-create tests below.
func createMediaWithBytesAndNames(t *testing.T, env *testEnv, name string, tagIDs []string, tagNames []string, fileName string, content []byte) mediaResponse {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("name", name)
	for _, id := range tagIDs {
		w.WriteField("tags", id)
	}
	for _, n := range tagNames {
		w.WriteField("tag_names", n)
	}
	part, err := w.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	part.Write(content)
	w.Close()

	resp := doPost(t, env, env.baseURL+"/api/v1/media", w.FormDataContentType(), &buf)
	expectStatus(t, resp, http.StatusCreated)

	var envelope apiResponse
	decodeBody(t, resp, &envelope)

	var media mediaResponse
	if err := json.Unmarshal(envelope.Data, &media); err != nil {
		t.Fatalf("unmarshal media: %v", err)
	}
	return media
}

// createMediaWithBytes uploads media with raw byte content.
func createMediaWithBytes(t *testing.T, env *testEnv, name string, tagIDs []string, fileName string, content []byte) mediaResponse {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("name", name)
	for _, id := range tagIDs {
		w.WriteField("tags", id)
	}
	part, err := w.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	part.Write(content)
	w.Close()

	resp := doPost(t, env, env.baseURL+"/api/v1/media", w.FormDataContentType(), &buf)
	expectStatus(t, resp, http.StatusCreated)

	var envelope apiResponse
	decodeBody(t, resp, &envelope)

	var media mediaResponse
	if err := json.Unmarshal(envelope.Data, &media); err != nil {
		t.Fatalf("unmarshal media: %v", err)
	}
	return media
}

// ==========================================================================
// Attach / Detach tags on an existing media item
// ==========================================================================

// TestE2E_DetachTagFromMedia verifies the full unlink flow: upload a media
// with two tags, DELETE one association, GET the media — should now show
// only the remaining tag. The tag that got unlinked must still exist in
// /api/v1/tags (we removed the link, not the tag).
func TestE2E_DetachTagFromMedia(t *testing.T) {
	env := setupTestEnv(t)

	bundes := createTag(t, env, "bundesliga")
	laliga := createTag(t, env, "laliga")
	media := createMediaWithBytes(t, env, "mbappé", []string{bundes.ID, laliga.ID}, "m.jpg", generateJPEG(t))
	if len(media.Tags) != 2 {
		t.Fatalf("setup: expected 2 tags on the upload, got %d", len(media.Tags))
	}

	// Unlink bundesliga from this media.
	req, _ := http.NewRequest(http.MethodDelete,
		env.baseURL+"/api/v1/media/"+media.ID+"/tags/"+bundes.ID, nil)
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusNoContent)

	// Media now carries only laliga.
	fetched := getMedia(t, env, media.ID)
	if len(fetched.Tags) != 1 || fetched.Tags[0].ID != laliga.ID {
		t.Errorf("expected only laliga remaining, got %+v", fetched.Tags)
	}

	// The bundesliga tag itself is still around for other media.
	list := listTags(t, env, 50, "")
	stillThere := false
	for _, tg := range list.Tags {
		if tg.ID == bundes.ID {
			stillThere = true
			break
		}
	}
	if !stillThere {
		t.Error("unlinked tag must still exist in /api/v1/tags")
	}
}

// TestE2E_DetachTag_Idempotent verifies that DELETE on a link that's
// already gone returns 204, not 404. Removes the same association twice.
func TestE2E_DetachTag_Idempotent(t *testing.T) {
	env := setupTestEnv(t)
	tag := createTag(t, env, "redo")
	media := createMediaWithBytes(t, env, "m", []string{tag.ID}, "m.jpg", generateJPEG(t))

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodDelete,
			env.baseURL+"/api/v1/media/"+media.ID+"/tags/"+tag.ID, nil)
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("iteration %d: expected 204, got %d", i, resp.StatusCode)
		}
	}
}

// TestE2E_DetachTag_NotFound covers 404 when the media doesn't exist and
// 400 when either UUID is malformed.
func TestE2E_DetachTag_NotFound(t *testing.T) {
	env := setupTestEnv(t)

	t.Run("media missing", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete,
			env.baseURL+"/api/v1/media/00000000-0000-0000-0000-000000000000/tags/00000000-0000-0000-0000-000000000000", nil)
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		expectStatus(t, resp, http.StatusNotFound)
	})

	t.Run("invalid media uuid", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete,
			env.baseURL+"/api/v1/media/not-a-uuid/tags/00000000-0000-0000-0000-000000000000", nil)
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		expectStatus(t, resp, http.StatusBadRequest)
	})
}

// TestE2E_AttachTagsToMedia verifies the symmetric link flow: upload with
// no tags, then POST /tags with a mix of UUIDs and names (auto-create),
// and observe the refreshed media in the response.
func TestE2E_AttachTagsToMedia(t *testing.T) {
	env := setupTestEnv(t)

	media := createMediaWithBytes(t, env, "untagged", nil, "u.jpg", generateJPEG(t))
	if len(media.Tags) != 0 {
		t.Fatalf("setup: expected no tags, got %d", len(media.Tags))
	}

	existing := createTag(t, env, "pre-existing")

	body := fmt.Sprintf(`{"tags":[%q], "tag_names":["auto-created"]}`, existing.ID)
	req, _ := http.NewRequest(http.MethodPost,
		env.baseURL+"/api/v1/media/"+media.ID+"/tags", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusOK)

	var envelope apiResponse
	decodeBody(t, resp, &envelope)
	var got mediaResponse
	json.Unmarshal(envelope.Data, &got)
	if len(got.Tags) != 2 {
		t.Fatalf("expected 2 tags after attach, got %d (%+v)", len(got.Tags), got.Tags)
	}
}

// TestE2E_AttachTags_Idempotent verifies that re-linking an already-linked
// tag is a no-op (200 + same state).
func TestE2E_AttachTags_Idempotent(t *testing.T) {
	env := setupTestEnv(t)
	tag := createTag(t, env, "stable")
	media := createMediaWithBytes(t, env, "m", []string{tag.ID}, "m.jpg", generateJPEG(t))

	body := fmt.Sprintf(`{"tags":[%q]}`, tag.ID)
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost,
			env.baseURL+"/api/v1/media/"+media.ID+"/tags", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		expectStatus(t, resp, http.StatusOK)

		var envelope apiResponse
		decodeBody(t, resp, &envelope)
		var got mediaResponse
		json.Unmarshal(envelope.Data, &got)
		if len(got.Tags) != 1 {
			t.Errorf("iteration %d: expected still 1 tag, got %d", i, len(got.Tags))
		}
	}
}

// TestE2E_AttachTags_UnknownTagID returns 400 (FK violation mapped to
// ErrValidation) when a tag_id doesn't exist.
func TestE2E_AttachTags_UnknownTagID(t *testing.T) {
	env := setupTestEnv(t)
	media := createMediaWithBytes(t, env, "m", nil, "m.jpg", generateJPEG(t))

	body := `{"tags":["00000000-0000-0000-0000-000000000000"]}`
	req, _ := http.NewRequest(http.MethodPost,
		env.baseURL+"/api/v1/media/"+media.ID+"/tags", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusBadRequest)
}

// TestE2E_AttachTags_MediaNotFound returns 404 when the media id is unknown.
func TestE2E_AttachTags_MediaNotFound(t *testing.T) {
	env := setupTestEnv(t)
	tag := createTag(t, env, "any")

	body := fmt.Sprintf(`{"tags":[%q]}`, tag.ID)
	req, _ := http.NewRequest(http.MethodPost,
		env.baseURL+"/api/v1/media/00000000-0000-0000-0000-000000000000/tags",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusNotFound)
}

// ==========================================================================
// Tag rename + delete (PATCH/DELETE /api/v1/tags/{id})
// ==========================================================================

// TestE2E_RenameTag verifies that PATCH /api/v1/tags/{id} updates the name
// in place, sanitizes the new value, and reflects through to media that
// reference the tag.
func TestE2E_RenameTag(t *testing.T) {
	env := setupTestEnv(t)

	tag := createTag(t, env, "Mbape") // typo on purpose
	media := createMediaWithBytes(t, env, "Goal", []string{tag.ID}, "g.jpg", generateJPEG(t))

	// PATCH with the corrected name (and extra whitespace to confirm
	// sanitization runs).
	body := `{"name":"  Mbappé  "}`
	req, _ := http.NewRequest(http.MethodPatch, env.baseURL+"/api/v1/tags/"+tag.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusOK)

	var envelope apiResponse
	decodeBody(t, resp, &envelope)
	var updated tagResponse
	json.Unmarshal(envelope.Data, &updated)
	if updated.ID != tag.ID {
		t.Errorf("expected same id after rename, got %s want %s", updated.ID, tag.ID)
	}
	if updated.Name != "Mbappé" {
		t.Errorf("expected sanitized name 'Mbappé', got %q", updated.Name)
	}

	// The previously-uploaded media should now report the new tag name.
	fetched := getMedia(t, env, media.ID)
	if len(fetched.Tags) != 1 || fetched.Tags[0].Name != "Mbappé" {
		t.Errorf("media tag should reflect rename, got %+v", fetched.Tags)
	}
}

// TestE2E_RenameTag_Idempotent verifies that renaming a tag to its current
// name is a no-op that returns 200, not 409 (UNIQUE excludes self).
func TestE2E_RenameTag_Idempotent(t *testing.T) {
	env := setupTestEnv(t)
	tag := createTag(t, env, "Static")

	body := `{"name":"Static"}`
	req, _ := http.NewRequest(http.MethodPatch, env.baseURL+"/api/v1/tags/"+tag.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusOK)
}

// TestE2E_RenameTag_Conflict verifies that renaming a tag to a name already
// owned by another tag returns 409.
func TestE2E_RenameTag_Conflict(t *testing.T) {
	env := setupTestEnv(t)
	a := createTag(t, env, "Existing")
	b := createTag(t, env, "Other")

	body := `{"name":"Existing"}`
	req, _ := http.NewRequest(http.MethodPatch, env.baseURL+"/api/v1/tags/"+b.ID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusConflict)
	_ = a // silence linter
}

// TestE2E_RenameTag_NotFound verifies that PATCH on a non-existent tag
// returns 404.
func TestE2E_RenameTag_NotFound(t *testing.T) {
	env := setupTestEnv(t)

	body := `{"name":"any"}`
	req, _ := http.NewRequest(http.MethodPatch,
		env.baseURL+"/api/v1/tags/00000000-0000-0000-0000-000000000000",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusNotFound)
}

// TestE2E_RenameTag_InvalidName covers 400 cases on the new name.
func TestE2E_RenameTag_InvalidName(t *testing.T) {
	env := setupTestEnv(t)
	tag := createTag(t, env, "Valid")

	cases := []struct {
		name string
		body string
	}{
		{"empty name", `{"name":""}`},
		{"whitespace only", `{"name":"   "}`},
		{"bidi override (U+202E)", "{\"name\":\"bad\u202Ename\"}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPatch, env.baseURL+"/api/v1/tags/"+tag.ID, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", testAPIKey)
			resp, err := env.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			expectStatus(t, resp, http.StatusBadRequest)
		})
	}
}

// TestE2E_DeleteTag verifies that DELETE removes the tag and cascades to
// media_tags — media items themselves remain, just lose the association.
func TestE2E_DeleteTag(t *testing.T) {
	env := setupTestEnv(t)
	tag := createTag(t, env, "ToBeDeleted")
	media := createMediaWithBytes(t, env, "M", []string{tag.ID}, "m.jpg", generateJPEG(t))

	// Confirm the media starts with the tag attached.
	pre := getMedia(t, env, media.ID)
	if len(pre.Tags) != 1 || pre.Tags[0].ID != tag.ID {
		t.Fatalf("setup wrong: expected media tagged with %s, got %+v", tag.ID, pre.Tags)
	}

	// DELETE the tag.
	req, _ := http.NewRequest(http.MethodDelete, env.baseURL+"/api/v1/tags/"+tag.ID, nil)
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusNoContent)

	// Media still exists but no longer carries the deleted tag (CASCADE).
	post := getMedia(t, env, media.ID)
	if post.ID != media.ID {
		t.Errorf("media should still exist after tag delete")
	}
	if len(post.Tags) != 0 {
		t.Errorf("expected no tags on media after tag delete, got %+v", post.Tags)
	}

	// And the tag itself is gone from the listing.
	list := listTags(t, env, 50, "")
	for _, tg := range list.Tags {
		if tg.ID == tag.ID {
			t.Errorf("deleted tag still appears in list: %+v", tg)
		}
	}
}

// TestE2E_DeleteTag_NotFound checks 404 on a non-existent id.
func TestE2E_DeleteTag_NotFound(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest(http.MethodDelete,
		env.baseURL+"/api/v1/tags/00000000-0000-0000-0000-000000000000", nil)
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusNotFound)
}

// TestE2E_DeleteTag_InvalidUUID checks 400 on a malformed id.
func TestE2E_DeleteTag_InvalidUUID(t *testing.T) {
	env := setupTestEnv(t)

	req, _ := http.NewRequest(http.MethodDelete, env.baseURL+"/api/v1/tags/not-a-uuid", nil)
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp, http.StatusBadRequest)
}

// ==========================================================================
// tag_names auto-create on upload
// ==========================================================================

// TestE2E_UploadWithTagNamesAutoCreates verifies that a name passed in
// tag_names creates the tag on first sight and attaches it; a second upload
// with the same name attaches the *same* tag (CreateOrGet idempotency).
func TestE2E_UploadWithTagNamesAutoCreates(t *testing.T) {
	env := setupTestEnv(t)
	jpegData := generateJPEG(t)

	media1 := createMediaWithBytesAndNames(t, env, "First", nil, []string{"AutoTag1", "AutoTag2"}, "a.jpg", jpegData)
	if len(media1.Tags) != 2 {
		t.Fatalf("expected 2 tags after auto-create, got %d", len(media1.Tags))
	}

	// Tag IDs should now be discoverable via the tag list.
	listData := listTags(t, env, 50, "")
	names := make(map[string]string)
	for _, tag := range listData.Tags {
		names[tag.Name] = tag.ID
	}
	if names["AutoTag1"] == "" || names["AutoTag2"] == "" {
		t.Fatalf("auto-created tags not in tag list: %+v", listData.Tags)
	}

	// Second upload using the same name must attach the same tag id, not
	// create a duplicate.
	media2 := createMediaWithBytesAndNames(t, env, "Second", nil, []string{"AutoTag1"}, "b.jpg", jpegData)
	if len(media2.Tags) != 1 {
		t.Fatalf("expected 1 tag, got %d", len(media2.Tags))
	}
	if media2.Tags[0].ID != names["AutoTag1"] {
		t.Errorf("expected reuse of existing tag id %s, got %s", names["AutoTag1"], media2.Tags[0].ID)
	}
}

// TestE2E_UploadWithMixedTagsAndNames verifies that a single upload can mix
// pre-resolved tag_ids and tag_names and that duplicates across the two
// fields collapse.
func TestE2E_UploadWithMixedTagsAndNames(t *testing.T) {
	env := setupTestEnv(t)
	jpegData := generateJPEG(t)

	pre := createTag(t, env, "PreCreated")

	// Reference the same tag by UUID *and* by name in the same request —
	// the service should dedupe to a single association.
	media := createMediaWithBytesAndNames(t, env,
		"Mixed", []string{pre.ID}, []string{"PreCreated", "NewByName"},
		"m.jpg", jpegData)

	if len(media.Tags) != 2 {
		t.Fatalf("expected 2 distinct tags (PreCreated + NewByName), got %d: %+v",
			len(media.Tags), media.Tags)
	}
}

// TestE2E_UploadTagNamesNormalized verifies that whitespace-padded and
// NFC-equivalent variants resolve to the same canonical tag.
func TestE2E_UploadTagNamesNormalized(t *testing.T) {
	env := setupTestEnv(t)
	jpegData := generateJPEG(t)

	// First upload creates the tag.
	media1 := createMediaWithBytesAndNames(t, env, "u1", nil, []string{"Mbappé"}, "a.jpg", jpegData)
	id := media1.Tags[0].ID

	// Second upload with whitespace and tabs around the same name should
	// resolve to the same id.
	media2 := createMediaWithBytesAndNames(t, env, "u2", nil, []string{"\t  Mbappé  \t"}, "b.jpg", jpegData)
	if media2.Tags[0].ID != id {
		t.Errorf("whitespace variant should resolve to same tag id; got %s want %s", media2.Tags[0].ID, id)
	}
}

// TestE2E_UploadTagNamesUnsafeRejected verifies that a control character in
// tag_names rejects the upload with 400 — and the upload doesn't happen at all
// (we can't easily assert "no orphan file" from outside, but a clean 400 is
// the user-visible contract).
func TestE2E_UploadTagNamesUnsafeRejected(t *testing.T) {
	env := setupTestEnv(t)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("name", "bad-tag")
	w.WriteField("tag_names", "tag\x00with-null")
	part, _ := w.CreateFormFile("file", "x.jpg")
	part.Write(generateJPEG(t))
	w.Close()

	resp := doPost(t, env, env.baseURL+"/api/v1/media", w.FormDataContentType(), &buf)
	expectStatus(t, resp, http.StatusBadRequest)
}

// ==========================================================================
// Security-related tests
// ==========================================================================

// TestE2E_Securite_AuthRequired verifies that requests without an API key
// are rejected with 401, and requests with a wrong key are also rejected.
func TestE2E_Security_AuthRequired(t *testing.T) {
	env := setupTestEnv(t)
	noAuthClient := &http.Client{} // No API key header

	t.Run("no key returns 401", func(t *testing.T) {
		resp, err := noAuthClient.Get(env.baseURL + "/api/v1/tags")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		expectStatus(t, resp, http.StatusUnauthorized)
	})

	t.Run("wrong key returns 401", func(t *testing.T) {
		req, _ := http.NewRequest("GET", env.baseURL+"/api/v1/tags", nil)
		req.Header.Set("X-API-Key", "wrong-key")
		resp, err := noAuthClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		expectStatus(t, resp, http.StatusUnauthorized)
	})

	t.Run("correct key via X-API-Key works", func(t *testing.T) {
		req, _ := http.NewRequest("GET", env.baseURL+"/api/v1/tags", nil)
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := noAuthClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		expectStatus(t, resp, http.StatusOK)
	})

	t.Run("correct key via Bearer works", func(t *testing.T) {
		req, _ := http.NewRequest("GET", env.baseURL+"/api/v1/tags", nil)
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		resp, err := noAuthClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		expectStatus(t, resp, http.StatusOK)
	})

	t.Run("uploads bypass auth", func(t *testing.T) {
		// /uploads/ should not require auth (static files).
		resp, err := noAuthClient.Get(env.baseURL + "/uploads/nonexistent")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		// 404 is expected (file doesn't exist), but NOT 401.
		if resp.StatusCode == http.StatusUnauthorized {
			t.Fatal("uploads should not require authentication")
		}
	})
}

// TestE2E_Security_CORSPolicy verifies that cross-origin requests from
// non-allowed origins do NOT receive CORS headers.
func TestE2E_Security_CORSPolicy(t *testing.T) {
	env := setupTestEnv(t)

	// Request with an Origin header from an unknown origin.
	req, _ := http.NewRequest("GET", env.baseURL+"/api/v1/tags", nil)
	req.Header.Set("X-API-Key", testAPIKey)
	req.Header.Set("Origin", "https://evil.com")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// No Access-Control-Allow-Origin should be set for unknown origins.
	if acao := resp.Header.Get("Access-Control-Allow-Origin"); acao != "" {
		t.Fatalf("expected no CORS header for unknown origin, got %q", acao)
	}
}

// TestE2E_Security_NoDirectoryListing verifies that browsing /uploads/ returns
// 404 instead of a directory listing.
func TestE2E_Security_NoDirectoryListing(t *testing.T) {
	env := setupTestEnv(t)

	// First upload a file to create the upload directory tree.
	createMediaWithBytes(t, env, "dir test", nil, "test.jpg", generateJPEG(t))

	// Try to list the uploads root directory.
	resp, err := http.Get(env.baseURL + "/uploads/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for directory listing, got %d", resp.StatusCode)
	}
}

// TestE2E_Security_FileContentDisposition verifies that downloaded files have
// Content-Disposition: attachment to prevent inline execution.
func TestE2E_Security_FileContentDisposition(t *testing.T) {
	env := setupTestEnv(t)

	jpegData := generateJPEG(t)
	media := createMediaWithBytes(t, env, "disposition test", nil, "photo.jpg", jpegData)

	fileURL := strings.Replace(media.FileURL, "http://localhost:0", env.baseURL, 1)
	resp, err := env.client.Get(fileURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	cd := resp.Header.Get("Content-Disposition")
	if cd != "attachment" {
		t.Fatalf("expected Content-Disposition: attachment, got %q", cd)
	}
	xcto := resp.Header.Get("X-Content-Type-Options")
	if xcto != "nosniff" {
		t.Fatalf("expected X-Content-Type-Options: nosniff, got %q", xcto)
	}
}

// ==========================================================================
// Health Check Tests
// ==========================================================================

// TestE2E_HealthLiveness verifies that GET /healthz returns 200 OK with
// {"status":"ok"}, proving the process is alive.
func TestE2E_HealthLiveness(t *testing.T) {
	env := setupTestEnv(t)

	// Use a plain client WITHOUT API key — health endpoints must bypass auth.
	plainClient := &http.Client{}

	resp, err := plainClient.Get(env.baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: expected 200, got %d", resp.StatusCode)
	}

	// Verify JSON body.
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /healthz body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status 'ok', got %q", body["status"])
	}

	// Verify Content-Type.
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected Content-Type application/json, got %q", ct)
	}
}

// TestE2E_HealthReadiness verifies that GET /readyz returns 200 OK when
// the database is reachable, with {"status":"ok"}.
func TestE2E_HealthReadiness(t *testing.T) {
	env := setupTestEnv(t)

	// Use a plain client WITHOUT API key — health endpoints must bypass auth.
	plainClient := &http.Client{}

	resp, err := plainClient.Get(env.baseURL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz: expected 200, got %d", resp.StatusCode)
	}

	// Verify JSON body.
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /readyz body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status 'ok', got %q", body["status"])
	}
}

// TestE2E_HealthEndpointsBypassAuth verifies that /healthz and /readyz
// are accessible without an API key. This is critical for Kubernetes
// liveness/readiness probes and load balancers.
func TestE2E_HealthEndpointsBypassAuth(t *testing.T) {
	env := setupTestEnv(t)

	// Plain client with no auth header at all.
	plainClient := &http.Client{}

	for _, endpoint := range []string{"/healthz", "/readyz"} {
		t.Run(endpoint, func(t *testing.T) {
			resp, err := plainClient.Get(env.baseURL + endpoint)
			if err != nil {
				t.Fatalf("GET %s: %v", endpoint, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("GET %s without auth: expected 200, got %d: %s",
					endpoint, resp.StatusCode, body)
			}
		})
	}
}

// ==========================================================================
// Media Listing Tests
// ==========================================================================

// TestE2E_ListMedia verifies paginated media listing and tag-based filtering,
// including multi-tag AND intersection.
func TestE2E_ListMedia(t *testing.T) {
	env := setupTestEnv(t)

	// Create two tags.
	tag1 := createTag(t, env, "sports")
	tag2 := createTag(t, env, "music")

	// Fixture for AND-intersection tests:
	//   Media A: [tag1]        → matches tag_id=tag1 only
	//   Media B: [tag1, tag2]  → matches both, and the AND intersection
	//   Media C: [tag2]        → matches tag_id=tag2 only
	jpegData := generateJPEG(t)
	createMediaWithBytes(t, env, "Media A", []string{tag1.ID}, "a.jpg", jpegData)
	mediaB := createMediaWithBytes(t, env, "Media B", []string{tag1.ID, tag2.ID}, "b.jpg", jpegData)
	createMediaWithBytes(t, env, "Media C", []string{tag2.ID}, "c.jpg", jpegData)

	t.Run("list all media", func(t *testing.T) {
		resp, err := env.client.Get(env.baseURL + "/api/v1/media")
		if err != nil {
			t.Fatalf("GET /api/v1/media: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var result mediaListResult
		json.NewDecoder(resp.Body).Decode(&result)

		if len(result.Data.Media) != 3 {
			t.Fatalf("expected 3 media items, got %d", len(result.Data.Media))
		}
		if result.Data.Pagination.HasMore {
			t.Fatal("expected hasMore=false when all media fit in one page")
		}
	})

	t.Run("filter by tag_id", func(t *testing.T) {
		resp, err := env.client.Get(env.baseURL + "/api/v1/media?tag_id=" + tag1.ID)
		if err != nil {
			t.Fatalf("GET /api/v1/media?tag_id: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var result mediaListResult
		json.NewDecoder(resp.Body).Decode(&result)

		if len(result.Data.Media) != 2 {
			t.Fatalf("expected 2 media items for tag1, got %d", len(result.Data.Media))
		}
	})

	t.Run("filter by tag_name", func(t *testing.T) {
		// tag1 is "sports" — 2 media tagged with it.
		resp, err := env.client.Get(env.baseURL + "/api/v1/media?tag_name=" + tag1.Name)
		if err != nil {
			t.Fatalf("GET /api/v1/media?tag_name: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var result mediaListResult
		json.NewDecoder(resp.Body).Decode(&result)

		if len(result.Data.Media) != 2 {
			t.Fatalf("expected 2 media items for tag_name=%s, got %d", tag1.Name, len(result.Data.Media))
		}
	})

	t.Run("tag_name normalization matches stored tags", func(t *testing.T) {
		// Whitespace around the name should be trimmed by SanitizeName and
		// still match the stored "sports" tag.
		resp, err := env.client.Get(env.baseURL + "/api/v1/media?tag_name=" + url.QueryEscape("  sports  "))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var result mediaListResult
		json.NewDecoder(resp.Body).Decode(&result)
		if len(result.Data.Media) != 2 {
			t.Fatalf("expected 2 media items for trimmed tag_name, got %d", len(result.Data.Media))
		}
	})

	t.Run("unknown tag_name returns empty page", func(t *testing.T) {
		resp, err := env.client.Get(env.baseURL + "/api/v1/media?tag_name=does-not-exist")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 (empty page), got %d", resp.StatusCode)
		}

		var result mediaListResult
		json.NewDecoder(resp.Body).Decode(&result)
		if len(result.Data.Media) != 0 {
			t.Fatalf("expected 0 media items for unknown tag_name, got %d", len(result.Data.Media))
		}
		if result.Data.Pagination.HasMore {
			t.Fatal("expected hasMore=false for empty page")
		}
	})

	t.Run("tag_id and tag_name together rejected", func(t *testing.T) {
		resp, err := env.client.Get(env.baseURL + "/api/v1/media?tag_id=" + tag1.ID + "&tag_name=" + tag2.Name)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 when both filters set, got %d", resp.StatusCode)
		}
	})

	t.Run("multi tag_id AND intersection", func(t *testing.T) {
		// tag1=sports AND tag2=music → only Media B has both.
		resp, err := env.client.Get(env.baseURL + "/api/v1/media?tag_id=" + tag1.ID + "&tag_id=" + tag2.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var result mediaListResult
		json.NewDecoder(resp.Body).Decode(&result)
		if len(result.Data.Media) != 1 {
			t.Fatalf("expected 1 media (intersection), got %d", len(result.Data.Media))
		}
		var got struct{ ID string `json:"id"` }
		json.Unmarshal(result.Data.Media[0], &got)
		if got.ID != mediaB.ID {
			t.Errorf("expected Media B (id=%s), got id=%s", mediaB.ID, got.ID)
		}
	})

	t.Run("multi tag_name AND intersection", func(t *testing.T) {
		resp, err := env.client.Get(env.baseURL + "/api/v1/media?tag_name=" + tag1.Name + "&tag_name=" + tag2.Name)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var result mediaListResult
		json.NewDecoder(resp.Body).Decode(&result)
		if len(result.Data.Media) != 1 {
			t.Fatalf("expected 1 media (intersection by name), got %d", len(result.Data.Media))
		}
	})

	t.Run("duplicate tag_name values are deduped", func(t *testing.T) {
		// Sending the same name 3 times should not require media to be tagged
		// 3× — the service dedupes before HAVING COUNT, so this matches the
		// same set as a single ?tag_name=sports.
		resp, err := env.client.Get(env.baseURL +
			"/api/v1/media?tag_name=" + tag1.Name +
			"&tag_name=" + tag1.Name +
			"&tag_name=" + tag1.Name)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var result mediaListResult
		json.NewDecoder(resp.Body).Decode(&result)
		if len(result.Data.Media) != 2 {
			t.Fatalf("expected 2 media for deduped tag_name, got %d", len(result.Data.Media))
		}
	})

	t.Run("multi-tag with no common media returns empty", func(t *testing.T) {
		// Create a third tag nothing is tagged with; AND with tag1 yields no media.
		extra := createTag(t, env, "no-media-tag")
		resp, err := env.client.Get(env.baseURL + "/api/v1/media?tag_id=" + tag1.ID + "&tag_id=" + extra.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var result mediaListResult
		json.NewDecoder(resp.Body).Decode(&result)
		if len(result.Data.Media) != 0 {
			t.Fatalf("expected 0 media when intersection is empty, got %d", len(result.Data.Media))
		}
	})

	t.Run("cursor pagination walks all pages", func(t *testing.T) {
		// Page 1: limit=2 should return 2 items + hasMore=true (3 total).
		resp1, err := env.client.Get(env.baseURL + "/api/v1/media?limit=2")
		if err != nil {
			t.Fatal(err)
		}
		defer resp1.Body.Close()

		var page1 mediaListResult
		json.NewDecoder(resp1.Body).Decode(&page1)
		if len(page1.Data.Media) != 2 {
			t.Fatalf("expected 2 items on page 1, got %d", len(page1.Data.Media))
		}
		if !page1.Data.Pagination.HasMore || page1.Data.Pagination.NextCursor == "" {
			t.Fatalf("expected nextCursor on page 1, got hasMore=%v cursor=%q",
				page1.Data.Pagination.HasMore, page1.Data.Pagination.NextCursor)
		}

		// Page 2: feed the cursor back; expect 1 final item.
		resp2, err := env.client.Get(env.baseURL + "/api/v1/media?limit=2&cursor=" + page1.Data.Pagination.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		defer resp2.Body.Close()

		var page2 mediaListResult
		json.NewDecoder(resp2.Body).Decode(&page2)
		if len(page2.Data.Media) != 1 {
			t.Fatalf("expected 1 item on page 2, got %d", len(page2.Data.Media))
		}
		if page2.Data.Pagination.HasMore {
			t.Fatalf("expected hasMore=false on final page")
		}
	})

	t.Run("invalid tag_id returns 400", func(t *testing.T) {
		resp, err := env.client.Get(env.baseURL + "/api/v1/media?tag_id=not-a-uuid")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("invalid cursor returns 400", func(t *testing.T) {
		resp, err := env.client.Get(env.baseURL + "/api/v1/media?cursor=!!!not-base64!!!")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
	})
}

// mediaListResult mirrors the JSON shape of GET /api/v1/media for tests.
type mediaListResult struct {
	Data struct {
		Media      []json.RawMessage `json:"media"`
		Pagination struct {
			Limit      int    `json:"limit"`
			NextCursor string `json:"nextCursor"`
			HasMore    bool   `json:"hasMore"`
		} `json:"pagination"`
	} `json:"data"`
}

// ==========================================================================
// Prometheus Metrics Tests
// ==========================================================================

// TestE2E_MetricsEndpoint verifies that GET /metrics returns Prometheus
// exposition format with expected metric families.
func TestE2E_MetricsEndpoint(t *testing.T) {
	env := setupTestEnv(t)

	// Make a request first so metrics have data.
	env.client.Get(env.baseURL + "/api/v1/tags")

	// Use a plain client — /metrics must bypass auth.
	plainClient := &http.Client{}
	resp, err := plainClient.Get(env.baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	// Verify key metric families are present.
	expectedMetrics := []string{
		"http_requests_total",
		"http_request_duration_seconds",
		"http_requests_in_flight",
	}
	for _, metric := range expectedMetrics {
		if !strings.Contains(bodyStr, metric) {
			t.Errorf("expected /metrics to contain %q", metric)
		}
	}
}

// TestE2E_MetricsBypassAuth verifies that /metrics is accessible without
// an API key, so Prometheus can scrape it.
func TestE2E_MetricsBypassAuth(t *testing.T) {
	env := setupTestEnv(t)

	plainClient := &http.Client{}
	resp, err := plainClient.Get(env.baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /metrics without auth: expected 200, got %d: %s",
			resp.StatusCode, body)
	}
}

// ==========================================================================
// Media Delete Tests
// ==========================================================================

// TestE2E_DeleteMedia verifies media deletion and its side effects.
func TestE2E_DeleteMedia(t *testing.T) {
	env := setupTestEnv(t)

	// Create a tag and a media item.
	tag := createTag(t, env, "delete-test")
	jpegData := generateJPEG(t)
	media := createMediaWithBytes(t, env, "To Delete", []string{tag.ID}, "delete-me.jpg", jpegData)

	t.Run("delete existing media returns 204", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, env.baseURL+"/api/v1/media/"+media.ID, nil)
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("DELETE /api/v1/media/%s: %v", media.ID, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 204, got %d: %s", resp.StatusCode, body)
		}
	})

	t.Run("GET deleted media returns 404", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, env.baseURL+"/api/v1/media/"+media.ID, nil)
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("GET /api/v1/media/%s: %v", media.ID, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 after deletion, got %d", resp.StatusCode)
		}
	})

	t.Run("delete same media again returns 404", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, env.baseURL+"/api/v1/media/"+media.ID, nil)
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("DELETE /api/v1/media/%s: %v", media.ID, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 for already-deleted media, got %d", resp.StatusCode)
		}
	})

	t.Run("delete non-existent media returns 404", func(t *testing.T) {
		fakeID := "00000000-0000-0000-0000-000000000000"
		req, _ := http.NewRequest(http.MethodDelete, env.baseURL+"/api/v1/media/"+fakeID, nil)
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", resp.StatusCode)
		}
	})

	t.Run("delete with invalid UUID returns 400", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, env.baseURL+"/api/v1/media/not-a-uuid", nil)
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("delete reduces list count", func(t *testing.T) {
		// Create 2 media, delete 1, verify count.
		m1 := createMediaWithBytes(t, env, "Keep", []string{tag.ID}, "keep.jpg", jpegData)
		m2 := createMediaWithBytes(t, env, "Remove", []string{tag.ID}, "remove.jpg", jpegData)
		_ = m1

		// Delete m2.
		req, _ := http.NewRequest(http.MethodDelete, env.baseURL+"/api/v1/media/"+m2.ID, nil)
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		// List should show count minus 1.
		listResp, err := env.client.Get(env.baseURL + "/api/v1/media")
		if err != nil {
			t.Fatal(err)
		}
		defer listResp.Body.Close()

		var result mediaListResult
		json.NewDecoder(listResp.Body).Decode(&result)

		// m2 was deleted, so it shouldn't appear.
		for _, raw := range result.Data.Media {
			var item struct{ ID string `json:"id"` }
			json.Unmarshal(raw, &item)
			if item.ID == m2.ID {
				t.Fatal("deleted media should not appear in list")
			}
		}
	})
}

// ==========================================================================
// Request Timeout Tests
// ==========================================================================

// TestE2E_RequestTimeout verifies that the timeout middleware is active
// by checking that requests complete normally within the timeout window.
// We can't easily test the expiry in E2E (it would require a slow handler),
// but we verify that the middleware doesn't interfere with normal requests
// and that health endpoints (which are also behind the middleware) work.
func TestE2E_RequestTimeout(t *testing.T) {
	env := setupTestEnv(t)

	t.Run("normal request completes within timeout", func(t *testing.T) {
		// Create a tag — this exercises the full middleware stack including timeout.
		req, _ := http.NewRequest(http.MethodPost, env.baseURL+"/api/v1/tags", strings.NewReader(`{"name":"timeout-test"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("POST /api/v1/tags: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200/201, got %d: %s", resp.StatusCode, body)
		}
	})

	t.Run("health endpoint works with timeout middleware", func(t *testing.T) {
		resp, err := http.Get(env.baseURL + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("media upload completes within timeout", func(t *testing.T) {
		tag := createTag(t, env, "timeout-upload")
		jpegData := generateJPEG(t)
		media := createMediaWithBytes(t, env, "Timeout Upload", []string{tag.ID}, "timeout.jpg", jpegData)
		if media.ID == "" {
			t.Fatal("expected media to be created successfully")
		}
	})
}

// ==========================================================================
// Input Sanitization Tests
// ==========================================================================

// TestE2E_InputSanitization verifies that the sanitization pipeline works
// end-to-end through the API layer.
func TestE2E_InputSanitization(t *testing.T) {
	env := setupTestEnv(t)

	t.Run("CJK tag names accepted", func(t *testing.T) {
		tag := createTag(t, env, "田中将大")
		if tag.Name != "田中将大" {
			t.Errorf("expected CJK tag name preserved, got %q", tag.Name)
		}
	})

	t.Run("accented tag names accepted", func(t *testing.T) {
		tag := createTag(t, env, "Mbappé")
		if tag.Name != "Mbappé" {
			t.Errorf("expected accented name preserved, got %q", tag.Name)
		}
	})

	t.Run("emoji tag names accepted", func(t *testing.T) {
		tag := createTag(t, env, "⚽ Goal")
		if tag.Name != "⚽ Goal" {
			t.Errorf("expected emoji name preserved, got %q", tag.Name)
		}
	})

	t.Run("control characters rejected", func(t *testing.T) {
		body := `{"name": "hello\u0000world"}`
		req, _ := http.NewRequest(http.MethodPost, env.baseURL+"/api/v1/tags", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 400 for control char, got %d: %s", resp.StatusCode, respBody)
		}
	})

	t.Run("zero-width characters rejected", func(t *testing.T) {
		// U+200B zero-width space embedded in tag name.
		body := `{"name": "Mbapp\u200bé"}`
		req, _ := http.NewRequest(http.MethodPost, env.baseURL+"/api/v1/tags", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 400 for zero-width char, got %d: %s", resp.StatusCode, respBody)
		}
	})

	t.Run("bidi override rejected", func(t *testing.T) {
		body := `{"name": "test\u202Evalue"}`
		req, _ := http.NewRequest(http.MethodPost, env.baseURL+"/api/v1/tags", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", testAPIKey)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			respBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 400 for bidi override, got %d: %s", resp.StatusCode, respBody)
		}
	})

	t.Run("whitespace normalization via API", func(t *testing.T) {
		// Multiple spaces should be collapsed.
		tag := createTag(t, env, "Goal   celebration")
		if tag.Name != "Goal celebration" {
			t.Errorf("expected collapsed spaces, got %q", tag.Name)
		}
	})

	t.Run("media with sanitized name", func(t *testing.T) {
		tag := createTag(t, env, "sanitize-media-test")
		jpegData := generateJPEG(t)
		media := createMediaWithBytes(t, env, "  손흥민  Goal  ", []string{tag.ID}, "test.jpg", jpegData)
		if media.Name != "손흥민 Goal" {
			t.Errorf("expected sanitized media name '손흥민 Goal', got %q", media.Name)
		}
	})
}

// ==========================================================================
// Idempotency Key Tests
// ==========================================================================

// TestE2E_Idempotency verifies the Idempotency-Key mechanism end-to-end
// through the real API with a real PostgreSQL database.
func TestE2E_Idempotency(t *testing.T) {
	env := setupTestEnv(t)

	t.Run("retry with same key returns same media", func(t *testing.T) {
		tag := createTag(t, env, "idem-test")
		jpegData := generateJPEG(t)
		idempotencyKey := "test-key-" + tag.ID

		// Helper to upload with idempotency key.
		upload := func() (*http.Response, mediaResponse) {
			var buf bytes.Buffer
			w := multipart.NewWriter(&buf)
			w.WriteField("name", "Idempotent Upload")
			w.WriteField("tags", tag.ID)
			part, _ := w.CreateFormFile("file", "idem.jpg")
			part.Write(jpegData)
			w.Close()

			req, _ := http.NewRequest(http.MethodPost, env.baseURL+"/api/v1/media", &buf)
			req.Header.Set("Content-Type", w.FormDataContentType())
			req.Header.Set("X-API-Key", testAPIKey)
			req.Header.Set("Idempotency-Key", idempotencyKey)
			resp, err := env.client.Do(req)
			if err != nil {
				t.Fatalf("POST /api/v1/media: %v", err)
			}

			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			var envelope struct {
				Data mediaResponse `json:"data"`
			}
			json.Unmarshal(body, &envelope)
			return resp, envelope.Data
		}

		// First call.
		resp1, media1 := upload()
		if resp1.StatusCode != http.StatusCreated {
			t.Fatalf("first call: expected 201, got %d", resp1.StatusCode)
		}
		if media1.ID == "" {
			t.Fatal("first call: expected media ID")
		}

		// Retry (same key).
		resp2, media2 := upload()
		if resp2.StatusCode != http.StatusCreated {
			t.Fatalf("retry: expected 201, got %d", resp2.StatusCode)
		}

		// Same media ID — no duplicate created.
		if media2.ID != media1.ID {
			t.Errorf("retry should return same media ID: first=%s retry=%s", media1.ID, media2.ID)
		}

		// Retry should have the replayed header.
		if resp2.Header.Get("Idempotency-Replayed") != "true" {
			t.Error("retry should have Idempotency-Replayed: true header")
		}
	})

	t.Run("different keys create different media", func(t *testing.T) {
		tag := createTag(t, env, "idem-diff")
		jpegData := generateJPEG(t)

		uploadWithKey := func(key string) mediaResponse {
			var buf bytes.Buffer
			w := multipart.NewWriter(&buf)
			w.WriteField("name", "Different Key Upload")
			w.WriteField("tags", tag.ID)
			part, _ := w.CreateFormFile("file", "diff.jpg")
			part.Write(jpegData)
			w.Close()

			req, _ := http.NewRequest(http.MethodPost, env.baseURL+"/api/v1/media", &buf)
			req.Header.Set("Content-Type", w.FormDataContentType())
			req.Header.Set("X-API-Key", testAPIKey)
			req.Header.Set("Idempotency-Key", key)
			resp, err := env.client.Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)
			var envelope struct {
				Data mediaResponse `json:"data"`
			}
			json.Unmarshal(body, &envelope)
			return envelope.Data
		}

		media1 := uploadWithKey("diff-key-a")
		media2 := uploadWithKey("diff-key-b")

		if media1.ID == media2.ID {
			t.Error("different keys should create different media items")
		}
	})

	t.Run("no idempotency key creates normally", func(t *testing.T) {
		tag := createTag(t, env, "no-idem")
		jpegData := generateJPEG(t)
		media := createMediaWithBytes(t, env, "No Key Upload", []string{tag.ID}, "nokey.jpg", jpegData)
		if media.ID == "" {
			t.Fatal("expected media to be created normally without key")
		}
	})
}
