// Copyright 2021 Upbound Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xpkg

import (
	"archive/tar"
	"bytes"
	"io/ioutil"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/pkg/errors"
	"github.com/spf13/afero"
	"github.com/spf13/afero/tarfs"

	"github.com/upbound/up/internal/credhelper"
	"github.com/upbound/up/internal/xpkg"
)

const (
	errGetwd           = "failed to get working directory while searching for package"
	errFindPackageinWd = "failed to find a package in current working directory"
	errBuildImage      = "failed to build image from layers"
)

const upboundRegistry = "registry.upbound.io"

// AfterApply constructs and binds Upbound-specific context to any subcommands
// that have Run() methods that receive it.
func (c *pushCmd) AfterApply() error {
	c.fs = afero.NewOsFs()
	return nil
}

// pushCmd pushes a package.
type pushCmd struct {
	fs afero.Fs

	Tag     string `arg:"" help:"Tag of the package to be pushed. Must be a valid OCI image tag."`
	Package string `short:"f" help:"Path to package. If not specified and only one package exists in current directory it will be used."`
	Profile string `env:"UP_PROFILE" help:"Profile used to execute command."`
}

// Run runs the push cmd.
func (c *pushCmd) Run() error {
	tag, err := name.NewTag(c.Tag, name.WithDefaultRegistry(upboundRegistry))
	if err != nil {
		return err
	}

	// If package is not defined, attempt to find single package in current
	// directory.
	if c.Package == "" {
		wd, err := os.Getwd()
		if err != nil {
			return errors.Wrap(err, errGetwd)
		}
		path, err := xpkg.FindXpkgInDir(c.fs, wd)
		if err != nil {
			return errors.Wrap(err, errFindPackageinWd)
		}
		c.Package = path
	}
	img, err := tarball.ImageFromPath(c.Package, nil)
	if err != nil {
		return err
	}

	// annotate image layers
	aimg, err := annotate(img)
	if err != nil {
		return err
	}

	if err := remote.Write(tag, aimg, remote.WithAuthFromKeychain(
		authn.NewMultiKeychain(
			authn.NewKeychainFromHelper(
				credhelper.New(credhelper.WithProfile(c.Profile)),
			),
			authn.DefaultKeychain,
		),
	)); err != nil {
		return err
	}
	return nil
}

// annotate reads in the layers of the given v1.Image and annotates the xpkg
// layers with their corresponding annotations, returning a new v1.Image
// containing the annotation details.
func annotate(i v1.Image) (v1.Image, error) { //nolint:gocyclo
	reader := mutate.Extract(i)
	fs := tarfs.New(tar.NewReader(reader))
	pkgYaml, err := fs.Open(xpkg.StreamFile)
	if err != nil {
		return nil, err
	}

	pkgBytes, err := ioutil.ReadAll(pkgYaml)
	if err != nil {
		return nil, err
	}

	addendums := []mutate.Addendum{}

	pkgAdd, err := xpkg.PackageAddendum(bytes.NewBuffer(pkgBytes))
	if err != nil {
		return nil, err
	}
	addendums = append(addendums, pkgAdd)

	exYaml, err := fs.Open(xpkg.XpkgExamplesFile)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	if exYaml != nil {
		exBytes, err := ioutil.ReadAll(exYaml)
		if err != nil {
			return nil, err
		}
		exAdd, err := xpkg.ExamplesAddendum(bytes.NewBuffer(exBytes))
		if err != nil {
			return nil, err
		}
		addendums = append(addendums, exAdd)
	}

	img := empty.Image
	for _, a := range addendums {
		img, err = mutate.Append(img, a)
		if err != nil {
			return nil, errors.Wrap(err, errBuildImage)
		}
	}
	return img, nil
}
