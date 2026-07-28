package config

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type LoadOptions struct {
	File       string
	WorkingDir string
	LookupEnv  func(string) (string, bool)
}

type Result struct {
	Config     *Config
	WorkingDir string
	Warnings   []string
}

// Load parses exactly one YAML document, interpolates its string values,
// applies defaults, resolves paths, validates it, and returns a normalized
// configuration. It never creates the working directory or runtime state.
func Load(opts LoadOptions) (*Result, error) {
	if opts.File == "" {
		opts.File = "topology.yml"
	}
	wd, err := ResolveWorkingDir(opts.WorkingDir)
	if err != nil {
		return nil, err
	}
	file, err := filepath.Abs(opts.File)
	if err != nil {
		return nil, fmt.Errorf("resolve topology file: %w", err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read topology file %q: %w", file, err)
	}
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	cfg, warnings, err := Parse(data, filepath.Dir(file), lookup)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if err := validateSocketPaths(wd, cfg); err != nil {
		return nil, err
	}
	if err := validateTemplateFiles(cfg.Templates); err != nil {
		return nil, err
	}
	return &Result{Config: cfg, WorkingDir: wd, Warnings: warnings}, nil
}

func validateTemplateFiles(templates map[string]string) error {
	for name, path := range templates {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("template %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("template %q path is not a regular file: %s", name, path)
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("template %q: %w", name, err)
		}
		header := make([]byte, 24)
		_, readErr := io.ReadFull(file, header)
		closeErr := file.Close()
		if readErr != nil {
			return fmt.Errorf("template %q has an incomplete qcow2 header: %w", name, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("template %q: %w", name, closeErr)
		}
		version := binary.BigEndian.Uint32(header[4:8])
		if !bytes.Equal(header[:4], []byte{'Q', 'F', 'I', 0xfb}) || (version != 2 && version != 3) {
			return fmt.Errorf("template %q is not a supported qcow2 image", name)
		}
		if binary.BigEndian.Uint64(header[8:16]) != 0 {
			return fmt.Errorf("template %q must not have a backing file", name)
		}
	}
	return nil
}

// Parse parses topology data. baseDir is used to resolve relative template
// paths and lookup supplies the process environment.
func Parse(data []byte, baseDir string, lookup func(string) (string, bool)) (*Config, []string, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("parse YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, nil, errors.New("topology document is empty")
	}
	var extra yaml.Node
	err := dec.Decode(&extra)
	if err != io.EOF {
		if err == nil {
			return nil, nil, errors.New("additional YAML documents are not allowed")
		}
		return nil, nil, fmt.Errorf("parse trailing YAML document: %w", err)
	}
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	warnings, err := interpolateDocument(&doc, lookupFunc(lookup))
	if err != nil {
		return nil, nil, fmt.Errorf("interpolate: %w", err)
	}
	if err := validateExplicitDefaults(&doc); err != nil {
		return nil, nil, err
	}
	if mappingValue(&doc, "host") != nil {
		return nil, nil, errors.New("legacy top-level key \"host\" is no longer supported; rename it to \"management_host\" and run down before up for any active legacy topology")
	}
	order := collectOrder(&doc)
	interpolated, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, nil, fmt.Errorf("encode interpolated YAML: %w", err)
	}
	strict := yaml.NewDecoder(bytes.NewReader(interpolated))
	strict.KnownFields(true)
	var cfg Config
	if err := strict.Decode(&cfg); err != nil {
		return nil, nil, fmt.Errorf("decode strict YAML: %w", err)
	}
	cfg.order = order
	if err := validateDecoded(&cfg); err != nil {
		return nil, nil, err
	}
	if err := normalize(&cfg, baseDir); err != nil {
		return nil, nil, err
	}
	validationWarnings, err := validate(&cfg)
	if err != nil {
		return nil, nil, err
	}
	warnings = append(warnings, validationWarnings...)
	return &cfg, warnings, nil
}

func ResolveWorkingDir(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("working directory is required (--working-dir or WORKING_DIR)")
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	abs = filepath.Clean(abs)
	resolved, err := resolveExistingPrefix(abs)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	if resolved == string(filepath.Separator) {
		return "", errors.New("working directory may not be the filesystem root")
	}
	return resolved, nil
}

func resolveExistingPrefix(path string) (string, error) {
	var suffix []string
	cur := path
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}

func validateSocketPaths(wd string, cfg *Config) error {
	const unixPathMax = 107
	for name := range cfg.VMs {
		sockets := []string{"qmp.sock", "serial.sock"}
		if cfg.VMs[name].GuestAgent {
			sockets = append(sockets, "guest-agent.sock")
		}
		for _, socket := range sockets {
			p := filepath.Join(wd, ".topolab", "topologies", cfg.Name, "nodes", name, socket)
			if len(p) > unixPathMax {
				return fmt.Errorf("working directory is too long for generated Unix socket %q (%d > %d)", p, len(p), unixPathMax)
			}
		}
	}
	return nil
}

func validateExplicitDefaults(doc *yaml.Node) error {
	zeroInvalid := map[string]bool{"mtu": true, "memory_mb": true, "queues": true, "sockets": true, "cores": true, "threads": true}
	emptyInvalid := map[string]bool{"model": true, "bus": true, "mode": true, "egress_interface": true, "source_ip": true, "lease": true}
	var walk func(*yaml.Node) error
	walk = func(n *yaml.Node) error {
		if n == nil {
			return nil
		}
		if n.Kind == yaml.DocumentNode || n.Kind == yaml.SequenceNode {
			for _, child := range n.Content {
				if err := walk(child); err != nil {
					return err
				}
			}
			return nil
		}
		if n.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i < len(n.Content); i += 2 {
			key, value := n.Content[i].Value, n.Content[i+1]
			if zeroInvalid[key] && value.Kind == yaml.ScalarNode && (value.Value == "0" || value.Value == "+0" || value.Value == "-0") {
				return fmt.Errorf("line %d: %s must be positive or omitted", value.Line, key)
			}
			if emptyInvalid[key] && value.Kind == yaml.ScalarNode && value.Tag == "!!str" && value.Value == "" {
				return fmt.Errorf("line %d: %s may not be empty", value.Line, key)
			}
			if key == "order" && value.Kind == yaml.SequenceNode && len(value.Content) == 0 {
				return fmt.Errorf("line %d: boot order may not be empty", value.Line)
			}
			if err := walk(value); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(doc)
}
