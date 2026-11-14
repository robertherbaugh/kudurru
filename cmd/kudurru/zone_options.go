package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

const zoneOptionsMaxBytes = 1 << 20

type zoneOptionsDocument struct {
	ResourceVersion string          `json:"resourceVersion"`
	Spec            json.RawMessage `json:"spec"`
}

func readZoneOptions(input io.Reader) (zoneOptionsDocument, error) {
	var doc zoneOptionsDocument
	body, err := io.ReadAll(io.LimitReader(input, zoneOptionsMaxBytes+1))
	if err != nil {
		return doc, err
	}
	if len(body) > zoneOptionsMaxBytes {
		return doc, errors.New("zone options exceed 1 MiB")
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(body)))
	first, err := reader.Read()
	if err != nil {
		return doc, errors.New("zone options require one YAML or JSON document")
	}
	if _, err := reader.Read(); err != io.EOF {
		return doc, errors.New("zone options require exactly one document")
	}
	encoded, err := yaml.YAMLToJSONStrict(first)
	if err != nil {
		return doc, errors.New("zone options contain invalid or duplicate fields")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return doc, errors.New("zone options require only resourceVersion and spec")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return doc, errors.New("zone options require exactly one document")
	}
	if doc.ResourceVersion == "" || strings.TrimSpace(doc.ResourceVersion) != doc.ResourceVersion {
		return doc, errors.New("zone options require the reviewed resourceVersion; use zones options get")
	}
	var spec *public.DNSZoneSpec
	decoder = json.NewDecoder(bytes.NewReader(doc.Spec))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil || spec == nil {
		return doc, errors.New("zone options spec must be a DNSZone specification with known fields")
	}
	return doc, nil
}

func zoneOptionsFromObject(raw json.RawMessage) (zoneOptionsDocument, error) {
	var obj struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Metadata.ResourceVersion == "" || len(obj.Spec) == 0 {
		return zoneOptionsDocument{}, errors.New("server returned incomplete zone options")
	}
	return zoneOptionsDocument{ResourceVersion: obj.Metadata.ResourceVersion, Spec: obj.Spec}, nil
}

func renderZoneOptions(o *cliOpts, cmd *cobra.Command, doc zoneOptionsDocument) error {
	return o.render(cmd, doc, func(bool) (*cli.Table, error) {
		table := &cli.Table{Headers: []string{"OPTION", "VALUE"}}
		table.Add("resourceVersion", doc.ResourceVersion)
		var spec map[string]json.RawMessage
		if err := json.Unmarshal(doc.Spec, &spec); err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(spec))
		for key := range spec {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			table.Add(key, string(spec[key]))
		}
		return table, nil
	})
}

func newZoneOptionsCmd(o *cliOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "options", Short: "Review and replace zone options with a resource-version check"}
	get := &cobra.Command{Use: "get <zone>", Short: "Show the zone specification and its reviewed resource version", Args: cobra.ExactArgs(1), ValidArgsFunction: o.completeZones, RunE: func(cmd *cobra.Command, args []string) error {
		c, _, err := o.client()
		if err != nil {
			return err
		}
		raw, err := c.GetObject(cmd.Context(), "dnszones", "", args[0])
		if err != nil {
			return err
		}
		doc, err := zoneOptionsFromObject(raw)
		if err != nil {
			return err
		}
		return renderZoneOptions(o, cmd, doc)
	}}
	var file string
	set := &cobra.Command{Use: "set <zone> -f options.yaml", Short: "Replace the complete zone specification from a reviewed options document", Long: `Replace the complete spec using the resourceVersion emitted by zones options get.
Metadata and status are preserved. A concurrent change, missing permission or
another manager's ownership is refused; this command never retries the write.
Use zones options get -o yaml, edit spec, then pass that document with -f.`, Args: cobra.ExactArgs(1), ValidArgsFunction: o.completeZones, RunE: func(cmd *cobra.Command, args []string) error {
		if file == "" {
			return errors.New("zone options set requires -f file|-")
		}
		input := o.stdin
		if file != "-" {
			// #nosec G304 -- the local CLI operator explicitly selects this input file.
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			input = f
		}
		doc, err := readZoneOptions(input)
		if err != nil {
			return err
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		raw, err := c.GetObject(cmd.Context(), "dnszones", "", args[0])
		if err != nil {
			return err
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(object["metadata"], &metadata); err != nil || metadata == nil {
			return errors.New("server returned no zone metadata")
		}
		metadata["resourceVersion"], err = json.Marshal(doc.ResourceVersion)
		if err != nil {
			return err
		}
		object["metadata"], err = json.Marshal(metadata)
		if err != nil {
			return err
		}
		object["spec"] = doc.Spec
		saved, err := c.PutObject(cmd.Context(), "dnszones", "", args[0], doc.ResourceVersion, object)
		if err != nil {
			return fmt.Errorf("zone options were not saved: %w", err)
		}
		current, err := zoneOptionsFromObject(saved)
		if err != nil {
			return err
		}
		return renderZoneOptions(o, cmd, current)
	}}
	set.Flags().StringVarP(&file, "file", "f", "", "reviewed YAML/JSON options document, or - for stdin")
	cmd.AddCommand(get, set)
	return cmd
}
