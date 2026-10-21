package jobs

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

type Request struct {
	OutputFormat string  `json:"output_format,omitempty"`
	Prompt       string  `json:"prompt"`
	Negative     string  `json:"negative_prompt"`
	Seed         uint64  `json:"seed"`
	Steps        int     `json:"steps"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	CFG          float64 `json:"cfg"`
	Exposure     float64 `json:"exposure_stops"`
	Saturation   float64 `json:"saturation"`
}

func defaults() Request {
	return Request{
		Negative:   "blurry, low quality, text, watermark, distorted",
		Seed:       42,
		Steps:      12,
		Width:      512,
		Height:     512,
		CFG:        7,
		Exposure:   0.2,
		Saturation: 0.8,
	}
}

func (r Request) valid() bool {
	if r.OutputFormat != "" && r.OutputFormat != "square" && r.OutputFormat != "portrait" {
		return false
	}

	promptLength := utf8.RuneCountInString(r.Prompt)
	if promptLength < 1 || promptLength > 2000 || utf8.RuneCountInString(r.Negative) > 2000 {
		return false
	}

	if strings.ContainsRune(r.Prompt, 0) || strings.ContainsRune(r.Negative, 0) {
		return false
	}

	if r.Seed > 9007199254740991 || r.Steps < 1 || r.Steps > 30 {
		return false
	}

	if r.Width < 256 || r.Width > 768 || r.Height < 256 || r.Height > 768 {
		return false
	}

	if r.Width%64 != 0 || r.Height%64 != 0 || r.Width*r.Height > 512*768 {
		return false
	}

	return !math.IsNaN(r.CFG) && r.CFG >= 1 && r.CFG <= 15 &&
		r.Exposure >= -2 && r.Exposure <= 2 && r.Saturation >= 0 && r.Saturation <= 2
}

// validateWorkflow checks the fixed graph before the server accepts requests.
func validateWorkflow(template []byte) error {
	var graph map[string]struct {
		Class  string         `json:"class_type"`
		Inputs map[string]any `json:"inputs"`
	}

	if len(template) > 65536 || json.Unmarshal(template, &graph) != nil {
		return fmt.Errorf("invalid workflow template")
	}

	expected := map[string]string{
		"3":  "KSampler",
		"4":  "CheckpointLoaderSimple",
		"5":  "EmptyLatentImage",
		"6":  "CLIPTextEncode",
		"7":  "CLIPTextEncode",
		"8":  "VAEDecode",
		"9":  "SaveImage",
		"10": "LabImageAdjust",
		"11": "SaveImage",
		"12": "LabCatalogFrame",
	}
	if len(graph) != len(expected) {
		return fmt.Errorf("unexpected workflow nodes")
	}

	for id, class := range expected {
		if graph[id].Class != class || graph[id].Inputs == nil {
			return fmt.Errorf("invalid template node %s", id)
		}
	}

	return nil
}

// buildWorkflow creates a fresh graph for each job; the template is immutable.
func buildWorkflow(template []byte, input Request) ([]byte, error) {
	if err := validateWorkflow(template); err != nil {
		return nil, err
	}

	graph := map[string]map[string]any{}
	if err := json.Unmarshal(template, &graph); err != nil {
		return nil, err
	}

	put := func(id, key string, value any) { graph[id]["inputs"].(map[string]any)[key] = value }
	put("6", "text", input.Prompt)
	put("7", "text", input.Negative)
	put("5", "width", input.Width)
	put("5", "height", input.Height)
	put("5", "batch_size", 1)
	put("3", "seed", input.Seed)
	put("3", "steps", input.Steps)
	put("3", "cfg", input.CFG)
	put("10", "exposure_stops", input.Exposure)
	put("10", "saturation", input.Saturation)

	format := input.OutputFormat
	if format == "" {
		format = "original"
	}

	put("12", "format", format)
	put("9", "filename_prefix", "service/adjusted")
	put("11", "filename_prefix", "service/original")

	return json.Marshal(graph)
}
