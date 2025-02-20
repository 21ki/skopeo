package directory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/containers/image/v5/internal/imagedestination/impl"
	"github.com/containers/image/v5/internal/imagedestination/stubs"
	"github.com/containers/image/v5/internal/private"
	"github.com/containers/image/v5/internal/putblobdigest"
	"github.com/containers/image/v5/internal/signature"
	"github.com/containers/image/v5/types"
	"github.com/opencontainers/go-digest"
	perrors "github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const version = "Directory Transport Version: 1.1\n"

// ErrNotContainerImageDir indicates that the directory doesn't match the expected contents of a directory created
// using the 'dir' transport
var ErrNotContainerImageDir = errors.New("not a containers image directory, don't want to overwrite important data")

type dirImageDestination struct {
	impl.Compat
	impl.PropertyMethodsInitialize
	stubs.NoPutBlobPartialInitialize
	stubs.AlwaysSupportsSignatures

	ref dirReference
}

// newImageDestination returns an ImageDestination for writing to a directory.
func newImageDestination(sys *types.SystemContext, ref dirReference) (private.ImageDestination, error) {
	desiredLayerCompression := types.PreserveOriginal
	if sys != nil {
		if sys.DirForceCompress {
			desiredLayerCompression = types.Compress

			if sys.DirForceDecompress {
				return nil, fmt.Errorf("Cannot compress and decompress at the same time")
			}
		}
		if sys.DirForceDecompress {
			desiredLayerCompression = types.Decompress
		}
	}

	// If directory exists check if it is empty
	// if not empty, check whether the contents match that of a container image directory and overwrite the contents
	// if the contents don't match throw an error
	dirExists, err := pathExists(ref.resolvedPath)
	if err != nil {
		return nil, perrors.Wrapf(err, "checking for path %q", ref.resolvedPath)
	}
	if dirExists {
		isEmpty, err := isDirEmpty(ref.resolvedPath)
		if err != nil {
			return nil, err
		}

		if !isEmpty {
			versionExists, err := pathExists(ref.versionPath())
			if err != nil {
				return nil, perrors.Wrapf(err, "checking if path exists %q", ref.versionPath())
			}
			if versionExists {
				contents, err := os.ReadFile(ref.versionPath())
				if err != nil {
					return nil, err
				}
				// check if contents of version file is what we expect it to be
				if string(contents) != version {
					return nil, ErrNotContainerImageDir
				}
			} else {
				return nil, ErrNotContainerImageDir
			}
			// delete directory contents so that only one image is in the directory at a time
			if err = removeDirContents(ref.resolvedPath); err != nil {
				return nil, perrors.Wrapf(err, "erasing contents in %q", ref.resolvedPath)
			}
			logrus.Debugf("overwriting existing container image directory %q", ref.resolvedPath)
		}
	} else {
		// create directory if it doesn't exist
		if err := os.MkdirAll(ref.resolvedPath, 0755); err != nil {
			return nil, perrors.Wrapf(err, "unable to create directory %q", ref.resolvedPath)
		}
	}
	// create version file
	err = os.WriteFile(ref.versionPath(), []byte(version), 0644)
	if err != nil {
		return nil, perrors.Wrapf(err, "creating version file %q", ref.versionPath())
	}

	d := &dirImageDestination{
		PropertyMethodsInitialize: impl.PropertyMethods(impl.Properties{
			SupportedManifestMIMETypes:     nil,
			DesiredLayerCompression:        desiredLayerCompression,
			AcceptsForeignLayerURLs:        false,
			MustMatchRuntimeOS:             false,
			IgnoresEmbeddedDockerReference: false, // N/A, DockerReference() returns nil.
			HasThreadSafePutBlob:           false,
		}),
		NoPutBlobPartialInitialize: stubs.NoPutBlobPartial(ref),

		ref: ref,
	}
	d.Compat = impl.AddCompat(d)
	return d, nil
}

// Reference returns the reference used to set up this destination.  Note that this should directly correspond to user's intent,
// e.g. it should use the public hostname instead of the result of resolving CNAMEs or following redirects.
func (d *dirImageDestination) Reference() types.ImageReference {
	return d.ref
}

// Close removes resources associated with an initialized ImageDestination, if any.
func (d *dirImageDestination) Close() error {
	return nil
}

// PutBlobWithOptions writes contents of stream and returns data representing the result.
// inputInfo.Digest can be optionally provided if known; if provided, and stream is read to the end without error, the digest MUST match the stream contents.
// inputInfo.Size is the expected length of stream, if known.
// inputInfo.MediaType describes the blob format, if known.
// WARNING: The contents of stream are being verified on the fly.  Until stream.Read() returns io.EOF, the contents of the data SHOULD NOT be available
// to any other readers for download using the supplied digest.
// If stream.Read() at any time, ESPECIALLY at end of input, returns an error, PutBlob MUST 1) fail, and 2) delete any data stored so far.
func (d *dirImageDestination) PutBlobWithOptions(ctx context.Context, stream io.Reader, inputInfo types.BlobInfo, options private.PutBlobOptions) (types.BlobInfo, error) {
	blobFile, err := os.CreateTemp(d.ref.path, "dir-put-blob")
	if err != nil {
		return types.BlobInfo{}, err
	}
	succeeded := false
	explicitClosed := false
	defer func() {
		if !explicitClosed {
			blobFile.Close()
		}
		if !succeeded {
			os.Remove(blobFile.Name())
		}
	}()

	digester, stream := putblobdigest.DigestIfCanonicalUnknown(stream, inputInfo)
	// TODO: This can take quite some time, and should ideally be cancellable using ctx.Done().
	size, err := io.Copy(blobFile, stream)
	if err != nil {
		return types.BlobInfo{}, err
	}
	blobDigest := digester.Digest()
	if inputInfo.Size != -1 && size != inputInfo.Size {
		return types.BlobInfo{}, fmt.Errorf("Size mismatch when copying %s, expected %d, got %d", blobDigest, inputInfo.Size, size)
	}
	if err := blobFile.Sync(); err != nil {
		return types.BlobInfo{}, err
	}

	// On POSIX systems, blobFile was created with mode 0600, so we need to make it readable.
	// On Windows, the “permissions of newly created files” argument to syscall.Open is
	// ignored and the file is already readable; besides, blobFile.Chmod, i.e. syscall.Fchmod,
	// always fails on Windows.
	if runtime.GOOS != "windows" {
		if err := blobFile.Chmod(0644); err != nil {
			return types.BlobInfo{}, err
		}
	}

	blobPath := d.ref.layerPath(blobDigest)
	// need to explicitly close the file, since a rename won't otherwise not work on Windows
	blobFile.Close()
	explicitClosed = true
	if err := os.Rename(blobFile.Name(), blobPath); err != nil {
		return types.BlobInfo{}, err
	}
	succeeded = true
	return types.BlobInfo{Digest: blobDigest, Size: size}, nil
}

// TryReusingBlobWithOptions checks whether the transport already contains, or can efficiently reuse, a blob, and if so, applies it to the current destination
// (e.g. if the blob is a filesystem layer, this signifies that the changes it describes need to be applied again when composing a filesystem tree).
// info.Digest must not be empty.
// If the blob has been successfully reused, returns (true, info, nil); info must contain at least a digest and size, and may
// include CompressionOperation and CompressionAlgorithm fields to indicate that a change to the compression type should be
// reflected in the manifest that will be written.
// If the transport can not reuse the requested blob, TryReusingBlob returns (false, {}, nil); it returns a non-nil error only on an unexpected failure.
func (d *dirImageDestination) TryReusingBlobWithOptions(ctx context.Context, info types.BlobInfo, options private.TryReusingBlobOptions) (bool, types.BlobInfo, error) {
	if info.Digest == "" {
		return false, types.BlobInfo{}, fmt.Errorf("Can not check for a blob with unknown digest")
	}
	blobPath := d.ref.layerPath(info.Digest)
	finfo, err := os.Stat(blobPath)
	if err != nil && os.IsNotExist(err) {
		return false, types.BlobInfo{}, nil
	}
	if err != nil {
		return false, types.BlobInfo{}, err
	}
	return true, types.BlobInfo{Digest: info.Digest, Size: finfo.Size()}, nil
}

// PutManifest writes manifest to the destination.
// If instanceDigest is not nil, it contains a digest of the specific manifest instance to write the manifest for (when
// the primary manifest is a manifest list); this should always be nil if the primary manifest is not a manifest list.
// It is expected but not enforced that the instanceDigest, when specified, matches the digest of `manifest` as generated
// by `manifest.Digest()`.
// FIXME? This should also receive a MIME type if known, to differentiate between schema versions.
// If the destination is in principle available, refuses this manifest type (e.g. it does not recognize the schema),
// but may accept a different manifest type, the returned error must be an ManifestTypeRejectedError.
func (d *dirImageDestination) PutManifest(ctx context.Context, manifest []byte, instanceDigest *digest.Digest) error {
	return os.WriteFile(d.ref.manifestPath(instanceDigest), manifest, 0644)
}

// PutSignaturesWithFormat writes a set of signatures to the destination.
// If instanceDigest is not nil, it contains a digest of the specific manifest instance to write or overwrite the signatures for
// (when the primary manifest is a manifest list); this should always be nil if the primary manifest is not a manifest list.
// MUST be called after PutManifest (signatures may reference manifest contents).
func (d *dirImageDestination) PutSignaturesWithFormat(ctx context.Context, signatures []signature.Signature, instanceDigest *digest.Digest) error {
	for i, sig := range signatures {
		blob, err := signature.Blob(sig)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d.ref.signaturePath(i, instanceDigest), blob, 0644); err != nil {
			return err
		}
	}
	return nil
}

// Commit marks the process of storing the image as successful and asks for the image to be persisted.
// unparsedToplevel contains data about the top-level manifest of the source (which may be a single-arch image or a manifest list
// if PutManifest was only called for the single-arch image with instanceDigest == nil), primarily to allow lookups by the
// original manifest list digest, if desired.
// WARNING: This does not have any transactional semantics:
// - Uploaded data MAY be visible to others before Commit() is called
// - Uploaded data MAY be removed or MAY remain around if Close() is called without Commit() (i.e. rollback is allowed but not guaranteed)
func (d *dirImageDestination) Commit(context.Context, types.UnparsedImage) error {
	return nil
}

// returns true if path exists
func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// returns true if directory is empty
func isDirEmpty(path string) (bool, error) {
	files, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(files) == 0, nil
}

// deletes the contents of a directory
func removeDirContents(path string) error {
	files, err := os.ReadDir(path)
	if err != nil {
		return err
	}

	for _, file := range files {
		if err := os.RemoveAll(filepath.Join(path, file.Name())); err != nil {
			return err
		}
	}
	return nil
}
