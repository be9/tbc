package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/be9/tbc/client"
	"gotest.tools/v3/assert"
)

func TestBadToken(t *testing.T) {
	s := NewServer(client.NewInMemoryClient(), Options{Token: "t0k3n"})
	r := s.CreateHandler()

	req, err := http.NewRequest("POST", "/v8/artifacts/events", nil)
	assert.NilError(t, err)
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	assert.Equal(t, rr.Code, http.StatusForbidden)
}

func TestEvents(t *testing.T) {
	r, _ := createHandler("")

	req, err := http.NewRequest("POST", "/v8/artifacts/events", nil)
	assert.NilError(t, err)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, rr.Code, http.StatusOK)
}

func TestStatus(t *testing.T) {
	r, _ := createHandler("")

	req, err := http.NewRequest("GET", "/v8/artifacts/status", nil)
	assert.NilError(t, err)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, rr.Code, http.StatusOK)
	assert.Equal(t, rr.Body.String(), "{\"status\":\"enabled\"}\n")
}

func TestUpload(t *testing.T) {
	const (
		input  = "valuable content to be cached"
		input2 = "other valuable content"
	)

	cl := client.NewInMemoryClient()
	r, srv := createHandlerForClient("", cl)

	t.Run("basic upload", func(t *testing.T) {
		srv.ResetStatistics()

		req := createBaseUploadRequest(t, "key1", bytes.NewBuffer([]byte(input)))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		assert.Equal(t, rr.Code, http.StatusAccepted)

		cacheData := new(bytes.Buffer)
		md, err := cl.DownloadFile(context.Background(), "key1", cacheData)
		assert.NilError(t, err)
		assert.Equal(t, len(md), 0)

		assert.DeepEqual(t, cacheData.String(), input)
		assert.DeepEqual(t, srv.GetStatistics(), Stats{UploadCount: 1, UploadedBytes: int64(len(input))})
	})

	t.Run("upload with metadata", func(t *testing.T) {
		srv.ResetStatistics()

		const tag = "Tc0BmHvJYMIYJ62/zx87YqO0Flxk+5Ovip25NY825CQ="
		req := createBaseUploadRequest(t, "key2", bytes.NewBuffer([]byte(input)))
		req.Header.Set("X-Artifact-Duration", "42")
		req.Header.Set("X-Artifact-Client-Ci", "TEST")
		req.Header.Set("X-Artifact-Client-Interactive", "1")
		req.Header.Set("X-Artifact-Tag", tag)

		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		assert.Equal(t, rr.Code, http.StatusAccepted)

		cacheData := new(bytes.Buffer)
		md, err := cl.DownloadFile(context.Background(), "key2", cacheData)
		assert.NilError(t, err)
		assert.DeepEqual(t, md, client.Metadata{
			"x-artifact-duration": "42",
			"x-artifact-tag":      tag,
			// other headers should not be recorded
		})
		assert.DeepEqual(t, cacheData.String(), input)
		assert.DeepEqual(t, srv.GetStatistics(), Stats{UploadCount: 1, UploadedBytes: int64(len(input))})
	})

	t.Run("teamId and slug scoping", func(t *testing.T) {
		srv.ResetStatistics()

		const sharedKey = "key3"

		req1 := createBaseUploadRequest(t, sharedKey+"?teamId=tid1&slug=slug1", bytes.NewBuffer([]byte(input)))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req1)

		ok, err := cl.FindFile(context.Background(), "slug1/tid1/"+sharedKey)
		assert.NilError(t, err)
		assert.Equal(t, ok, true)

		req2 := createBaseUploadRequest(t, sharedKey+"?teamId=tid2&slug=slug2", bytes.NewBuffer([]byte(input2)))
		rr2 := httptest.NewRecorder()
		r.ServeHTTP(rr2, req2)

		cacheData := new(bytes.Buffer)
		_, err = cl.DownloadFile(context.Background(), "slug2/tid2/"+sharedKey, cacheData)
		assert.NilError(t, err)

		assert.DeepEqual(t, cacheData.String(), input2)
		assert.DeepEqual(t, srv.GetStatistics(), Stats{UploadCount: 2, UploadedBytes: int64(len(input) + len(input2))})
	})
}

func TestCheck(t *testing.T) {
	cl := client.NewInMemoryClient()
	uploadFile(t, cl, "key", []byte("DATA"), nil)
	uploadFile(t, cl, "slug/teamid/key", []byte("DATA"), nil)

	r, srv := createHandlerForClient("", cl)

	t.Run("key exists", func(t *testing.T) {
		srv.ResetStatistics()

		for _, k := range []string{"key", "key?teamId=teamid&slug=slug"} {
			req := createCheckRequest(t, k)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			assert.Equal(t, rr.Code, http.StatusOK)
		}

		assert.DeepEqual(t, srv.GetStatistics(), Stats{ExistsYesCount: 2})
	})

	t.Run("key does not exist", func(t *testing.T) {
		srv.ResetStatistics()

		for _, k := range []string{"unknown key", "key?teamId=badteamid&slug=slug"} {
			req := createCheckRequest(t, k)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			assert.Equal(t, rr.Code, http.StatusNotFound)
		}

		assert.DeepEqual(t, srv.GetStatistics(), Stats{ExistsNoCount: 2})
	})
}

func TestDownload(t *testing.T) {
	cl := client.NewInMemoryClient()
	randomContent := randomBytes(t, 4096)

	uploadFile(t, cl, "key", randomContent, client.Metadata{
		"x-artifact-duration": "42",
		"x-artifact-tag":      "hmac tag",
	})
	uploadFile(t, cl, "slug/teamid/key", randomContent, nil)

	r, srv := createHandlerForClient("", cl)

	t.Run("successful download", func(t *testing.T) {
		srv.ResetStatistics()
		req := createDownloadRequest(t, "key")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		assert.Equal(t, rr.Code, http.StatusOK)
		assert.Equal(t, bytes.Equal(rr.Body.Bytes(), randomContent), true)
		assert.Equal(t, rr.Header().Get("X-Artifact-Duration"), "42")
		assert.Equal(t, rr.Header().Get("X-Artifact-Tag"), "hmac tag")
		assert.DeepEqual(t, srv.GetStatistics(), Stats{DownloadCount: 1, DownloadedBytes: int64(len(randomContent))})
	})

	t.Run("scoped download", func(t *testing.T) {
		srv.ResetStatistics()
		req := createDownloadRequest(t, "key?teamId=teamid&slug=slug")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		assert.Equal(t, rr.Code, http.StatusOK)
		assert.Equal(t, bytes.Equal(rr.Body.Bytes(), randomContent), true)
		assert.Equal(t, rr.Header().Get("X-Artifact-Duration"), "")
		assert.Equal(t, rr.Header().Get("X-Artifact-Tag"), "")
		assert.DeepEqual(t, srv.GetStatistics(), Stats{DownloadCount: 1, DownloadedBytes: int64(len(randomContent))})
	})

	t.Run("not found", func(t *testing.T) {
		srv.ResetStatistics()
		for _, k := range []string{"unknown key", "key?teamId=badteamid&slug=slug"} {
			req := createDownloadRequest(t, k)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			assert.Equal(t, rr.Code, http.StatusNotFound)
		}
		assert.DeepEqual(t, srv.GetStatistics(), Stats{DownloadNotFoundCount: 2})
	})
}

func TestBidirectional(t *testing.T) {
	var (
		cl      = client.NewInMemoryClient()
		r, srv  = createHandlerForClient("", cl)
		content = randomBytes(t, 64*1024*1024) // 64 MiB
	)
	const (
		tag = "Tc0BmHvJYMIYJ62/zx87YqO0Flxk+5Ovip25NY825CQ="
		key = "12HKQaOmR5t5Uy6vdcQsNIiZgHGB"
	)

	// first upload
	func() {
		req := createBaseUploadRequest(t, key, bytes.NewBuffer(content))
		req.Header.Set("X-Artifact-Duration", "42")
		req.Header.Set("X-Artifact-Client-Ci", "TEST")
		req.Header.Set("X-Artifact-Client-Interactive", "1")
		req.Header.Set("X-Artifact-Tag", tag)

		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		assert.Equal(t, rr.Code, http.StatusAccepted)
	}()

	// ...then download
	req := createDownloadRequest(t, key)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	assert.Equal(t, rr.Code, http.StatusOK)
	assert.Equal(t, bytes.Equal(rr.Body.Bytes(), content), true)
	assert.Equal(t, rr.Header().Get("X-Artifact-Duration"), "42")
	assert.Equal(t, rr.Header().Get("X-Artifact-Tag"), tag)

	assert.DeepEqual(t, srv.GetStatistics(), Stats{
		DownloadCount:   1,
		UploadCount:     1,
		UploadedBytes:   int64(len(content)),
		DownloadedBytes: int64(len(content)),
	})
}

func createHandler(token string) (http.Handler, *Server) {
	return createHandlerForClient(token, client.NewInMemoryClient())
}

func createHandlerForClient(token string, cl client.Interface) (http.Handler, *Server) {
	s := NewServer(cl, Options{Token: token})
	r := s.CreateHandler()
	return r, s
}

func createBaseUploadRequest(t *testing.T, key string, body io.Reader) *http.Request {
	req, err := http.NewRequest("PUT", "/v8/artifacts/"+key, body)
	assert.NilError(t, err)
	return req
}

func createCheckRequest(t *testing.T, key string) *http.Request {
	req, err := http.NewRequest("HEAD", "/v8/artifacts/"+key, nil)
	assert.NilError(t, err)
	return req
}

func createDownloadRequest(t *testing.T, key string) *http.Request {
	req, err := http.NewRequest("GET", "/v8/artifacts/"+key, nil)
	assert.NilError(t, err)
	return req
}

func uploadFile(t *testing.T, cl client.Interface, key string, data []byte, md client.Metadata) {
	filePath := filepath.Join(t.TempDir(), "upload.dat")
	err := os.WriteFile(filePath, data, 0644)
	assert.NilError(t, err)

	err = cl.UploadFile(context.Background(), key, filePath, md)
	assert.NilError(t, err)
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	_, err := rand.Read(b)
	assert.NilError(t, err)
	return b
}
