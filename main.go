package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/reference/docker"

	"go.cuelabs.dev/ociregistry"
	"go.cuelabs.dev/ociregistry/ociserver"
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
		ref, err := docker.ParseNormalizedNamed(image.Name)
		if err != nil {
			// just ignore images whose names we can't parse (TODO debug log?)
			continue
		}
		repo := ref.Name()
		if len(names) > 0 && names[len(names)-1] == repo {
			// "List" returns sorted order, so we only need to check the last item in the list to dedupe
			continue
		}
		names = append(names, repo)
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
	// TODO convert not found into proper 404 errors
	return newContainerdBlobReaderFromDigest(ctx, r.client, digest)
}

func (r containerdRegistry) GetBlobRange(ctx context.Context, repo string, digest ociregistry.Digest, offset0, offset1 int64) (ociregistry.BlobReader, error) {
	br, err := newContainerdBlobReaderFromDigest(ctx, r.client, digest)
	if err != nil {
		// TODO convert not found into proper 404 errors
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
		return nil, err
	}
	defer ra.Close()

	desc.Size = ra.Size()

	// wrap in a LimitedReader here to make sure we don't read an enormous amount of valid but useless JSON that DoS's us
	reader := io.LimitReader(content.NewReader(ra), 4*1024*1024)
	// 4MiB: https://github.com/opencontainers/distribution-spec/pull/293, especially https://github.com/opencontainers/distribution-spec/pull/293#issuecomment-1452780554

	mediaTypeWrapper := struct {
		MediaType string `json:"mediaType"`
	}{}
	if err := json.NewDecoder(reader).Decode(&mediaTypeWrapper); err != nil {
		return nil, err
	}
	if mediaTypeWrapper.MediaType == "" {
		return nil, errors.New("failed to parse mediaType") // TODO better error
	}
	desc.MediaType = mediaTypeWrapper.MediaType

	return newContainerdBlobReaderFromDescriptor(ctx, r.client, desc)
	// TODO convert not found into proper 404 errors
}

func (r containerdRegistry) GetTag(ctx context.Context, repo string, tagName string) (ociregistry.BlobReader, error) {
	is := r.client.ImageService()

	img, err := is.Get(ctx, repo+":"+tagName)
	if err != nil {
		return nil, err
	}

	return &containerdBlobReader{
		client: r.client,
		ctx:    ctx,
		desc:   img.Target,
	}, nil
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

// TODO func (r containerdRegistry) PushBlob(ctx context.Context, repo string, desc ociregistry.Descriptor, r io.Reader) (ociregistry.Descriptor, error)

// TODO func (r containerdRegistry) PushBlobChunked(ctx context.Context, repo string, id string, chunkSize int) (ociregistry.BlobWriter, error)

func (r containerdRegistry) MountBlob(ctx context.Context, fromRepo, toRepo string, digest ociregistry.Digest) (ociregistry.Descriptor, error) {
	// since we don't do per-repo blobs, this just succeeds
	return r.ResolveBlob(ctx, toRepo, digest)
}

// TODO func (r containerdRegistry) PushManifest(ctx context.Context, repo string, tag string, contents []byte, mediaType string) (ociregistry.Descriptor, error)

func main() {
	containerdAddr := defaults.DefaultAddress
	if val, ok := os.LookupEnv("CONTAINERD_ADDRESS"); ok {
		containerdAddr = val
	}
	containerdNamespace := "default"
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
