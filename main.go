package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"cuelabs.dev/go/oci/ociregistry"
	"cuelabs.dev/go/oci/ociregistry/ociserver"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/reference/docker"
	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
)

const (
	blobLeaseExpiration = 15 * time.Minute // TODO make this period configurable?

	// 4MiB: https://github.com/opencontainers/distribution-spec/pull/293, especially https://github.com/opencontainers/distribution-spec/pull/293#issuecomment-1452780554
	manifestSizeLimit = 4 * 1024 * 1024
)

type containerdRegistry struct {
	*ociregistry.Funcs
	client *containerd.Client
}

func (r containerdRegistry) Repositories(ctx context.Context) ociregistry.Iter[string] {
	is := r.client.ImageService()

	images, err := is.List(ctx)
	if err != nil {
		return ociregistry.ErrorIter[string](err)
	}

	names := []string{}
	for _, image := range images {
		// image.Name is a fully qualified name like "repo:tag" or "repo@digest" so we need to parse it so we can return just the repo name list
		ref, err := docker.Parse(image.Name)
		if err != nil {
			// just ignore images whose names we can't parse (TODO debug log?)
			continue
		}
		if named, ok := ref.(docker.Named); !ok {
			// TODO debug log?
			continue
		} else {
			repo := named.Name()
			if len(names) > 0 && names[len(names)-1] == repo {
				// "List" returns sorted order, so we only need to check the last item in the list to dedupe
				continue
			}
			names = append(names, repo)
		}
	}

	return ociregistry.SliceIter[string](names)
}

func (r containerdRegistry) Tags(ctx context.Context, repo string) ociregistry.Iter[string] {
	is := r.client.ImageService()

	images, err := is.List(ctx, "name~="+strconv.Quote("^"+regexp.QuoteMeta(repo)+":"))
	if err != nil {
		return ociregistry.ErrorIter[string](err)
	}

	tags := []string{}
	for _, image := range images {
		// image.Name is a fully qualified name like "repo:tag" or "repo@digest" so we need to parse it so we can return just the tags
		ref, err := docker.Parse(image.Name)
		if err != nil {
			// just ignore images whose names we can't parse (TODO debug log?)
			continue
		}
		// TODO do we trust the filter we provided to List(), or do we verify that ref is a ref.Named _and_ that Name() == repo?
		if _, ok := ref.(docker.Digested); ok {
			// ignore "digested" references (foo:bar@baz)
			continue
		}
		if tagged, ok := ref.(docker.Tagged); ok {
			tags = append(tags, tagged.Tag())
		}
	}

	return ociregistry.SliceIter[string](tags)
}

type containerdBlobReader struct {
	client *containerd.Client
	ctx    context.Context
	desc   ociregistry.Descriptor

	readerAt content.ReaderAt
	reader   io.Reader
}

func (br *containerdBlobReader) validate() error {
	info, err := br.client.ContentStore().Info(br.ctx, br.desc.Digest)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return ociregistry.ErrBlobUnknown
		}
		return err
	}
	// add Size/MediaType to our descriptor for poor ociregistry's sake (Content-Length/Content-Type headers)
	if br.desc.Size == 0 && info.Size != 0 {
		br.desc.Size = info.Size
	}
	if br.desc.MediaType == "" {
		br.desc.MediaType = "application/octet-stream"
	}
	return nil
}

func (br *containerdBlobReader) ensureReaderAt() (content.ReaderAt, error) {
	if br.readerAt == nil {
		var err error
		br.readerAt, err = br.client.ContentStore().ReaderAt(br.ctx, br.desc)
		if err != nil {
			return nil, err
		}
	}
	return br.readerAt, nil
}

func (br *containerdBlobReader) ensureReader() (io.Reader, error) {
	if br.reader == nil {
		ra, err := br.ensureReaderAt()
		if err != nil {
			return nil, err
		}
		br.reader = content.NewReader(ra)
	}
	return br.reader, nil
}

func (br *containerdBlobReader) Read(p []byte) (int, error) {
	r, err := br.ensureReader()
	if err != nil {
		return 0, err
	}
	return r.Read(p)
}

func (br *containerdBlobReader) Descriptor() ociregistry.Descriptor {
	return br.desc
}

func (br *containerdBlobReader) Close() error {
	if br.readerAt != nil {
		return br.readerAt.Close()
	}
	return nil
}

// containerd.Client becomes owned by the returned containerdBlobReader (or Close'd by this method on error)
func newContainerdBlobReaderFromDescriptor(ctx context.Context, client *containerd.Client, desc ociregistry.Descriptor) (*containerdBlobReader, error) {
	br := &containerdBlobReader{
		client: client,
		ctx:    ctx,
		desc:   desc,
	}

	// let's verify the blob exists (and add size to the descriptor, if it's missing)
	if err := br.validate(); err != nil {
		br.Close()
		return nil, err
	}

	return br, nil
}

// containerd.Client becomes owned by the returned containerdBlobReader (or Close'd by this method on error)
func newContainerdBlobReaderFromDigest(ctx context.Context, client *containerd.Client, digest ociregistry.Digest) (*containerdBlobReader, error) {
	return newContainerdBlobReaderFromDescriptor(ctx, client, ociregistry.Descriptor{
		// this is technically not a valid Descriptor, but containerd's content store is addressed by digest so it works fine ("[containerd/content.Provider.]ReaderAt only requires desc.Digest to be set.")
		Digest: digest,
	})
}

func (r containerdRegistry) GetBlob(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	return newContainerdBlobReaderFromDigest(ctx, r.client, digest)
}

func (r containerdRegistry) GetBlobRange(ctx context.Context, repo string, digest ociregistry.Digest, offset0, offset1 int64) (ociregistry.BlobReader, error) {
	br, err := newContainerdBlobReaderFromDigest(ctx, r.client, digest)
	if err != nil {
		return nil, err
	}

	requestedSize := offset1 - offset0
	if offset1 < 0 || offset0+requestedSize > br.desc.Size {
		// "If offset1 is negative or exceeds the actual size of the blob, GetBlobRange will return all the data starting from offset0."
		return br, nil
	}

	ra, err := br.ensureReaderAt()
	if err != nil {
		br.Close()
		return nil, err
	}

	// hack hack hack
	br.reader = io.NewSectionReader(ra, offset0, requestedSize)

	return br, nil
}

func (r containerdRegistry) GetManifest(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	// we can technically just return the manifest directly from the content store, but we need the "right" MediaType value for the Content-Type header (and thanks to https://github.com/opencontainers/image-spec/security/advisories/GHSA-77vh-xpmg-72qh we can safely assume manifests have "mediaType" set for us to parse this value out of or else they're manifests we don't care to support!)
	desc := ociregistry.Descriptor{Digest: digest}

	ra, err := r.client.ContentStore().ReaderAt(ctx, desc)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, ociregistry.ErrManifestUnknown
		}
		return nil, err
	}
	defer ra.Close()

	desc.Size = ra.Size()

	// wrap in a LimitedReader here to make sure we don't read an enormous amount of valid but useless JSON that DoS's us
	reader := io.LimitReader(content.NewReader(ra), manifestSizeLimit)

	mediaTypeWrapper := struct {
		MediaType string `json:"mediaType"`
	}{}
	if err := json.NewDecoder(reader).Decode(&mediaTypeWrapper); err != nil {
		return nil, err
	}
	if mediaTypeWrapper.MediaType == "" {
		return nil, errors.New("failed to parse mediaType from " + string(desc.Digest) + " (for Content-Type header)")
	}
	desc.MediaType = mediaTypeWrapper.MediaType

	br, err := newContainerdBlobReaderFromDescriptor(ctx, r.client, desc)
	if err == ociregistry.ErrBlobUnknown {
		return nil, ociregistry.ErrManifestUnknown
	}
	return br, err
}

func (r containerdRegistry) GetTag(ctx context.Context, repo string, tagName string) (ociregistry.BlobReader, error) {
	is := r.client.ImageService()

	img, err := is.Get(ctx, repo+":"+tagName)
	if err != nil {
		if errdefs.IsNotFound(err) {
			if repo == "sha256" && len(tagName) == 64 {
				// this might be a digest, so let's be cute and allow repo-less "registry/sha256:xxx" references, if they're valid
				if digest, err := digest.Parse(repo + ":" + tagName); err == nil {
					if br, err := r.GetManifest(ctx, repo, digest); err == nil {
						return br, nil
					}
				}
			}

			// TODO differentiate ErrNameUnknown (repo unknown) from ErrManifestUnknown ?
			return nil, ociregistry.ErrManifestUnknown
		}
		return nil, err
	}

	return newContainerdBlobReaderFromDescriptor(ctx, r.client, img.Target)
}

func (r containerdRegistry) ResolveBlob(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.Descriptor, error) {
	blobReader, err := r.GetBlob(ctx, repo, digest)
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	defer blobReader.Close()

	return blobReader.Descriptor(), nil
}

func (r containerdRegistry) ResolveManifest(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.Descriptor, error) {
	blobReader, err := r.GetManifest(ctx, repo, digest)
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	defer blobReader.Close()

	return blobReader.Descriptor(), nil
}

func (r containerdRegistry) ResolveTag(ctx context.Context, repo string, tagName string) (ociregistry.Descriptor, error) {
	blobReader, err := r.GetTag(ctx, repo, tagName)
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	defer blobReader.Close()

	return blobReader.Descriptor(), nil
}

func (r containerdRegistry) DeleteBlob(ctx context.Context, repo string, digest ociregistry.Digest) error {
	// TODO should we stop this from removing things that are still tagged or children of tagged?
	return r.client.ContentStore().Delete(ctx, digest)
}

func (r containerdRegistry) DeleteManifest(ctx context.Context, repo string, digest ociregistry.Digest) error {
	// TODO should we stop this from removing things that are still tagged or children of tagged?
	return r.client.ContentStore().Delete(ctx, digest)
}

func (r containerdRegistry) DeleteTag(ctx context.Context, repo string, name string) error {
	return r.client.ImageService().Delete(ctx, repo+":"+name)
}

func (r containerdRegistry) PushBlob(ctx context.Context, repo string, desc ociregistry.Descriptor, reader io.Reader) (ociregistry.Descriptor, error) {
	cs := r.client.ContentStore()

	// since we don't know how soon this blob might be part of a tagged manifest (if ever), add an expiring lease so we have time to get to it being tagged before gc takes it
	ctx, deleteLease, err := r.client.WithLease(ctx, leases.WithExpiration(blobLeaseExpiration))
	if err != nil {
		return ociregistry.Descriptor{}, err
	}

	// WriteBlob does *not* limit reads to the provided size, so let's wrap ourselves in a LimitedReader to prevent reading (much) more than we intend
	reader = io.LimitReader(
		reader,
		desc.Size+1, // +1 to allow WriteBlob to detect if it reads too much
	)

	ingestRef := string(desc.Digest)

	// explicitly "abort" the ref we're about to use in case there's a partial or failed ingest already (which content.WriteBlob will then quietly reuse, over and over)
	_ = cs.Abort(ctx, ingestRef)

	if err := content.WriteBlob(ctx, cs, ingestRef, reader, desc); err != nil {
		_ = cs.Abort(ctx, ingestRef)
		_ = deleteLease(ctx)
		return ociregistry.Descriptor{}, err
	}

	return desc, nil
}

type containerdBlobWriter struct {
	ctx context.Context
	cs  content.Store
	id  string
	content.Writer

	closedStatus *content.Status
}

func (bw *containerdBlobWriter) cacheStatus() error {
	// if the writer is closed, Status() fails with "error getting writer status: SendMsg called after CloseSend: unknown", but Size() might still be invoked, so we need to cache that final result *before* we close our Writer
	status, err := bw.Writer.Status()
	if err == nil {
		bw.closedStatus = &status
	}
	return err
}

func (bw *containerdBlobWriter) Close() error {
	return errors.Join(bw.cacheStatus(), bw.Writer.Close())
}

func (bw *containerdBlobWriter) Size() int64 {
	if bw.closedStatus != nil {
		return bw.closedStatus.Offset
	}
	status, err := bw.Writer.Status()
	if err != nil {
		log.Panic(err)
		return -1 // TODO something better 😭
	}
	return status.Offset
}

func (bw *containerdBlobWriter) ID() string {
	return bw.id
}

func (bw *containerdBlobWriter) Commit(digest ociregistry.Digest) (ociregistry.Descriptor, error) {
	// Commit implies Close (and thus invalidates our Writer in the same way)
	if err := bw.cacheStatus(); err != nil {
		return ociregistry.Descriptor{}, err
	}
	if err := bw.Writer.Commit(bw.ctx, 0, digest); err != nil {
		return ociregistry.Descriptor{}, err
	}
	return ociregistry.Descriptor{
		Digest:    digest,
		Size:      bw.Size(),
		MediaType: "application/octet-stream", // 🙈
	}, nil
}

func (bw *containerdBlobWriter) Cancel() error {
	if err := bw.Close(); err != nil {
		return err
	}
	return bw.cs.Abort(bw.ctx, bw.id)
}

func (r containerdRegistry) PushBlobChunked(ctx context.Context, repo string, id string, chunkSize int) (ociregistry.BlobWriter, error) {
	cs := r.client.ContentStore()

	// if you do not have an id, one will be assigned to you
	if id == "" {
		if uuid, err := uuid.NewRandom(); err != nil {
			return nil, err
		} else {
			id = uuid.String()
		}
	}
	// (this function doesn't "Abort" like PushBlob because being able to resume partial uploads is kind of the whole point)

	// since we don't know how soon this blob might be part of a tagged manifest (if ever), add an expiring lease so we have time to get to it being tagged before gc takes it
	ctx, deleteLease, err := r.client.WithLease(ctx, leases.WithExpiration(blobLeaseExpiration))
	if err != nil {
		return nil, err
	}
	// TODO add labels to leases so we can remove them later if we get something else that references them?

	writer, err := content.OpenWriter(ctx, cs, content.WithRef(id))
	if err != nil {
		_ = deleteLease(ctx)
		return nil, err
	}

	return &containerdBlobWriter{
		ctx:    ctx,
		cs:     cs,
		id:     id,
		Writer: writer,
	}, nil
}

func (r containerdRegistry) MountBlob(ctx context.Context, fromRepo, toRepo string, digest ociregistry.Digest) (ociregistry.Descriptor, error) {
	// since we don't do per-repo blobs, this just succeeds (assuming the requested blob exists)
	return r.ResolveBlob(ctx, toRepo, digest)
}

func (r containerdRegistry) PushManifest(ctx context.Context, repo string, tag string, contents []byte, mediaType string) (ociregistry.Descriptor, error) {
	desc := ociregistry.Descriptor{
		Digest:    digest.FromBytes(contents),
		Size:      int64(len(contents)),
		MediaType: mediaType,
	}

	// see https://github.com/containerd/containerd/blob/f92e576f6b3d4c6505f543967d5caeb3c1a8edc4/docs/content-flow.md, especially the "containerd.io/gc.ref.xxx" labels (which is what we need to apply here), so we need to do some parsing of the manifest
	manifestChildren := struct {
		// manifest, but just the fields that might point at child/related objects we should tag/protect from GC
		// https://github.com/opencontainers/image-spec/blob/9615142d016838b5dfe7453f80af0be74feb5c7c/specs-go/v1/index.go#L27-L28
		Manifests []ociregistry.Descriptor `json:"manifests"`
		// https://github.com/opencontainers/image-spec/blob/9615142d016838b5dfe7453f80af0be74feb5c7c/specs-go/v1/manifest.go#L29-L37
		Config  *ociregistry.Descriptor  `json:"config"`
		Layers  []ociregistry.Descriptor `json:"layers"`
		Subject *ociregistry.Descriptor  `json:"subject"`
	}{}
	if err := json.Unmarshal(contents, &manifestChildren); err != nil {
		return ociregistry.Descriptor{}, err
	}
	labelMappings := map[string]*ociregistry.Descriptor{
		"config":  manifestChildren.Config,
		"subject": manifestChildren.Subject,
	}
	for prefix, list := range map[string][]ociregistry.Descriptor{
		"m": manifestChildren.Manifests,
		"l": manifestChildren.Layers,
	} {
		for i, d := range list {
			d := d
			labelMappings[prefix+"."+strconv.Itoa(i)] = &d
		}
	}
	labels := map[string]string{}
	for field, desc := range labelMappings {
		if desc != nil {
			labels["containerd.io/gc.ref.content."+field] = string(desc.Digest)
		}
	}

	// see PushBlob for commentary on this
	ctx, deleteLease, err := r.client.WithLease(ctx, leases.WithExpiration(blobLeaseExpiration))
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	cs := r.client.ContentStore()
	ingestRef := string(desc.Digest)
	_ = cs.Abort(ctx, ingestRef)
	if err := content.WriteBlob(ctx, cs, ingestRef, bytes.NewReader(contents), desc, content.WithLabels(labels)); err != nil {
		return ociregistry.Descriptor{}, err
	}

	if tag != "" {
		is := r.client.ImageService()
		img := images.Image{
			Name:   repo + ":" + tag,
			Target: desc,
		}
		_, err := is.Update(ctx, img, "target") // "target" here is to specify that we want to update the descriptor that "Name" points to (if this image name already exists)
		if err != nil {
			if !errdefs.IsNotFound(err) {
				return desc, err
			}
			_, err = is.Create(ctx, img)
			if err != nil {
				return desc, err
			}
		}
		// now that we're tagged, we can proactively remove the lease for this manifest
		if err := deleteLease(ctx); err != nil {
			return desc, err
		}
	}

	return desc, nil
}

func main() {
	containerdAddr := defaults.DefaultAddress
	if val, ok := os.LookupEnv("CONTAINERD_ADDRESS"); ok {
		containerdAddr = val
	}
	containerdNamespace := "default" // TODO have a different mode perhaps that prefixes all repository names with a namespace (thus making all the mostly ignored "repo" arguments relevant again); ala "localhost:5000/default/docker.io/tianon/true:oci"
	if val, ok := os.LookupEnv("CONTAINERD_NAMESPACE"); ok {
		containerdNamespace = val
	}
	client, err := containerd.New(
		containerdAddr,
		containerd.WithDefaultNamespace(containerdNamespace),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	server := ociserver.New(&containerdRegistry{
		client: client,
	}, nil)
	println("listening on http://*:5000")
	// TODO listen address/port should be configurable somehow
	log.Fatal(http.ListenAndServe(":5000", server))
}
