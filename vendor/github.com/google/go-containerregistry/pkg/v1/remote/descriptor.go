// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remote

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

var defaultPlatform = v1.Platform{
	Architecture: "amd64",
	OS:           "linux",
}

// ErrSchema1 indicates that we received a schema1 manifest from the registry.
// This library doesn't have plans to support this legacy image format:
// https://github.com/google/go-containerregistry/issues/377
var ErrSchema1 = errors.New("unsupported MediaType: https://github.com/google/go-containerregistry/issues/377")

// Descriptor provides access to metadata about remote artifact and accessors
// for efficiently converting it into a v1.Image or v1.ImageIndex.
type Descriptor struct {
	fetcher
	v1.Descriptor
	Manifest []byte

	// So we can share this implementation with Image..
	platform v1.Platform
}

type imageOpener struct {
	auth      authn.Authenticator
	transport http.RoundTripper
	ref       name.Reference
	platform  v1.Platform
}

// Get returns a remote.Descriptor for the given reference. The response from
// the registry is left un-interpreted, for the most part. This is useful for
// querying what kind of artifact a reference represents.
func Get(ref name.Reference, options ...ImageOption) (*Descriptor, error) {
	acceptable := []types.MediaType{
		types.DockerManifestSchema2,
		types.OCIManifestSchema1,
		types.DockerManifestList,
		types.OCIImageIndex,
		// Just to look at them.
		types.DockerManifestSchema1,
		types.DockerManifestSchema1Signed,
	}
	return get(ref, acceptable, options...)
}

// Handle options and fetch the manifest with the acceptable MediaTypes in the
// Accept header.
func get(ref name.Reference, acceptable []types.MediaType, options ...ImageOption) (*Descriptor, error) {
	i := &imageOpener{
		auth:      authn.Anonymous,
		transport: http.DefaultTransport,
		ref:       ref,
		platform:  defaultPlatform,
	}

	for _, option := range options {
		if err := option(i); err != nil {
			return nil, err
		}
	}
	tr, err := transport.New(i.ref.Context().Registry, i.auth, i.transport, []string{i.ref.Scope(transport.PullScope)})
	if err != nil {
		return nil, err
	}

	f := fetcher{
		Ref:    i.ref,
		Client: &http.Client{Transport: tr},
	}

	b, desc, err := f.fetchManifest(ref, acceptable)
	if err != nil {
		return nil, err
	}

	return &Descriptor{
		fetcher:    f,
		Manifest:   b,
		Descriptor: *desc,
		platform:   i.platform,
	}, nil
}

// Image converts the Descriptor into a v1.Image.
//
// If the fetched artifact is already an image, it will just return it.
//
// If the fetched artifact is an index, it will attempt to resolve the index to
// a child image with the appropriate platform.
//
// See WithPlatform to set the desired platform.
func (d *Descriptor) Image() (v1.Image, error) {
	switch d.MediaType {
	case types.DockerManifestSchema1, types.DockerManifestSchema1Signed:
		// We don't care to support schema 1 images:
		// https://github.com/google/go-containerregistry/issues/377
		return nil, ErrSchema1
	case types.OCIImageIndex, types.DockerManifestList:
		// We want an image but the registry has an index, resolve it to an image.
		return d.remoteIndex().imageByPlatform(d.platform)
	case types.OCIManifestSchema1, types.DockerManifestSchema2:
		// These are expected. Enumerated here to allow a default case.
	default:
		// We could just return an error here, but some registries (e.g. static
		// registries) don't set the Content-Type headers correctly, so instead...
		// TODO(#390): Log a warning.
	}

	// Wrap the v1.Layers returned by this v1.Image in a hint for downstream
	// remote.Write calls to facilitate cross-repo "mounting".
	imgCore, err := partial.CompressedToImage(d.remoteImage())
	if err != nil {
		return nil, err
	}
	return &mountableImage{
		Image:     imgCore,
		Reference: d.Ref,
	}, nil
}

// ImageIndex converts the Descriptor into a v1.ImageIndex.
func (d *Descriptor) ImageIndex() (v1.ImageIndex, error) {
	switch d.MediaType {
	case types.DockerManifestSchema1, types.DockerManifestSchema1Signed:
		// We don't care to support schema 1 images:
		// https://github.com/google/go-containerregistry/issues/377
		return nil, ErrSchema1
	case types.OCIManifestSchema1, types.DockerManifestSchema2:
		// We want an index but the registry has an image, nothing we can do.
		return nil, fmt.Errorf("unexpected media type for ImageIndex(): %s; call Image() instead", d.MediaType)
	case types.OCIImageIndex, types.DockerManifestList:
		// These are expected.
	default:
		// We could just return an error here, but some registries (e.g. static
		// registries) don't set the Content-Type headers correctly, so instead...
		// TODO(#390): Log a warning.
	}
	return d.remoteIndex(), nil
}

func (d *Descriptor) remoteImage() *remoteImage {
	return &remoteImage{
		fetcher: fetcher{
			Ref:    d.Ref,
			Client: d.Client,
		},
		manifest:  d.Manifest,
		mediaType: d.MediaType,
	}
}

func (d *Descriptor) remoteIndex() *remoteIndex {
	return &remoteIndex{
		fetcher: fetcher{
			Ref:    d.Ref,
			Client: d.Client,
		},
		manifest:  d.Manifest,
		mediaType: d.MediaType,
	}
}

// fetcher implements methods for reading from a registry.
type fetcher struct {
	Ref    name.Reference
	Client *http.Client
}

// url returns a url.Url for the specified path in the context of this remote image reference.
func (f *fetcher) url(resource, identifier string) url.URL {
	return url.URL{
		Scheme: f.Ref.Context().Registry.Scheme(),
		Host:   f.Ref.Context().RegistryStr(),
		Path:   fmt.Sprintf("/v2/%s/%s/%s", f.Ref.Context().RepositoryStr(), resource, identifier),
	}
}

func (f *fetcher) fetchManifest(ref name.Reference, acceptable []types.MediaType) ([]byte, *v1.Descriptor, error) {
	u := f.url("manifests", ref.Identifier())
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	accept := []string{}
	for _, mt := range acceptable {
		accept = append(accept, string(mt))
	}
	req.Header.Set("Accept", strings.Join(accept, ","))

	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if err := transport.CheckError(resp, http.StatusOK); err != nil {
		return nil, nil, err
	}

	manifest, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	digest, size, err := v1.SHA256(bytes.NewReader(manifest))
	if err != nil {
		return nil, nil, err
	}

	mediaType := types.MediaType(resp.Header.Get("Content-Type"))

	// Validate the digest matches what we asked for, if pulling by digest.
	if dgst, ok := ref.(name.Digest); ok {
		if mediaType == types.DockerManifestSchema1Signed {
			// Digests for this are stupid to calculate, ignore it.
		} else if digest.String() != dgst.DigestStr() {
			return nil, nil, fmt.Errorf("manifest digest: %q does not match requested digest: %q for %q", digest, dgst.DigestStr(), f.Ref)
		}
	} else {
		// Do nothing for tags; I give up.
		//
		// We'd like to validate that the "Docker-Content-Digest" header matches what is returned by the registry,
		// but so many registries implement this incorrectly that it's not worth checking.
		//
		// For reference:
		// https://github.com/docker/distribution/issues/2395
		// https://github.com/GoogleContainerTools/kaniko/issues/298
	}

	// Return all this info since we have to calculate it anyway.
	desc := v1.Descriptor{
		Digest:    digest,
		Size:      size,
		MediaType: mediaType,
	}

	return manifest, &desc, nil
}
