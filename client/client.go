package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/google/uuid"
	"google.golang.org/api/transport/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
)

// client carries various underlying clients required for Remote Cache operations.
type client struct {
	cap remoteexecution.CapabilitiesClient
	cas remoteexecution.ContentAddressableStorageClient
	ac  remoteexecution.ActionCacheClient
	bs  *bytestream.Client
}

var _ Interface = (*client)(nil)

// NewClient instantiates a client for a remote cache.
func NewClient(cc *grpc.ClientConn) Interface {
	return &client{
		cap: remoteexecution.NewCapabilitiesClient(cc),
		cas: remoteexecution.NewContentAddressableStorageClient(cc),
		ac:  remoteexecution.NewActionCacheClient(cc),
		bs:  bytestream.NewClient(cc),
	}
}

// CheckCapabilities requests capabilities and verifies that they are OK.
func (c *client) CheckCapabilities(ctx context.Context) error {
	capabilities, err := c.cap.GetCapabilities(ctx, &remoteexecution.GetCapabilitiesRequest{})
	if err != nil {
		return fmt.Errorf("GetCapabilities() failed: %w", err)
	}

	cc := capabilities.GetCacheCapabilities()
	if !slices.Contains(cc.GetDigestFunctions(), remoteexecution.DigestFunction_SHA256) {
		return errors.New("SHA256 is not supported by remote cache")
	}

	if !cc.GetActionCacheUpdateCapabilities().GetUpdateEnabled() {
		return errors.New("AC update is not supported by remote cache")
	}

	return nil
}

// blobFileName carries the file name that our action result pretends to have generated.
const blobFileName = "cache_blob"

// UploadFile uploads a file at filePath to the remote cache so that it can be referenced by
// the provided key.
func (c *client) UploadFile(ctx context.Context, key, filePath string, metadata Metadata) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("error opening file: %w", err)
	}
	defer func() { _ = f.Close() }()

	fileDigest, err := c.uploadToCAS(ctx, f)
	if err != nil {
		return err
	}

	acProtos, err := prepareACProtos(key)
	if err != nil {
		return err
	}

	updateResponse, err := c.cas.BatchUpdateBlobs(ctx, &remoteexecution.BatchUpdateBlobsRequest{
		Requests: []*remoteexecution.BatchUpdateBlobsRequest_Request{
			{Digest: acProtos.command.digest, Data: acProtos.command.data},
			{Digest: acProtos.action.digest, Data: acProtos.action.data},
		},
	})
	if err != nil {
		return err
	}

	for _, response := range updateResponse.Responses {
		if response.GetStatus().GetCode() != 0 {
			return fmt.Errorf("BatchUpdateBlobs failed: %s", prototext.Format(updateResponse))
		}
	}

	var eam *remoteexecution.ExecutedActionMetadata
	if len(metadata) > 0 {
		var protoMd []*anypb.Any
		protoMd, err = convertMetadataToProto(metadata)
		if err != nil {
			return err
		}
		eam = &remoteexecution.ExecutedActionMetadata{
			AuxiliaryMetadata: protoMd,
		}
	}

	_, err = c.ac.UpdateActionResult(ctx, &remoteexecution.UpdateActionResultRequest{
		ActionDigest: acProtos.action.digest,
		ActionResult: &remoteexecution.ActionResult{
			OutputFiles: []*remoteexecution.OutputFile{
				{
					Path:   blobFileName,
					Digest: fileDigest,
				},
			},
			ExecutionMetadata: eam,
		},
	})
	return err
}

// uploadToCAS uses the bytestream client to upload the file to CAS.
func (c *client) uploadToCAS(ctx context.Context, f *os.File) (d *remoteexecution.Digest, err error) {
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		err = fmt.Errorf("error hashing file: %w", err)
		return
	}

	fi, err := f.Stat()
	if err != nil {
		err = fmt.Errorf("error getting file info: %w", err)
		return
	}

	d = &remoteexecution.Digest{
		Hash:      fmt.Sprintf("%x", hash.Sum(nil)),
		SizeBytes: fi.Size(),
	}
	_, err = f.Seek(0, 0)
	if err != nil {
		err = fmt.Errorf("error seeking file: %w", err)
		return
	}

	w, err := c.bs.NewWriter(ctx, getUploadResourceName(d))
	if err != nil {
		return
	}
	if _, err = io.Copy(w, f); err != nil {
		err = fmt.Errorf("upload error: %w", err)
		return
	}
	err = w.Close()
	return
}

type acProto struct {
	digest *remoteexecution.Digest
	data   []byte
}

type acProtos struct {
	command, action acProto
}

func prepareACProtos(key string) (result acProtos, err error) {
	commandDigest, commandData, err := prepareProto(&remoteexecution.Command{
		Arguments: []string{
			"tbc fake command",
			"key",
			key,
		},
	})
	if err != nil {
		return
	}

	actionDigest, actionData, err := prepareProto(&remoteexecution.Action{
		CommandDigest: commandDigest,
	})
	if err != nil {
		return
	}

	return acProtos{
		command: acProto{digest: commandDigest, data: commandData},
		action:  acProto{digest: actionDigest, data: actionData},
	}, nil
}

// prepareProto marshals m and generates the digest for it.
func prepareProto(m proto.Message) (digest *remoteexecution.Digest, data []byte, err error) {
	data, err = proto.Marshal(m)
	if err != nil {
		return
	}

	h := sha256.New()
	if _, err = h.Write(data); err != nil {
		return
	}
	digest = &remoteexecution.Digest{
		Hash:      fmt.Sprintf("%x", h.Sum(nil)),
		SizeBytes: int64(len(data)),
	}
	return
}

func convertMetadataToProto(metadata Metadata) (result []*anypb.Any, err error) {
	val, err := structpb.NewStruct(metadata)
	if err != nil {
		return
	}
	payload, err := anypb.New(val)
	if err != nil {
		return
	}

	return []*anypb.Any{payload}, nil
}

func convertMetadataFromProto(in []*anypb.Any) (Metadata, error) {
	if len(in) != 1 {
		return nil, nil
	}

	structMetadata := &structpb.Struct{}
	if err := in[0].UnmarshalTo(structMetadata); err != nil {
		return nil, err
	}
	return structMetadata.AsMap(), nil
}

// FindFile checks if a file was uploaded under given key. Returns true if file exists.
func (c *client) FindFile(ctx context.Context, key string) (bool, error) {
	if _, _, err := c.locateArtifact(ctx, key); err != nil {
		if s, ok := status.FromError(err); ok {
			if s.Code() == codes.NotFound {
				return false, nil
			}
		}
		return false, err
	}

	return true, nil
}

// DownloadFile attempts to download a file from the remote cache identified by key. The file is
// written at filePath.
func (c *client) DownloadFile(ctx context.Context, key string, w io.Writer) (md Metadata, err error) {
	of, md, err := c.locateArtifact(ctx, key)
	if err != nil {
		return
	}

	rdr, err := c.bs.NewReader(ctx, getDownloadResourceName(of.GetDigest()))
	if err != nil {
		return
	}

	defer func() { _ = rdr.Close() }()

	if _, err = io.Copy(w, rdr); err != nil {
		err = fmt.Errorf("fetch error: %w", err)
		return
	}
	return
}

func (c *client) locateArtifact(ctx context.Context, key string) (of *remoteexecution.OutputFile, md Metadata, err error) {
	acProtos, err := prepareACProtos(key)
	if err != nil {
		return
	}
	resp, err := c.ac.GetActionResult(ctx, &remoteexecution.GetActionResultRequest{
		ActionDigest: acProtos.action.digest,
	})
	if err != nil {
		return
	}

	idx := slices.IndexFunc(resp.OutputFiles, func(f *remoteexecution.OutputFile) bool {
		return f.GetPath() == blobFileName
	})
	if idx < 0 {
		err = fmt.Errorf("cache blob not found amount output files")
		return
	}
	md, err = convertMetadataFromProto(resp.GetExecutionMetadata().GetAuxiliaryMetadata())
	of = resp.OutputFiles[idx]
	return
}

func getDownloadResourceName(d *remoteexecution.Digest) string {
	return fmt.Sprintf("blobs/%s/%d", d.GetHash(), d.GetSizeBytes())
}

func getUploadResourceName(d *remoteexecution.Digest) string {
	return fmt.Sprintf("uploads/%s/blobs/%s/%d", uuid.NewString(), d.GetHash(), d.GetSizeBytes())
}
