package buildah

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containers/buildah/copier"
	"github.com/containers/buildah/define"
	"github.com/containers/buildah/docker"
	"github.com/containers/buildah/internal/config"
	"github.com/containers/buildah/internal/mkcw"
	"github.com/containers/buildah/internal/tmpdir"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	is "github.com/containers/image/v5/storage"
	"github.com/containers/image/v5/types"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/archive"
	"github.com/containers/storage/pkg/chrootarchive"
	"github.com/containers/storage/pkg/idtools"
	"github.com/containers/storage/pkg/ioutils"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
)

const (
	// OCIv1ImageManifest is the MIME type of an OCIv1 image manifest,
	// suitable for specifying as a value of the PreferredManifestType
	// member of a CommitOptions structure.  It is also the default.
	OCIv1ImageManifest = define.OCIv1ImageManifest
	// Dockerv2ImageManifest is the MIME type of a Docker v2s2 image
	// manifest, suitable for specifying as a value of the
	// PreferredManifestType member of a CommitOptions structure.
	Dockerv2ImageManifest = define.Dockerv2ImageManifest
)

// ExtractRootfsOptions is consumed by ExtractRootfs() which allows users to
// control whether various information like the like setuid and setgid bits and
// xattrs are preserved when extracting file system objects.
type ExtractRootfsOptions struct {
	StripSetuidBit bool // strip the setuid bit off of items being extracted.
	StripSetgidBit bool // strip the setgid bit off of items being extracted.
	StripXattrs    bool // don't record extended attributes of items being extracted.
}

type containerImageRef struct {
	fromImageName         string
	fromImageID           string
	store                 storage.Store
	compression           archive.Compression
	name                  reference.Named
	names                 []string
	containerID           string
	mountLabel            string
	layerID               string
	oconfig               []byte
	dconfig               []byte
	created               *time.Time
	createdBy             string
	historyComment        string
	annotations           map[string]string
	preferredManifestType string
	squash                bool
	confidentialWorkload  ConfidentialWorkloadOptions
	omitHistory           bool
	emptyLayer            bool
	idMappingOptions      *define.IDMappingOptions
	parent                string
	blobDirectory         string
	preEmptyLayers        []v1.History
	preLayers             []commitLinkedLayerInfo
	postEmptyLayers       []v1.History
	postLayers            []commitLinkedLayerInfo
	overrideChanges       []string
	overrideConfig        *manifest.Schema2Config
	extraImageContent     map[string]string
	compatSetParent       types.OptionalBool
}

type blobLayerInfo struct {
	ID   string
	Size int64
}

type commitLinkedLayerInfo struct {
	layerID            string // more like layer "ID"
	linkedLayer        LinkedLayer
	uncompressedDigest digest.Digest
	size               int64
}

type containerImageSource struct {
	path          string
	ref           *containerImageRef
	store         storage.Store
	containerID   string
	mountLabel    string
	layerID       string
	names         []string
	compression   archive.Compression
	config        []byte
	configDigest  digest.Digest
	manifest      []byte
	manifestType  string
	blobDirectory string
	blobLayers    map[digest.Digest]blobLayerInfo
}

func (i *containerImageRef) NewImage(ctx context.Context, sc *types.SystemContext) (types.ImageCloser, error) {
	src, err := i.NewImageSource(ctx, sc)
	if err != nil {
		return nil, err
	}
	return image.FromSource(ctx, sc, src)
}

func expectedOCIDiffIDs(image v1.Image) int {
	expected := 0
	for _, history := range image.History {
		if !history.EmptyLayer {
			expected = expected + 1
		}
	}
	return expected
}

func expectedDockerDiffIDs(image docker.V2Image) int {
	expected := 0
	for _, history := range image.History {
		if !history.EmptyLayer {
			expected = expected + 1
		}
	}
	return expected
}

// Compute the media types which we need to attach to a layer, given the type of
// compression that we'll be applying.
func computeLayerMIMEType(what string, layerCompression archive.Compression) (omediaType, dmediaType string, err error) {
	omediaType = v1.MediaTypeImageLayer
	dmediaType = docker.V2S2MediaTypeUncompressedLayer
	if layerCompression != archive.Uncompressed {
		switch layerCompression {
		case archive.Gzip:
			omediaType = v1.MediaTypeImageLayerGzip
			dmediaType = manifest.DockerV2Schema2LayerMediaType
			logrus.Debugf("compressing %s with gzip", what)
		case archive.Bzip2:
			// Until the image specs define a media type for bzip2-compressed layers, even if we know
			// how to decompress them, we can't try to compress layers with bzip2.
			return "", "", errors.New("media type for bzip2-compressed layers is not defined")
		case archive.Xz:
			// Until the image specs define a media type for xz-compressed layers, even if we know
			// how to decompress them, we can't try to compress layers with xz.
			return "", "", errors.New("media type for xz-compressed layers is not defined")
		case archive.Zstd:
			// Until the image specs define a media type for zstd-compressed layers, even if we know
			// how to decompress them, we can't try to compress layers with zstd.
			return "", "", errors.New("media type for zstd-compressed layers is not defined")
		default:
			logrus.Debugf("compressing %s with unknown compressor(?)", what)
		}
	}
	return omediaType, dmediaType, nil
}

// Extract the container's whole filesystem as a filesystem image, wrapped
// in LUKS-compatible encryption.
func (i *containerImageRef) extractConfidentialWorkloadFS(options ConfidentialWorkloadOptions) (io.ReadCloser, error) {
	var image v1.Image
	if err := json.Unmarshal(i.oconfig, &image); err != nil {
		return nil, fmt.Errorf("recreating OCI configuration for %q: %w", i.containerID, err)
	}
	if options.TempDir == "" {
		cdir, err := i.store.ContainerDirectory(i.containerID)
		if err != nil {
			return nil, fmt.Errorf("getting the per-container data directory for %q: %w", i.containerID, err)
		}
		tempdir, err := os.MkdirTemp(cdir, "buildah-rootfs")
		if err != nil {
			return nil, fmt.Errorf("creating a temporary data directory to hold a rootfs image for %q: %w", i.containerID, err)
		}
		defer func() {
			if err := os.RemoveAll(tempdir); err != nil {
				logrus.Warnf("removing temporary directory %q: %v", tempdir, err)
			}
		}()
		options.TempDir = tempdir
	}
	mountPoint, err := i.store.Mount(i.containerID, i.mountLabel)
	if err != nil {
		return nil, fmt.Errorf("mounting container %q: %w", i.containerID, err)
	}
	archiveOptions := mkcw.ArchiveOptions{
		AttestationURL:           options.AttestationURL,
		CPUs:                     options.CPUs,
		Memory:                   options.Memory,
		TempDir:                  options.TempDir,
		TeeType:                  options.TeeType,
		IgnoreAttestationErrors:  options.IgnoreAttestationErrors,
		WorkloadID:               options.WorkloadID,
		DiskEncryptionPassphrase: options.DiskEncryptionPassphrase,
		Slop:                     options.Slop,
		FirmwareLibrary:          options.FirmwareLibrary,
		GraphOptions:             i.store.GraphOptions(),
		ExtraImageContent:        i.extraImageContent,
	}
	rc, _, err := mkcw.Archive(mountPoint, &image, archiveOptions)
	if err != nil {
		if _, err2 := i.store.Unmount(i.containerID, false); err2 != nil {
			logrus.Debugf("unmounting container %q: %v", i.containerID, err2)
		}
		return nil, fmt.Errorf("converting rootfs %q: %w", i.containerID, err)
	}
	return ioutils.NewReadCloserWrapper(rc, func() error {
		if err = rc.Close(); err != nil {
			err = fmt.Errorf("closing tar archive of container %q: %w", i.containerID, err)
		}
		if _, err2 := i.store.Unmount(i.containerID, false); err == nil {
			if err2 != nil {
				err2 = fmt.Errorf("unmounting container %q: %w", i.containerID, err2)
			}
			err = err2
		} else {
			logrus.Debugf("unmounting container %q: %v", i.containerID, err2)
		}
		return err
	}), nil
}

// Extract the container's whole filesystem as if it were a single layer.
// The ExtractRootfsOptions control whether or not to preserve setuid and
// setgid bits and extended attributes on contents.
func (i *containerImageRef) extractRootfs(opts ExtractRootfsOptions) (io.ReadCloser, chan error, error) {
	var uidMap, gidMap []idtools.IDMap
	mountPoint, err := i.store.Mount(i.containerID, i.mountLabel)
	if err != nil {
		return nil, nil, fmt.Errorf("mounting container %q: %w", i.containerID, err)
	}
	pipeReader, pipeWriter := io.Pipe()
	errChan := make(chan error, 1)
	go func() {
		defer close(errChan)
		if len(i.extraImageContent) > 0 {
			// Abuse the tar format and _prepend_ the synthesized
			// data items to the archive we'll get from
			// copier.Get(), in a way that looks right to a reader
			// as long as we DON'T Close() the tar Writer.
			filename, _, _, err := i.makeExtraImageContentDiff(false)
			if err != nil {
				errChan <- err
				return
			}
			file, err := os.Open(filename)
			if err != nil {
				errChan <- err
				return
			}
			defer file.Close()
			if _, err = io.Copy(pipeWriter, file); err != nil {
				errChan <- err
				return
			}
		}
		if i.idMappingOptions != nil {
			uidMap, gidMap = convertRuntimeIDMaps(i.idMappingOptions.UIDMap, i.idMappingOptions.GIDMap)
		}
		copierOptions := copier.GetOptions{
			UIDMap:         uidMap,
			GIDMap:         gidMap,
			StripSetuidBit: opts.StripSetuidBit,
			StripSetgidBit: opts.StripSetgidBit,
			StripXattrs:    opts.StripXattrs,
		}
		err := copier.Get(mountPoint, mountPoint, copierOptions, []string{"."}, pipeWriter)
		errChan <- err
		pipeWriter.Close()

	}()
	return ioutils.NewReadCloserWrapper(pipeReader, func() error {
		if err = pipeReader.Close(); err != nil {
			err = fmt.Errorf("closing tar archive of container %q: %w", i.containerID, err)
		}
		if _, err2 := i.store.Unmount(i.containerID, false); err == nil {
			if err2 != nil {
				err2 = fmt.Errorf("unmounting container %q: %w", i.containerID, err2)
			}
			err = err2
		}
		return err
	}), errChan, nil
}

// Build fresh copies of the container configuration structures so that we can edit them
// without making unintended changes to the original Builder.
func (i *containerImageRef) createConfigsAndManifests() (v1.Image, v1.Manifest, docker.V2Image, docker.V2S2Manifest, error) {
	created := time.Now().UTC()
	if i.created != nil {
		created = *i.created
	}

	// Build an empty image, and then decode over it.
	oimage := v1.Image{}
	if err := json.Unmarshal(i.oconfig, &oimage); err != nil {
		return v1.Image{}, v1.Manifest{}, docker.V2Image{}, docker.V2S2Manifest{}, err
	}
	// Always replace this value, since we're newer than our base image.
	oimage.Created = &created
	// Clear the list of diffIDs, since we always repopulate it.
	oimage.RootFS.Type = docker.TypeLayers
	oimage.RootFS.DiffIDs = []digest.Digest{}
	// Only clear the history if we're squashing, otherwise leave it be so that we can append
	// entries to it.
	if i.confidentialWorkload.Convert || i.squash || i.omitHistory {
		oimage.History = []v1.History{}
	}

	// Build an empty image, and then decode over it.
	dimage := docker.V2Image{}
	if err := json.Unmarshal(i.dconfig, &dimage); err != nil {
		return v1.Image{}, v1.Manifest{}, docker.V2Image{}, docker.V2S2Manifest{}, err
	}
	// Set the parent, but only if we want to be compatible with "classic" docker build.
	if i.compatSetParent == types.OptionalBoolTrue {
		dimage.Parent = docker.ID(i.parent)
	}
	// Set the container ID and containerConfig in the docker format.
	dimage.Container = i.containerID
	if dimage.Config != nil {
		dimage.ContainerConfig = *dimage.Config
	}
	// Always replace this value, since we're newer than our base image.
	dimage.Created = created
	// Clear the list of diffIDs, since we always repopulate it.
	dimage.RootFS = &docker.V2S2RootFS{}
	dimage.RootFS.Type = docker.TypeLayers
	dimage.RootFS.DiffIDs = []digest.Digest{}
	// Only clear the history if we're squashing, otherwise leave it be so
	// that we can append entries to it.  Clear the parent, too, to reflect
	// that we no longer include its layers and history.
	if i.confidentialWorkload.Convert || i.squash || i.omitHistory {
		dimage.Parent = ""
		dimage.History = []docker.V2S2History{}
	}

	// If we were supplied with a configuration, copy fields from it to
	// matching fields in both formats.
	if err := config.Override(dimage.Config, &oimage.Config, i.overrideChanges, i.overrideConfig); err != nil {
		return v1.Image{}, v1.Manifest{}, docker.V2Image{}, docker.V2S2Manifest{}, fmt.Errorf("applying changes: %w", err)
	}

	// If we're producing a confidential workload, override the command and
	// assorted other settings that aren't expected to work correctly.
	if i.confidentialWorkload.Convert {
		dimage.Config.Entrypoint = []string{"/entrypoint"}
		oimage.Config.Entrypoint = []string{"/entrypoint"}
		dimage.Config.Cmd = nil
		oimage.Config.Cmd = nil
		dimage.Config.User = ""
		oimage.Config.User = ""
		dimage.Config.WorkingDir = ""
		oimage.Config.WorkingDir = ""
		dimage.Config.Healthcheck = nil
		dimage.Config.Shell = nil
		dimage.Config.Volumes = nil
		oimage.Config.Volumes = nil
		dimage.Config.ExposedPorts = nil
		oimage.Config.ExposedPorts = nil
	}

	// Build empty manifests.  The Layers lists will be populated later.
	omanifest := v1.Manifest{
		Versioned: specs.Versioned{
			SchemaVersion: 2,
		},
		MediaType: v1.MediaTypeImageManifest,
		Config: v1.Descriptor{
			MediaType: v1.MediaTypeImageConfig,
		},
		Layers:      []v1.Descriptor{},
		Annotations: i.annotations,
	}

	dmanifest := docker.V2S2Manifest{
		V2Versioned: docker.V2Versioned{
			SchemaVersion: 2,
			MediaType:     manifest.DockerV2Schema2MediaType,
		},
		Config: docker.V2S2Descriptor{
			MediaType: manifest.DockerV2Schema2ConfigMediaType,
		},
		Layers: []docker.V2S2Descriptor{},
	}

	return oimage, omanifest, dimage, dmanifest, nil
}

func (i *containerImageRef) NewImageSource(_ context.Context, _ *types.SystemContext) (src types.ImageSource, err error) {
	// Decide which type of manifest and configuration output we're going to provide.
	manifestType := i.preferredManifestType
	// If it's not a format we support, return an error.
	if manifestType != v1.MediaTypeImageManifest && manifestType != manifest.DockerV2Schema2MediaType {
		return nil, fmt.Errorf("no supported manifest types (attempted to use %q, only know %q and %q)",
			manifestType, v1.MediaTypeImageManifest, manifest.DockerV2Schema2MediaType)
	}
	// These maps will let us check if a layer ID is part of one group or another.
	parentLayerIDs := make(map[string]bool)
	apiLayerIDs := make(map[string]bool)
	// Start building the list of layers with any prepended layers.
	layers := []string{}
	for _, preLayer := range i.preLayers {
		layers = append(layers, preLayer.layerID)
		apiLayerIDs[preLayer.layerID] = true
	}
	// Now look at the read-write layer, and prepare to work our way back
	// through all of its parent layers.
	layerID := i.layerID
	layer, err := i.store.Layer(layerID)
	if err != nil {
		return nil, fmt.Errorf("unable to read layer %q: %w", layerID, err)
	}
	// Walk the list of parent layers, prepending each as we go.  If we're squashing
	// or making a confidential workload, we're only producing one layer, so stop at
	// the layer ID of the top layer, which we won't really be using anyway.
	for layer != nil {
		if layerID == i.layerID {
			// append the layer for this container to the list,
			// whether it's first or after some prepended layers
			layers = append(layers, layerID)
		} else {
			// prepend this parent layer to the list
			layers = append(append([]string{}, layerID), layers...)
			parentLayerIDs[layerID] = true
		}
		layerID = layer.Parent
		if layerID == "" || i.confidentialWorkload.Convert || i.squash {
			err = nil
			break
		}
		layer, err = i.store.Layer(layerID)
		if err != nil {
			return nil, fmt.Errorf("unable to read layer %q: %w", layerID, err)
		}
	}
	layer = nil

	// If we're slipping in a synthesized layer to hold some files, we need
	// to add a placeholder for it to the list just after the read-write
	// layer.  Confidential workloads and squashed images will just inline
	// the files, so we don't need to create a layer in those cases.
	const synthesizedLayerID = "(synthesized layer)"
	if len(i.extraImageContent) > 0 && !i.confidentialWorkload.Convert && !i.squash {
		layers = append(layers, synthesizedLayerID)
	}
	// Now add any API-supplied layers we have to append.
	for _, postLayer := range i.postLayers {
		layers = append(layers, postLayer.layerID)
		apiLayerIDs[postLayer.layerID] = true
	}
	logrus.Debugf("layer list: %q", layers)

	// It's simpler from here on to keep track of these as a group.
	apiLayers := append(slices.Clone(i.preLayers), slices.Clone(i.postLayers)...)

	// Make a temporary directory to hold blobs.
	path, err := os.MkdirTemp(tmpdir.GetTempDir(), define.Package)
	if err != nil {
		return nil, fmt.Errorf("creating temporary directory to hold layer blobs: %w", err)
	}
	logrus.Debugf("using %q to hold temporary data", path)
	defer func() {
		if src == nil {
			err2 := os.RemoveAll(path)
			if err2 != nil {
				logrus.Errorf("error removing layer blob directory: %v", err)
			}
		}
	}()

	// Build fresh copies of the configurations and manifest so that we don't mess with any
	// values in the Builder object itself.
	oimage, omanifest, dimage, dmanifest, err := i.createConfigsAndManifests()
	if err != nil {
		return nil, err
	}

	// Extract each layer and compute its digests, both compressed (if requested) and uncompressed.
	var extraImageContentDiff string
	var extraImageContentDiffDigest digest.Digest
	blobLayers := make(map[digest.Digest]blobLayerInfo)
	for _, layerID := range layers {
		what := fmt.Sprintf("layer %q", layerID)
		if i.confidentialWorkload.Convert || i.squash {
			what = fmt.Sprintf("container %q", i.containerID)
		}
		if layerID == synthesizedLayerID {
			what = synthesizedLayerID
		}
		if apiLayerIDs[layerID] {
			what = layerID
		}
		// The default layer media type assumes no compression.
		omediaType := v1.MediaTypeImageLayer
		dmediaType := docker.V2S2MediaTypeUncompressedLayer
		// Look up this layer.
		var layerUncompressedDigest digest.Digest
		var layerUncompressedSize int64
		linkedLayerHasLayerID := func(l commitLinkedLayerInfo) bool { return l.layerID == layerID }
		if apiLayerIDs[layerID] {
			// API-provided prepended or appended layer
			apiLayerIndex := slices.IndexFunc(apiLayers, linkedLayerHasLayerID)
			layerUncompressedDigest = apiLayers[apiLayerIndex].uncompressedDigest
			layerUncompressedSize = apiLayers[apiLayerIndex].size
		} else if layerID == synthesizedLayerID {
			// layer diff consisting of extra files to synthesize into a layer
			diffFilename, digest, size, err := i.makeExtraImageContentDiff(true)
			if err != nil {
				return nil, fmt.Errorf("unable to generate layer for additional content: %w", err)
			}
			extraImageContentDiff = diffFilename
			extraImageContentDiffDigest = digest
			layerUncompressedDigest = digest
			layerUncompressedSize = size
		} else {
			// "normal" layer
			layer, err := i.store.Layer(layerID)
			if err != nil {
				return nil, fmt.Errorf("unable to locate layer %q: %w", layerID, err)
			}
			layerID = layer.ID
			layerUncompressedDigest = layer.UncompressedDigest
			layerUncompressedSize = layer.UncompressedSize
		}
		// We already know the digest of the contents of parent layers,
		// so if this is a parent layer, and we know its digest, reuse
		// its blobsum, diff ID, and size.
		if !i.confidentialWorkload.Convert && !i.squash && parentLayerIDs[layerID] && layerUncompressedDigest != "" {
			layerBlobSum := layerUncompressedDigest
			layerBlobSize := layerUncompressedSize
			diffID := layerUncompressedDigest
			// Note this layer in the manifest, using the appropriate blobsum.
			olayerDescriptor := v1.Descriptor{
				MediaType: omediaType,
				Digest:    layerBlobSum,
				Size:      layerBlobSize,
			}
			omanifest.Layers = append(omanifest.Layers, olayerDescriptor)
			dlayerDescriptor := docker.V2S2Descriptor{
				MediaType: dmediaType,
				Digest:    layerBlobSum,
				Size:      layerBlobSize,
			}
			dmanifest.Layers = append(dmanifest.Layers, dlayerDescriptor)
			// Note this layer in the list of diffIDs, again using the uncompressed digest.
			oimage.RootFS.DiffIDs = append(oimage.RootFS.DiffIDs, diffID)
			dimage.RootFS.DiffIDs = append(dimage.RootFS.DiffIDs, diffID)
			blobLayers[diffID] = blobLayerInfo{
				ID:   layerID,
				Size: layerBlobSize,
			}
			continue
		}
		// Figure out if we need to change the media type, in case we've changed the compression.
		omediaType, dmediaType, err = computeLayerMIMEType(what, i.compression)
		if err != nil {
			return nil, err
		}
		// Start reading either the layer or the whole container rootfs.
		noCompression := archive.Uncompressed
		diffOptions := &storage.DiffOptions{
			Compression: &noCompression,
		}
		var rc io.ReadCloser
		var errChan chan error
		if i.confidentialWorkload.Convert {
			// Convert the root filesystem into an encrypted disk image.
			rc, err = i.extractConfidentialWorkloadFS(i.confidentialWorkload)
			if err != nil {
				return nil, err
			}
		} else if i.squash {
			// Extract the root filesystem as a single layer.
			rc, errChan, err = i.extractRootfs(ExtractRootfsOptions{})
			if err != nil {
				return nil, err
			}
		} else {
			if apiLayerIDs[layerID] {
				// We're reading an API-supplied blob.
				apiLayerIndex := slices.IndexFunc(apiLayers, linkedLayerHasLayerID)
				f, err := os.Open(apiLayers[apiLayerIndex].linkedLayer.BlobPath)
				if err != nil {
					return nil, fmt.Errorf("opening layer blob for %s: %w", layerID, err)
				}
				rc = f
			} else if layerID == synthesizedLayerID {
				// Slip in additional content as an additional layer.
				if rc, err = os.Open(extraImageContentDiff); err != nil {
					return nil, err
				}
			} else {
				// If we're up to the final layer, but we don't want to
				// include a diff for it, we're done.
				if i.emptyLayer && layerID == i.layerID {
					continue
				}
				// Extract this layer, one of possibly many.
				rc, err = i.store.Diff("", layerID, diffOptions)
				if err != nil {
					return nil, fmt.Errorf("extracting %s: %w", what, err)
				}
			}
		}
		srcHasher := digest.Canonical.Digester()
		// Set up to write the possibly-recompressed blob.
		layerFile, err := os.OpenFile(filepath.Join(path, "layer"), os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("opening file for %s: %w", what, err)
		}

		counter := ioutils.NewWriteCounter(layerFile)
		var destHasher digest.Digester
		var multiWriter io.Writer
		// Avoid rehashing when we do not compress.
		if i.compression != archive.Uncompressed {
			destHasher = digest.Canonical.Digester()
			multiWriter = io.MultiWriter(counter, destHasher.Hash())
		} else {
			destHasher = srcHasher
			multiWriter = counter
		}
		// Compress the layer, if we're recompressing it.
		writeCloser, err := archive.CompressStream(multiWriter, i.compression)
		if err != nil {
			layerFile.Close()
			rc.Close()
			return nil, fmt.Errorf("compressing %s: %w", what, err)
		}
		writer := io.MultiWriter(writeCloser, srcHasher.Hash())
		{
			// Tweak the contents of layers we're creating.
			nestedWriteCloser := ioutils.NewWriteCloserWrapper(writer, writeCloser.Close)
			writeCloser = newTarFilterer(nestedWriteCloser, func(hdr *tar.Header) (bool, bool, io.Reader) {
				// Scrub any local user names that might correspond to UIDs or GIDs of
				// files in this layer.
				hdr.Uname, hdr.Gname = "", ""
				// Use specified timestamps in the layer, if we're doing that for history
				// entries.
				if i.created != nil {
					// Changing a zeroed field to a non-zero field can affect the
					// format that the library uses for writing the header, so only
					// change fields that are already set to avoid changing the
					// format (and as a result, changing the length) of the header
					// that we write.
					if !hdr.ModTime.IsZero() {
						hdr.ModTime = *i.created
					}
					if !hdr.AccessTime.IsZero() {
						hdr.AccessTime = *i.created
					}
					if !hdr.ChangeTime.IsZero() {
						hdr.ChangeTime = *i.created
					}
					return false, false, nil
				}
				return false, false, nil
			})
			writer = io.Writer(writeCloser)
		}
		// Okay, copy from the raw diff through the filter, compressor, and counter and
		// digesters.
		size, err := io.Copy(writer, rc)
		if err := writeCloser.Close(); err != nil {
			layerFile.Close()
			rc.Close()
			return nil, fmt.Errorf("storing %s to file: %w on pipe close", what, err)
		}
		if err := layerFile.Close(); err != nil {
			rc.Close()
			return nil, fmt.Errorf("storing %s to file: %w on file close", what, err)
		}
		rc.Close()

		if errChan != nil {
			err = <-errChan
			if err != nil {
				return nil, fmt.Errorf("extracting container rootfs: %w", err)
			}
		}

		if err != nil {
			return nil, fmt.Errorf("storing %s to file: %w", what, err)
		}
		if i.compression == archive.Uncompressed {
			if size != counter.Count {
				return nil, fmt.Errorf("storing %s to file: inconsistent layer size (copied %d, wrote %d)", what, size, counter.Count)
			}
		} else {
			size = counter.Count
		}
		logrus.Debugf("%s size is %d bytes, uncompressed digest %s, possibly-compressed digest %s", what, size, srcHasher.Digest().String(), destHasher.Digest().String())
		// Rename the layer so that we can more easily find it by digest later.
		finalBlobName := filepath.Join(path, destHasher.Digest().String())
		if err = os.Rename(filepath.Join(path, "layer"), finalBlobName); err != nil {
			return nil, fmt.Errorf("storing %s to file while renaming %q to %q: %w", what, filepath.Join(path, "layer"), finalBlobName, err)
		}
		// Add a note in the manifest about the layer.  The blobs are identified by their possibly-
		// compressed blob digests.
		olayerDescriptor := v1.Descriptor{
			MediaType: omediaType,
			Digest:    destHasher.Digest(),
			Size:      size,
		}
		omanifest.Layers = append(omanifest.Layers, olayerDescriptor)
		dlayerDescriptor := docker.V2S2Descriptor{
			MediaType: dmediaType,
			Digest:    destHasher.Digest(),
			Size:      size,
		}
		dmanifest.Layers = append(dmanifest.Layers, dlayerDescriptor)
		// Add a note about the diffID, which is always the layer's uncompressed digest.
		oimage.RootFS.DiffIDs = append(oimage.RootFS.DiffIDs, srcHasher.Digest())
		dimage.RootFS.DiffIDs = append(dimage.RootFS.DiffIDs, srcHasher.Digest())
	}

	// Build history notes in the image configurations.
	appendHistory := func(history []v1.History, empty bool) {
		for i := range history {
			var created *time.Time
			if history[i].Created != nil {
				copiedTimestamp := *history[i].Created
				created = &copiedTimestamp
			}
			onews := v1.History{
				Created:    created,
				CreatedBy:  history[i].CreatedBy,
				Author:     history[i].Author,
				Comment:    history[i].Comment,
				EmptyLayer: empty,
			}
			oimage.History = append(oimage.History, onews)
			if created == nil {
				created = &time.Time{}
			}
			dnews := docker.V2S2History{
				Created:    *created,
				CreatedBy:  history[i].CreatedBy,
				Author:     history[i].Author,
				Comment:    history[i].Comment,
				EmptyLayer: empty,
			}
			dimage.History = append(dimage.History, dnews)
		}
	}

	// Only attempt to append history if history was not disabled explicitly.
	if !i.omitHistory {
		// Keep track of how many entries the base image's history had
		// before we started adding to it.
		baseImageHistoryLen := len(oimage.History)

		// Add history entries for prepended empty layers.
		appendHistory(i.preEmptyLayers, true)
		// Add history entries for prepended API-supplied layers.
		for _, h := range i.preLayers {
			appendHistory([]v1.History{h.linkedLayer.History}, h.linkedLayer.History.EmptyLayer)
		}
		// Add a history entry for this layer, empty or not.
		created := time.Now().UTC()
		if i.created != nil {
			created = (*i.created).UTC()
		}
		onews := v1.History{
			Created:    &created,
			CreatedBy:  i.createdBy,
			Author:     oimage.Author,
			EmptyLayer: i.emptyLayer,
			Comment:    i.historyComment,
		}
		oimage.History = append(oimage.History, onews)
		dnews := docker.V2S2History{
			Created:    created,
			CreatedBy:  i.createdBy,
			Author:     dimage.Author,
			EmptyLayer: i.emptyLayer,
			Comment:    i.historyComment,
		}
		dimage.History = append(dimage.History, dnews)
		// Add a history entry for the extra image content if we added a layer for it.
		// This diff was added to the list of layers before API-supplied layers that
		// needed to be appended, and we need to keep the order of history entries for
		// not-empty layers consistent with that.
		if extraImageContentDiff != "" {
			createdBy := fmt.Sprintf(`/bin/sh -c #(nop) ADD dir:%s in /",`, extraImageContentDiffDigest.Encoded())
			onews := v1.History{
				Created:   &created,
				CreatedBy: createdBy,
			}
			oimage.History = append(oimage.History, onews)
			dnews := docker.V2S2History{
				Created:   created,
				CreatedBy: createdBy,
			}
			dimage.History = append(dimage.History, dnews)
		}
		// Add history entries for appended empty layers.
		appendHistory(i.postEmptyLayers, true)
		// Add history entries for appended API-supplied layers.
		for _, h := range i.postLayers {
			appendHistory([]v1.History{h.linkedLayer.History}, h.linkedLayer.History.EmptyLayer)
		}

		// Assemble a comment indicating which base image was used, if it wasn't
		// just an image ID, and add it to the first history entry we added.
		var fromComment string
		if strings.Contains(i.parent, i.fromImageID) && i.fromImageName != "" && !strings.HasPrefix(i.fromImageID, i.fromImageName) {
			if oimage.History[baseImageHistoryLen].Comment != "" {
				fromComment = " "
			}
			fromComment += "FROM " + i.fromImageName
		}
		oimage.History[baseImageHistoryLen].Comment += fromComment
		dimage.History[baseImageHistoryLen].Comment += fromComment

		// Confidence check that we didn't just create a mismatch between non-empty layers in the
		// history and the number of diffIDs.  Only applicable if the base image (if there was
		// one) provided us at least one entry to use as a starting point.
		if baseImageHistoryLen != 0 {
			expectedDiffIDs := expectedOCIDiffIDs(oimage)
			if len(oimage.RootFS.DiffIDs) != expectedDiffIDs {
				return nil, fmt.Errorf("internal error: history lists %d non-empty layers, but we have %d layers on disk", expectedDiffIDs, len(oimage.RootFS.DiffIDs))
			}
			expectedDiffIDs = expectedDockerDiffIDs(dimage)
			if len(dimage.RootFS.DiffIDs) != expectedDiffIDs {
				return nil, fmt.Errorf("internal error: history lists %d non-empty layers, but we have %d layers on disk", expectedDiffIDs, len(dimage.RootFS.DiffIDs))
			}
		}
	}

	// Encode the image configuration blob.
	oconfig, err := json.Marshal(&oimage)
	if err != nil {
		return nil, fmt.Errorf("encoding %#v as json: %w", oimage, err)
	}
	logrus.Debugf("OCIv1 config = %s", oconfig)

	// Add the configuration blob to the manifest.
	omanifest.Config.Digest = digest.Canonical.FromBytes(oconfig)
	omanifest.Config.Size = int64(len(oconfig))
	omanifest.Config.MediaType = v1.MediaTypeImageConfig

	// Encode the manifest.
	omanifestbytes, err := json.Marshal(&omanifest)
	if err != nil {
		return nil, fmt.Errorf("encoding %#v as json: %w", omanifest, err)
	}
	logrus.Debugf("OCIv1 manifest = %s", omanifestbytes)

	// Encode the image configuration blob.
	dconfig, err := json.Marshal(&dimage)
	if err != nil {
		return nil, fmt.Errorf("encoding %#v as json: %w", dimage, err)
	}
	logrus.Debugf("Docker v2s2 config = %s", dconfig)

	// Add the configuration blob to the manifest.
	dmanifest.Config.Digest = digest.Canonical.FromBytes(dconfig)
	dmanifest.Config.Size = int64(len(dconfig))
	dmanifest.Config.MediaType = manifest.DockerV2Schema2ConfigMediaType

	// Encode the manifest.
	dmanifestbytes, err := json.Marshal(&dmanifest)
	if err != nil {
		return nil, fmt.Errorf("encoding %#v as json: %w", dmanifest, err)
	}
	logrus.Debugf("Docker v2s2 manifest = %s", dmanifestbytes)

	// Decide which manifest and configuration blobs we'll actually output.
	var config []byte
	var imageManifest []byte
	switch manifestType {
	case v1.MediaTypeImageManifest:
		imageManifest = omanifestbytes
		config = oconfig
	case manifest.DockerV2Schema2MediaType:
		imageManifest = dmanifestbytes
		config = dconfig
	default:
		panic("unreachable code: unsupported manifest type")
	}
	src = &containerImageSource{
		path:          path,
		ref:           i,
		store:         i.store,
		containerID:   i.containerID,
		mountLabel:    i.mountLabel,
		layerID:       i.layerID,
		names:         i.names,
		compression:   i.compression,
		config:        config,
		configDigest:  digest.Canonical.FromBytes(config),
		manifest:      imageManifest,
		manifestType:  manifestType,
		blobDirectory: i.blobDirectory,
		blobLayers:    blobLayers,
	}
	return src, nil
}

func (i *containerImageRef) NewImageDestination(_ context.Context, _ *types.SystemContext) (types.ImageDestination, error) {
	return nil, errors.New("can't write to a container")
}

func (i *containerImageRef) DockerReference() reference.Named {
	return i.name
}

func (i *containerImageRef) StringWithinTransport() string {
	if len(i.names) > 0 {
		return i.names[0]
	}
	return ""
}

func (i *containerImageRef) DeleteImage(context.Context, *types.SystemContext) error {
	// we were never here
	return nil
}

func (i *containerImageRef) PolicyConfigurationIdentity() string {
	return ""
}

func (i *containerImageRef) PolicyConfigurationNamespaces() []string {
	return nil
}

func (i *containerImageRef) Transport() types.ImageTransport {
	return is.Transport
}

func (i *containerImageSource) Close() error {
	err := os.RemoveAll(i.path)
	if err != nil {
		return fmt.Errorf("removing layer blob directory: %w", err)
	}
	return nil
}

func (i *containerImageSource) Reference() types.ImageReference {
	return i.ref
}

func (i *containerImageSource) GetSignatures(_ context.Context, _ *digest.Digest) ([][]byte, error) {
	return nil, nil
}

func (i *containerImageSource) GetManifest(_ context.Context, _ *digest.Digest) ([]byte, string, error) {
	return i.manifest, i.manifestType, nil
}

func (i *containerImageSource) LayerInfosForCopy(_ context.Context, _ *digest.Digest) ([]types.BlobInfo, error) {
	return nil, nil
}

func (i *containerImageSource) HasThreadSafeGetBlob() bool {
	return false
}

func (i *containerImageSource) GetBlob(_ context.Context, blob types.BlobInfo, _ types.BlobInfoCache) (reader io.ReadCloser, size int64, err error) {
	if blob.Digest == i.configDigest {
		logrus.Debugf("start reading config")
		reader := bytes.NewReader(i.config)
		closer := func() error {
			logrus.Debugf("finished reading config")
			return nil
		}
		return ioutils.NewReadCloserWrapper(reader, closer), reader.Size(), nil
	}
	var layerReadCloser io.ReadCloser
	size = -1
	if blobLayerInfo, ok := i.blobLayers[blob.Digest]; ok {
		noCompression := archive.Uncompressed
		diffOptions := &storage.DiffOptions{
			Compression: &noCompression,
		}
		layerReadCloser, err = i.store.Diff("", blobLayerInfo.ID, diffOptions)
		size = blobLayerInfo.Size
	} else {
		for _, blobDir := range []string{i.blobDirectory, i.path} {
			var layerFile *os.File
			layerFile, err = os.OpenFile(filepath.Join(blobDir, blob.Digest.String()), os.O_RDONLY, 0600)
			if err == nil {
				st, err := layerFile.Stat()
				if err != nil {
					logrus.Warnf("error reading size of layer file %q: %v", blob.Digest.String(), err)
				} else {
					size = st.Size()
					layerReadCloser = layerFile
					break
				}
				layerFile.Close()
			}
			if !errors.Is(err, os.ErrNotExist) {
				logrus.Debugf("error checking for layer %q in %q: %v", blob.Digest.String(), blobDir, err)
			}
		}
	}
	if err != nil || layerReadCloser == nil || size == -1 {
		logrus.Debugf("error reading layer %q: %v", blob.Digest.String(), err)
		return nil, -1, fmt.Errorf("opening layer blob: %w", err)
	}
	logrus.Debugf("reading layer %q", blob.Digest.String())
	closer := func() error {
		logrus.Debugf("finished reading layer %q", blob.Digest.String())
		if err := layerReadCloser.Close(); err != nil {
			return fmt.Errorf("closing layer %q after reading: %w", blob.Digest.String(), err)
		}
		return nil
	}
	return ioutils.NewReadCloserWrapper(layerReadCloser, closer), size, nil
}

// makeExtraImageContentDiff creates an archive file containing the contents of
// files named in i.extraImageContent.  The footer that marks the end of the
// archive may be omitted.
func (i *containerImageRef) makeExtraImageContentDiff(includeFooter bool) (_ string, _ digest.Digest, _ int64, retErr error) {
	cdir, err := i.store.ContainerDirectory(i.containerID)
	if err != nil {
		return "", "", -1, err
	}
	diff, err := os.CreateTemp(cdir, "extradiff")
	if err != nil {
		return "", "", -1, err
	}
	defer diff.Close()
	defer func() {
		if retErr != nil {
			os.Remove(diff.Name())
		}
	}()
	digester := digest.Canonical.Digester()
	counter := ioutils.NewWriteCounter(digester.Hash())
	tw := tar.NewWriter(io.MultiWriter(diff, counter))
	created := time.Now()
	if i.created != nil {
		created = *i.created
	}
	for path, contents := range i.extraImageContent {
		if err := func() error {
			content, err := os.Open(contents)
			if err != nil {
				return err
			}
			defer content.Close()
			st, err := content.Stat()
			if err != nil {
				return err
			}
			if err := tw.WriteHeader(&tar.Header{
				Name:     path,
				Typeflag: tar.TypeReg,
				Mode:     0o644,
				ModTime:  created,
				Size:     st.Size(),
			}); err != nil {
				return err
			}
			if _, err := io.Copy(tw, content); err != nil {
				return err
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			return nil
		}(); err != nil {
			return "", "", -1, err
		}
	}
	if !includeFooter {
		return diff.Name(), "", -1, nil
	}
	tw.Close()
	return diff.Name(), digester.Digest(), counter.Count, nil
}

// makeLinkedLayerInfos calculates the size and digest information for a layer
// we intend to add to the image that we're committing.
func (b *Builder) makeLinkedLayerInfos(layers []LinkedLayer, layerType string) ([]commitLinkedLayerInfo, error) {
	if layers == nil {
		return nil, nil
	}
	infos := make([]commitLinkedLayerInfo, 0, len(layers))
	for i, layer := range layers {
		// complain if EmptyLayer and "is the BlobPath empty" don't agree
		if layer.History.EmptyLayer != (layer.BlobPath == "") {
			return nil, fmt.Errorf("internal error: layer-is-empty = %v, but content path is %q", layer.History.EmptyLayer, layer.BlobPath)
		}
		// if there's no layer contents, we're done with this one
		if layer.History.EmptyLayer {
			continue
		}
		// check if it's a directory or a non-directory
		st, err := os.Stat(layer.BlobPath)
		if err != nil {
			return nil, fmt.Errorf("checking if layer content %s is a directory: %w", layer.BlobPath, err)
		}
		info := commitLinkedLayerInfo{
			layerID:     fmt.Sprintf("(%s %d)", layerType, i+1),
			linkedLayer: layer,
		}
		if err = func() error {
			if st.IsDir() {
				// if it's a directory, archive it and digest the archive while we're storing a copy somewhere
				cdir, err := b.store.ContainerDirectory(b.ContainerID)
				if err != nil {
					return fmt.Errorf("determining directory for working container: %w", err)
				}
				f, err := os.CreateTemp(cdir, "")
				if err != nil {
					return fmt.Errorf("creating temporary file to hold blob for %q: %w", info.linkedLayer.BlobPath, err)
				}
				defer f.Close()
				rc, err := chrootarchive.Tar(info.linkedLayer.BlobPath, nil, info.linkedLayer.BlobPath)
				if err != nil {
					return fmt.Errorf("generating a layer blob from %q: %w", info.linkedLayer.BlobPath, err)
				}
				digester := digest.Canonical.Digester()
				sizeCounter := ioutils.NewWriteCounter(digester.Hash())
				_, copyErr := io.Copy(f, io.TeeReader(rc, sizeCounter))
				if err := rc.Close(); err != nil {
					return fmt.Errorf("storing a copy of %q: %w", info.linkedLayer.BlobPath, err)
				}
				if copyErr != nil {
					return fmt.Errorf("storing a copy of %q: %w", info.linkedLayer.BlobPath, copyErr)
				}
				info.uncompressedDigest = digester.Digest()
				info.size = sizeCounter.Count
				info.linkedLayer.BlobPath = f.Name()
			} else {
				// if it's not a directory, just digest it
				f, err := os.Open(info.linkedLayer.BlobPath)
				if err != nil {
					return err
				}
				defer f.Close()
				sizeCounter := ioutils.NewWriteCounter(io.Discard)
				uncompressedDigest, err := digest.Canonical.FromReader(io.TeeReader(f, sizeCounter))
				if err != nil {
					return err
				}
				info.uncompressedDigest = uncompressedDigest
				info.size = sizeCounter.Count
			}
			return nil
		}(); err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// makeContainerImageRef creates a containers/image/v5/types.ImageReference
// which is mainly used for representing the working container as a source
// image that can be copied, which is how we commit the container to create the
// image.
func (b *Builder) makeContainerImageRef(options CommitOptions) (*containerImageRef, error) {
	if (len(options.PrependedLinkedLayers) > 0 || len(options.AppendedLinkedLayers) > 0) &&
		(options.ConfidentialWorkloadOptions.Convert || options.Squash) {
		return nil, errors.New("can't add prebuilt layers and produce an image with only one layer, at the same time")
	}
	var name reference.Named
	container, err := b.store.Container(b.ContainerID)
	if err != nil {
		return nil, fmt.Errorf("locating container %q: %w", b.ContainerID, err)
	}
	if len(container.Names) > 0 {
		if parsed, err2 := reference.ParseNamed(container.Names[0]); err2 == nil {
			name = parsed
		}
	}
	manifestType := options.PreferredManifestType
	if manifestType == "" {
		manifestType = define.OCIv1ImageManifest
	}

	for _, u := range options.UnsetEnvs {
		b.UnsetEnv(u)
	}
	oconfig, err := json.Marshal(&b.OCIv1)
	if err != nil {
		return nil, fmt.Errorf("encoding OCI-format image configuration %#v: %w", b.OCIv1, err)
	}
	dconfig, err := json.Marshal(&b.Docker)
	if err != nil {
		return nil, fmt.Errorf("encoding docker-format image configuration %#v: %w", b.Docker, err)
	}
	var created *time.Time
	if options.HistoryTimestamp != nil {
		historyTimestampUTC := options.HistoryTimestamp.UTC()
		created = &historyTimestampUTC
	}
	createdBy := b.CreatedBy()
	if createdBy == "" {
		createdBy = strings.Join(b.Shell(), " ")
		if createdBy == "" {
			createdBy = "/bin/sh"
		}
	}

	parent := ""
	forceOmitHistory := false
	if b.FromImageID != "" {
		parentDigest := digest.NewDigestFromEncoded(digest.Canonical, b.FromImageID)
		if parentDigest.Validate() == nil {
			parent = parentDigest.String()
		}
		if !options.OmitHistory && len(b.OCIv1.History) == 0 && len(b.OCIv1.RootFS.DiffIDs) != 0 {
			// Parent had layers, but no history.  We shouldn't confuse
			// our own confidence checks by adding history for layers
			// that we're adding, creating an image with multiple layers,
			// only some of which have history entries, which would be
			// broken in confusing ways.
			b.Logger.Debugf("parent image %q had no history but had %d layers, assuming OmitHistory", b.FromImageID, len(b.OCIv1.RootFS.DiffIDs))
			forceOmitHistory = true
		}
	}

	preLayerInfos, err := b.makeLinkedLayerInfos(append(slices.Clone(b.PrependedLinkedLayers), slices.Clone(options.PrependedLinkedLayers)...), "prepended layer")
	if err != nil {
		return nil, err
	}
	postLayerInfos, err := b.makeLinkedLayerInfos(append(slices.Clone(options.AppendedLinkedLayers), slices.Clone(b.AppendedLinkedLayers)...), "appended layer")
	if err != nil {
		return nil, err
	}

	ref := &containerImageRef{
		fromImageName:         b.FromImage,
		fromImageID:           b.FromImageID,
		store:                 b.store,
		compression:           options.Compression,
		name:                  name,
		names:                 container.Names,
		containerID:           container.ID,
		mountLabel:            b.MountLabel,
		layerID:               container.LayerID,
		oconfig:               oconfig,
		dconfig:               dconfig,
		created:               created,
		createdBy:             createdBy,
		historyComment:        b.HistoryComment(),
		annotations:           b.Annotations(),
		preferredManifestType: manifestType,
		squash:                options.Squash,
		confidentialWorkload:  options.ConfidentialWorkloadOptions,
		omitHistory:           options.OmitHistory || forceOmitHistory,
		emptyLayer:            options.EmptyLayer && !options.Squash && !options.ConfidentialWorkloadOptions.Convert,
		idMappingOptions:      &b.IDMappingOptions,
		parent:                parent,
		blobDirectory:         options.BlobDirectory,
		preEmptyLayers:        slices.Clone(b.PrependedEmptyLayers),
		preLayers:             preLayerInfos,
		postEmptyLayers:       slices.Clone(b.AppendedEmptyLayers),
		postLayers:            postLayerInfos,
		overrideChanges:       options.OverrideChanges,
		overrideConfig:        options.OverrideConfig,
		extraImageContent:     maps.Clone(options.ExtraImageContent),
		compatSetParent:       options.CompatSetParent,
	}
	if ref.created != nil {
		for i := range ref.preEmptyLayers {
			ref.preEmptyLayers[i].Created = ref.created
		}
		for i := range ref.preLayers {
			ref.preLayers[i].linkedLayer.History.Created = ref.created
		}
		for i := range ref.postEmptyLayers {
			ref.postEmptyLayers[i].Created = ref.created
		}
		for i := range ref.postLayers {
			ref.postLayers[i].linkedLayer.History.Created = ref.created
		}
	}
	return ref, nil
}

// Extract the container's whole filesystem as if it were a single layer from current builder instance
func (b *Builder) ExtractRootfs(options CommitOptions, opts ExtractRootfsOptions) (io.ReadCloser, chan error, error) {
	src, err := b.makeContainerImageRef(options)
	if err != nil {
		return nil, nil, fmt.Errorf("creating image reference for container %q to extract its contents: %w", b.ContainerID, err)
	}
	return src.extractRootfs(opts)
}
