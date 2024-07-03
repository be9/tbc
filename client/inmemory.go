package client

import (
	"context"
	"io"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type artifact struct {
	data     []byte
	metadata Metadata
}

type InMemoryClient struct {
	artifacts map[string]artifact
}

var _ Interface = (*InMemoryClient)(nil)

func NewInMemoryClient() *InMemoryClient {
	return &InMemoryClient{
		artifacts: make(map[string]artifact),
	}
}

func (c *InMemoryClient) CheckCapabilities(context.Context) error {
	return nil
}

func (c *InMemoryClient) UploadFile(ctx context.Context, key, filePath string, metadata Metadata) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	c.artifacts[key] = artifact{
		data:     data,
		metadata: metadata,
	}

	return nil
}

func (c *InMemoryClient) FindFile(ctx context.Context, key string) (bool, error) {
	if _, ok := c.artifacts[key]; ok {
		return true, nil
	}
	return false, nil
}

func (c *InMemoryClient) DownloadFile(ctx context.Context, key string, w io.Writer) (Metadata, error) {
	if af, ok := c.artifacts[key]; ok {
		_, err := w.Write(af.data)
		if err != nil {
			return nil, err
		}
		return af.metadata, nil
	}

	return nil, status.Error(codes.NotFound, "artifact not found")
}
