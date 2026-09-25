package model

// Cost mirrors the CLI-normalized cost in price per 1M tokens.
type Cost struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// Limit mirrors the CLI-normalized token limits.
type Limit struct {
	Context int
	Input   int
	Output  int
}

// Modalities lists supported input/output modalities.
type Modalities struct {
	Input  []string
	Output []string
}

// Capabilities mirrors the CLI-normalized model capabilities.
type Capabilities struct {
	ToolCall         bool
	Reasoning        bool
	Attachment       bool
	Temperature      bool
	Modalities       Modalities
	InterleavedField string
}

// ReasoningOption mirrors one entry of models.dev reasoning_options.
type ReasoningOption struct {
	Type   string
	Values []string
	Min    int
	Max    int
}

// Variant is a named reasoning preset with the minimal payload the
// inference layer needs.
type Variant struct {
	ReasoningEffort string
}

// Model is the normalized internal representation every provider maps to.
type Model struct {
	ID               string
	Name             string
	Description      string
	Family           string
	Status           string
	Owner            string
	Cost             Cost
	Limit            Limit
	Capabilities     Capabilities
	ReasoningOptions []ReasoningOption
	Variants         map[string]Variant
	Transport        string
	Endpoint         string
	Created          int64
}
