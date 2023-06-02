package main

import (
	"context"
	"net/http"
	"regexp"
	"strconv"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/reference/docker"

	"github.com/rogpeppe/ociregistry"
	"github.com/rogpeppe/ociregistry/ociserver"
)

// caller responsible for client.Close!
func newContainerdClient() (*containerd.Client, error) {
	// TODO environment variables (CONTAINERD_ADDRESS, CONTAINERD_NAMESPACE)
	return containerd.New(
		"/run/containerd/containerd.sock",
		containerd.WithDefaultNamespace("default"),
	)
}

type containerdRegistry struct {
	*ociregistry.Funcs
}

func (_ containerdRegistry) Repositories(ctx context.Context) ociregistry.Iter[string] {
	client, err := newContainerdClient()
	if err != nil {
		return ociregistry.ErrorIter[string](err)
	}
	defer client.Close()

	is := client.ImageService()

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

func (_ containerdRegistry) Tags(ctx context.Context, repo string) ociregistry.Iter[string] {
	client, err := newContainerdClient()
	if err != nil {
		return ociregistry.ErrorIter[string](err)
	}
	defer client.Close()

	is := client.ImageService()

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

func main() {
	registry := containerdRegistry{}
	server := ociserver.New(&registry, &ociserver.Options{})
	println("listening on http://*:5000")
	panic(http.ListenAndServe(":5000", server))
}
