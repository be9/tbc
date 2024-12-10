package client

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/Southclaws/fault"
	"github.com/Southclaws/fault/fctx"
	"github.com/Southclaws/fault/fmsg"
	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
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
		return fault.Wrap(err, fmsg.With("GetCapabilities() failed"), fctx.With(ctx))
	}

	cc := capabilities.GetCacheCapabilities()
	if !slices.Contains(cc.GetDigestFunctions(), remoteexecution.DigestFunction_SHA256) {
		return fault.New("SHA256 is not supported by remote cache", fctx.With(ctx))
	}

	if !cc.GetActionCacheUpdateCapabilities().GetUpdateEnabled() {
		return fault.New("AC update is not supported by remote cache", fctx.With(ctx))
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
		return fault.Wrap(err, fmsg.With("error opening file"), fctx.With(ctx))
	}
	defer func() { _ = f.Close() }()

	fileDigest, err := c.uploadToCAS(ctx, f)
	if err != nil {
		return fault.Wrap(err, fmsg.With("CAS upload failed"), fctx.With(ctx))
	}

	acProtos, err := prepareACProtos(key)
	if err != nil {
		return fault.Wrap(err, fctx.With(ctx))
	}

	updateResponse, err := c.cas.BatchUpdateBlobs(ctx, &remoteexecution.BatchUpdateBlobsRequest{
		Requests: []*remoteexecution.BatchUpdateBlobsRequest_Request{
			{Digest: acProtos.command.digest, Data: acProtos.command.data},
			{Digest: acProtos.action.digest, Data: acProtos.action.data},
		},
	})
	if err != nil {
		return fault.Wrap(err, fmsg.With("BatchUpdateBlobs failed"), fctx.With(ctx))
	}

	for _, response := range updateResponse.Responses {
		if response.GetStatus().GetCode() != 0 {
			return fault.Wrap(err,
				fmsg.With(fmt.Sprintf("BatchUpdateBlobs failed. %s", prototext.Format(updateResponse))),
				fctx.With(ctx))
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
	return fault.Wrap(err, fmsg.With("UpdateActionResult failed"), fctx.With(ctx))
}

// uploadToCAS uses the bytestream client to upload the file to CAS.
func (c *client) uploadToCAS(ctx context.Context, f *os.File) (d *remoteexecution.Digest, err error) {
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		err = fault.Wrap(err, fmsg.With("error hashing file"), fctx.With(ctx))
		return
	}

	fi, err := f.Stat()
	if err != nil {
		err = fault.Wrap(err, fmsg.With("error getting file info"), fctx.With(ctx))
		return
	}

	d = &remoteexecution.Digest{
		Hash:      fmt.Sprintf("%x", hash.Sum(nil)),
		SizeBytes: fi.Size(),
	}
	_, err = f.Seek(0, 0)
	if err != nil {
		err = fault.Wrap(err, fmsg.With("error seeking file"), fctx.With(ctx))
		return
	}

	w, err := c.bs.NewWriter(ctx, getUploadResourceName(d))
	if err != nil {
		err = fault.Wrap(err, fmsg.With("error creating upload writer"), fctx.With(ctx))
		return
	}
	if _, err = io.Copy(w, f); err != nil {
		err = fault.Wrap(err, fmsg.With("upload error"), fctx.With(ctx))
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
		err = fault.Wrap(err, fmsg.With("marshaling failed"))
		return
	}

	h := sha256.New()
	if _, err = h.Write(data); err != nil {
		err = fault.Wrap(err, fmsg.With("hashing failed"))
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
		err = fault.Wrap(err, fmsg.With("NewStruct failed"))
		return
	}
	payload, err := anypb.New(val)
	if err != nil {
		err = fault.Wrap(err, fmsg.With("anypb.New failed"))
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
		return nil, fault.Wrap(err, fmsg.With("unmarshaling failed"))
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
// written to w.
func (c *client) DownloadFile(ctx context.Context, key string, w io.Writer) (md Metadata, err error) {
	of, md, err := c.locateArtifact(ctx, key)
	if err != nil {
		return
	}

	rdr, err := c.bs.NewReader(ctx, getDownloadResourceName(of.GetDigest()))
	if err != nil {
		err = fault.Wrap(err, fmsg.With("NewReader failed"), fctx.With(ctx))
		return
	}

	defer func() { _ = rdr.Close() }()

	if _, err = io.Copy(w, rdr); err != nil {
		err = fault.Wrap(err, fmsg.With("fetching from bytestream client failed"), fctx.With(ctx))
		return
	}
	return
}

func (c *client) locateArtifact(ctx context.Context, key string) (of *remoteexecution.OutputFile, md Metadata, err error) {
	acProtos, err := prepareACProtos(key)
	if err != nil {
		err = fault.Wrap(err, fctx.With(ctx))
		return
	}
	resp, err := c.ac.GetActionResult(ctx, &remoteexecution.GetActionResultRequest{
		ActionDigest: acProtos.action.digest,
	})
	if err != nil {
		err = fault.Wrap(err, fmsg.With("GetActionResult failed"), fctx.With(ctx))
		return
	}

	idx := slices.IndexFunc(resp.OutputFiles, func(f *remoteexecution.OutputFile) bool {
		return f.GetPath() == blobFileName
	})
	if idx < 0 {
		err = fault.New("cache blob not found amount output files", fctx.With(ctx))
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
