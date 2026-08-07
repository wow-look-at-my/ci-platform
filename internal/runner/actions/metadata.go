package actions

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Metadata is a parsed action.yml.
type Metadata struct {
	Name        string
	Description string
	Inputs      map[string]Input
	// InputOrder preserves declaration order so INPUT_ env vars and error
	// messages are deterministic.
	InputOrder []string
	Outputs    map[string]Output
	Runs       Runs
}

// Input is one declared input.
type Input struct {
	Description        string
	Default            string
	HasDefault         bool
	Required           bool
	DeprecationMessage string
}

// Output is one declared output.
type Output struct {
	Description string
	// Value is the ${{ }} expression a composite action computes its output
	// from; empty for JavaScript actions, which write to $GITHUB_OUTPUT.
	Value string
}

// Runs is the runs: block.
type Runs struct {
	Using  string
	Main   string
	Pre    string
	PreIf  string
	Post   string
	PostIf string
	Steps  []CompositeStep

	// Docker action fields, parsed so the unsupported failure can name what it
	// found rather than reporting an empty runs: block.
	Image      string
	Entrypoint string
	Args       []string
	Env        map[string]string
}

// CompositeStep is one step of a composite action's runs.steps.
type CompositeStep struct {
	ID               string
	Name             string
	If               string
	Run              string
	Uses             string
	Shell            string
	WorkingDirectory string
	With             map[string]string
	Env              map[string]string
	ContinueOnError  bool
	TimeoutMinutes   int
}

// IsJavaScript reports whether runs.using names a node runtime.
func (r Runs) IsJavaScript() bool { return r.NodeVersion() != "" }

// NodeVersion returns the node major version for node20/node24, "" otherwise.
func (r Runs) NodeVersion() string {
	switch strings.ToLower(r.Using) {
	case "node20":
		return "20"
	case "node24":
		return "24"
	}
	return ""
}

// IsComposite reports whether this is a composite action.
func (r Runs) IsComposite() bool { return strings.EqualFold(r.Using, "composite") }

// IsDocker reports whether this is a container action.
func (r Runs) IsDocker() bool {
	u := strings.ToLower(r.Using)
	return u == "docker" || strings.HasPrefix(u, "docker")
}

// rawMetadata mirrors the YAML shape; the exported form is normalized.
type rawMetadata struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Inputs      yaml.Node `yaml:"inputs"`
	Outputs     yaml.Node `yaml:"outputs"`
	Runs        rawRuns   `yaml:"runs"`
}

type rawRuns struct {
	Using      string            `yaml:"using"`
	Main       string            `yaml:"main"`
	Pre        string            `yaml:"pre"`
	PreIf      string            `yaml:"pre-if"`
	Post       string            `yaml:"post"`
	PostIf     string            `yaml:"post-if"`
	Steps      []rawStep         `yaml:"steps"`
	Image      string            `yaml:"image"`
	Entrypoint string            `yaml:"entrypoint"`
	Args       []string          `yaml:"args"`
	Env        map[string]string `yaml:"env"`
}

type rawStep struct {
	ID               string            `yaml:"id"`
	Name             string            `yaml:"name"`
	If               string            `yaml:"if"`
	Run              string            `yaml:"run"`
	Uses             string            `yaml:"uses"`
	Shell            string            `yaml:"shell"`
	WorkingDirectory string            `yaml:"working-directory"`
	With             map[string]string `yaml:"with"`
	Env              map[string]string `yaml:"env"`
	ContinueOnError  bool              `yaml:"continue-on-error"`
	TimeoutMinutes   int               `yaml:"timeout-minutes"`
}

type rawInput struct {
	Description        string    `yaml:"description"`
	Default            yaml.Node `yaml:"default"`
	Required           bool      `yaml:"required"`
	DeprecationMessage string    `yaml:"deprecationMessage"`
}

type rawOutput struct {
	Description string `yaml:"description"`
	Value       string `yaml:"value"`
}

// ParseMetadata parses an action.yml. Every failure names the file's defect;
// an action whose runs: block is missing is rejected rather than treated as a
// no-op step.
func ParseMetadata(data []byte) (*Metadata, error) {
	var raw rawMetadata
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("action.yml: %w", err)
	}
	m := &Metadata{
		Name:        raw.Name,
		Description: raw.Description,
		Inputs:      map[string]Input{},
		Outputs:     map[string]Output{},
		Runs: Runs{
			Using:      raw.Runs.Using,
			Main:       raw.Runs.Main,
			Pre:        raw.Runs.Pre,
			PreIf:      raw.Runs.PreIf,
			Post:       raw.Runs.Post,
			PostIf:     raw.Runs.PostIf,
			Image:      raw.Runs.Image,
			Entrypoint: raw.Runs.Entrypoint,
			Args:       raw.Runs.Args,
			Env:        raw.Runs.Env,
		},
	}
	if err := decodeInputs(&raw.Inputs, m); err != nil {
		return nil, err
	}
	if err := decodeOutputs(&raw.Outputs, m); err != nil {
		return nil, err
	}
	for _, s := range raw.Runs.Steps {
		m.Runs.Steps = append(m.Runs.Steps, CompositeStep{
			ID: s.ID, Name: s.Name, If: s.If, Run: s.Run, Uses: s.Uses,
			Shell: s.Shell, WorkingDirectory: s.WorkingDirectory,
			With: s.With, Env: s.Env,
			ContinueOnError: s.ContinueOnError, TimeoutMinutes: s.TimeoutMinutes,
		})
	}
	if strings.TrimSpace(m.Runs.Using) == "" {
		return nil, fmt.Errorf("action.yml: runs.using is missing, so there is no way to execute this action")
	}
	return m, nil
}

func decodeInputs(n *yaml.Node, m *Metadata) error {
	if n == nil || n.Kind == 0 {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("action.yml: inputs must be a mapping")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		name := n.Content[i].Value
		var ri rawInput
		if err := n.Content[i+1].Decode(&ri); err != nil {
			return fmt.Errorf("action.yml: input %q: %w", name, err)
		}
		in := Input{
			Description:        ri.Description,
			Required:           ri.Required,
			DeprecationMessage: ri.DeprecationMessage,
		}
		if ri.Default.Kind != 0 {
			in.HasDefault = true
			in.Default = ri.Default.Value
		}
		m.Inputs[name] = in
		m.InputOrder = append(m.InputOrder, name)
	}
	return nil
}

func decodeOutputs(n *yaml.Node, m *Metadata) error {
	if n == nil || n.Kind == 0 {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("action.yml: outputs must be a mapping")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		name := n.Content[i].Value
		var ro rawOutput
		if err := n.Content[i+1].Decode(&ro); err != nil {
			return fmt.Errorf("action.yml: output %q: %w", name, err)
		}
		m.Outputs[name] = Output{Description: ro.Description, Value: ro.Value}
	}
	return nil
}

// InputEnv builds the INPUT_<NAME> environment for an action invocation from
// the caller's `with:` and the action's declared defaults. A required input
// with neither a value nor a default is an error naming the input, because an
// action run without it will fail in a way that does not.
func (m *Metadata) InputEnv(with map[string]string) (map[string]string, []string, error) {
	env := map[string]string{}
	var warnings []string
	seen := map[string]bool{}

	names := append([]string(nil), m.InputOrder...)
	for _, n := range names {
		seen[n] = true
	}
	extra := make([]string, 0, len(with))
	for k := range with {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	names = append(names, extra...)

	for _, name := range names {
		spec, declared := m.Inputs[name]
		value, given := with[name]
		switch {
		case given:
			if declared && spec.DeprecationMessage != "" {
				warnings = append(warnings, fmt.Sprintf("input %q is deprecated: %s", name, spec.DeprecationMessage))
			}
		case declared && spec.HasDefault:
			value = spec.Default
		case declared && spec.Required:
			return nil, warnings, fmt.Errorf("unsupported: required input %q has no value and no default", name)
		default:
			continue
		}
		env[InputEnvName(name)] = value
	}
	return env, warnings, nil
}

// InputEnvName maps an input name to its environment variable: upper-cased with
// spaces replaced by underscores, matching the Actions runner.
func InputEnvName(name string) string {
	return "INPUT_" + strings.ToUpper(strings.ReplaceAll(name, " ", "_"))
}

// InputValues resolves inputs to their plain values, for a composite action's
// ${{ inputs.x }} scope.
func (m *Metadata) InputValues(with map[string]string) (map[string]any, error) {
	env, _, err := m.InputEnv(with)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for name := range m.Inputs {
		if v, ok := env[InputEnvName(name)]; ok {
			out[name] = v
		} else {
			out[name] = ""
		}
	}
	for k, v := range with {
		if _, declared := m.Inputs[k]; !declared {
			out[k] = v
		}
	}
	return out, nil
}
