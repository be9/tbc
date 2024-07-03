package client

import (
	"context"
	"io"
)

// Metadata contains additional keys-values stored with the uploaded file.
type Metadata = map[string]any

// Interface is the remote cache client interface.
type Interface interface {
	CheckCapabilities(ctx context.Context) error
	UploadFile(ctx context.Context, key, filePath string, metadata Metadata) error
	FindFile(ctx context.Context, key string) (bool, error)
	DownloadFile(ctx context.Context, key string, w io.Writer) (Metadata, error)
}
