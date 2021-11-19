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

package dep

import (
	encjson "encoding/json"
	"os"
	"path/filepath"

	"github.com/pkg/errors"

	"github.com/spf13/afero"
	sigsyaml "sigs.k8s.io/yaml"

	apimetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	v1 "github.com/crossplane/crossplane/apis/pkg/meta/v1"
	"github.com/crossplane/crossplane/apis/pkg/v1beta1"

	"github.com/upbound/up/internal/xpkg"
)

const (
	errInvalidMetaFile      = "invalid meta type supplied"
	errMetaFileDoesNotExist = "meta file does not exist"
	errMetaContainsDupeDep  = "meta file contains duplicate dependency"
)

// Workspace defines our view of the current directory
type Workspace struct {
	fs             afero.Fs
	metaFileExists bool
	wd             WorkingDirFn
	root           string
}

// NewWorkspace establishees a workspace for acting on current ws entries
func NewWorkspace(opts ...WSOption) (*Workspace, error) {
	ws := &Workspace{
		fs: afero.NewOsFs(),
		wd: os.Getwd,
	}

	for _, o := range opts {
		o(ws)
	}

	wd, err := ws.wd()
	if err != nil {
		return nil, err
	}

	ws.root = wd

	return ws, nil
}

// WorkingDirFn indicates the location of the working directory.
type WorkingDirFn func() (string, error)

// WSOption is used to modify a Workspace.
type WSOption func(*Workspace)

// WithFS configures the workspace with the given filesystem.
func WithFS(fs afero.Fs) WSOption {
	return func(ws *Workspace) {
		ws.fs = fs
	}
}

// Init initializes the workspace
func (w *Workspace) Init() error {
	exists, err := afero.Exists(w.fs, filepath.Join(w.root, xpkg.MetaFile))
	if err != nil {
		return errors.Wrap(err, errMetaFileDoesNotExist)
	}

	w.metaFileExists = exists

	return nil
}

// MetaExists returns true if a meta file exists in the workspace
func (w *Workspace) MetaExists() bool {
	return w.metaFileExists
}

// Upsert will add an entry to the meta file, if the meta file exists and
// does not yet have an entry for the given package. If an entry does exist,
// the entry will be updated to the given package version.
func (w *Workspace) Upsert(d v1beta1.Dependency) error {
	p, err := w.readPkgMeta()
	if err != nil {
		return err
	}

	err = upsertDeps(d, p)
	if err != nil {
		return err
	}

	return w.writeMetaPkg(p)
}

func upsertDeps(d v1beta1.Dependency, p v1.Pkg) error { // nolint:gocyclo
	deps := p.GetDependencies()

	processed := false
	for i := range deps {
		// modify the underlying slice
		dep := deps[i]
		if dep.Provider != nil && *dep.Provider == d.Package {
			if processed {
				return errors.New(errMetaContainsDupeDep)
			}
			deps[i].Version = d.Constraints
			processed = true
		} else if dep.Configuration != nil && *dep.Configuration == d.Package {
			if processed {
				return errors.New(errMetaContainsDupeDep)
			}
			deps[i].Version = d.Constraints
			processed = true
		}
	}

	if !processed {

		dep := v1.Dependency{
			Version: d.Constraints,
		}

		if d.Type == v1beta1.ProviderPackageType {
			dep.Provider = &d.Package
		} else {
			dep.Configuration = &d.Package
		}

		deps = append(deps, dep)
	}

	switch v := p.(type) {
	case *v1.Configuration:
		v.Spec.DependsOn = deps
	case *v1.Provider:
		v.Spec.DependsOn = deps
	}

	return nil
}

func (w *Workspace) readPkgMeta() (v1.Pkg, error) {

	b, err := afero.ReadFile(w.fs, filepath.Join(w.root, xpkg.MetaFile))
	if err != nil && os.IsNotExist(err) {
		return nil, errors.Wrap(err, errMetaFileDoesNotExist)
	}

	unmarshalFns := []packageUnmarshal{
		configUnmarshal,
		providerUnmarshal,
	}

	var p interface{}

	for _, u := range unmarshalFns {
		pkg, ok, err := u(b)
		if !ok && err != nil {
			return nil, err
		}
		if ok {
			p = pkg
			break
		}
	}

	return p.(v1.Pkg), nil
}

type packageUnmarshal func(b []byte) (interface{}, bool, error)

func configUnmarshal(b []byte) (interface{}, bool, error) {
	var c v1.Configuration

	err := yaml.Unmarshal(b, &c)
	if err != nil {
		return nil, false, err
	}

	// we were able to unmarshal but we didn't get the expected type,
	// return ok = false
	if c.Kind != v1.ConfigurationKind {
		return nil, false, nil
	}
	return &c, true, nil
}

func providerUnmarshal(b []byte) (interface{}, bool, error) {
	var p v1.Provider
	err := yaml.Unmarshal(b, &p)
	if err != nil {
		return nil, false, err
	}

	// we were able to unmarshal but we didn't get the expected type,
	// return ok = false
	if p.Kind != v1.ProviderKind {
		return nil, false, nil
	}

	return &p, true, nil
}

// writeMetaPkg writes to the current meta file (crossplane.yaml).
// If the file currently exists, it will be overwritten rather than
// appended to.
func (w *Workspace) writeMetaPkg(p v1.Pkg) error {
	data, err := sigsyaml.Marshal(p)
	if err != nil {
		return err
	}

	// (@tnthornton) workaround for `creationTimestamp: null` in marshaled result.
	// see https://github.com/kubernetes/kubernetes/pull/104857 for inspiration

	t := apimetav1.Time{}

	switch v := p.(type) {
	case *v1.Configuration:
		t = v.GetCreationTimestamp()
	case *v1.Provider:
		t = v.GetCreationTimestamp()
	default:
		return errors.New(errInvalidMetaFile)
	}

	if t.Equal(&apimetav1.Time{}) {
		// the timestamp is empty, we need to clean it from the resulting
		// file data
		data, err = cleanNullTs(p)
		if err != nil {
			return err
		}
	}

	return afero.WriteFile(w.fs, filepath.Join(w.root, xpkg.MetaFile), data, os.ModePerm)
}

// cleanNullTs is a helper function for cleaning the erroneous
// `creationTimestamp: null` from the marshaled data that we're
// going to writer to the meta file.
func cleanNullTs(p v1.Pkg) ([]byte, error) {
	ob, err := encjson.Marshal(p)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	err = encjson.Unmarshal(ob, &m)
	if err != nil {
		return nil, err
	}
	// remove the erroneous creationTimestamp: null entry
	delete(m["metadata"].(map[string]interface{}), "creationTimestamp")

	return sigsyaml.Marshal(m)
}
