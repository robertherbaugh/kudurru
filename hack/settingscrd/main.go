// settingscrd adds generated public cluster settings to the Kudurru CRD.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/robertherbaugh/kudurru/internal/config/crdschema"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func main() {
	path := flag.String("file", "config/crd/bases/kudurru.io_kudurrus.yaml", "generated Kudurru CRD")
	flag.Parse()
	if err := run(*path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(path string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // generator's output argument
	if err != nil {
		return err
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(raw, &crd); err != nil {
		return err
	}
	if err := crdschema.Populate(&crd); err != nil {
		return err
	}
	raw, err = yaml.Marshal(crd)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}
