package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

var (
	remoteCacheHost = flag.String("remote-cache-host", "", "Remote cache server host")
	tlsCert         = flag.String("remote-tls-cert", "", "Remote cache server TLS certificate")
	tlsKey          = flag.String("remote-tls-key", "", "Remote cache server TLS key")
)

func TestClientIntegration(t *testing.T) {
	if *remoteCacheHost == "" {
		t.Skip("remote-cache-host is not set, skipping the integration test")
	}

	cc, err := DialGrpc(*remoteCacheHost, *tlsCert, *tlsKey)
	assert.NilError(t, err)

	t.Cleanup(func() { _ = cc.Close() })

	cl := NewClient(cc)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Check capabilities
	err = cl.CheckCapabilities(ctx)
	assert.NilError(t, err)

	t.Run("metadata", func(t *testing.T) {
		metadata := Metadata{
			"key1": "value1",
			"key2": "value2",
		}
		downloadAndUpload(ctx, t, cl, metadata)
	})

	t.Run("nil metadata", func(t *testing.T) {
		downloadAndUpload(ctx, t, cl, nil)
	})
}

func TestInmemoryClient(t *testing.T) {
	cl := NewInMemoryClient()
	ctx := context.Background()

	err := cl.CheckCapabilities(ctx)
	assert.NilError(t, err)

	downloadAndUpload(ctx, t, cl, nil)
}

func downloadAndUpload(ctx context.Context, t *testing.T, cl Interface, metadata Metadata) {
	// Generate a random key
	randombytes := make([]byte, 16)
	_, err := rand.Read(randombytes)
	assert.NilError(t, err)

	key := fmt.Sprintf("tbc_test_%x", randombytes)

	t.Logf("random key = %s", key)

	// The key must not be present
	ok, err := cl.FindFile(ctx, key)
	assert.NilError(t, err)
	assert.Equal(t, ok, false)

	var (
		tempDir           = t.TempDir()
		filePath          = filepath.Join(tempDir, "ul_data.dat")
		downloadedContent bytes.Buffer
	)

	// Attempt to download the file must fail
	_, err = cl.DownloadFile(ctx, key, &downloadedContent)
	assert.ErrorContains(t, err, "code = NotFound")
	assert.Equal(t, downloadedContent.Len(), 0)

	// Create a file with 4K random bytes
	randomContent := make([]byte, 4096)
	_, err = rand.Read(randomContent)
	assert.NilError(t, err)

	err = os.WriteFile(filePath, randomContent, 0644)
	assert.NilError(t, err)

	// Upload the file
	err = cl.UploadFile(ctx, key, filePath, metadata)
	assert.NilError(t, err)

	// Now the key must be present
	ok, err = cl.FindFile(ctx, key)
	assert.NilError(t, err)
	assert.Equal(t, ok, true)

	// Download the file
	md, err := cl.DownloadFile(ctx, key, &downloadedContent)
	assert.NilError(t, err)

	// Check if it's the same
	assert.DeepEqual(t, downloadedContent.Bytes(), randomContent)
	assert.DeepEqual(t, md, metadata)
}
