// Package tools provides the project's controlled generation actions.
// Additional tools plug into the same dependency model via engine.Tool.
package tools

import (
	"fmt"
	"os"
	"strings"
)

// Copy writes the single input to the single output, optionally prefixed
// with params["header"].
func Copy(params map[string]string, inputs, outputs []string) error {
	if len(inputs) != 1 || len(outputs) != 1 {
		return fmt.Errorf("copy: want 1 input and 1 output, got %d/%d", len(inputs), len(outputs))
	}
	data, err := os.ReadFile(inputs[0])
	if err != nil {
		return err
	}
	return os.WriteFile(outputs[0], append([]byte(params["header"]), data...), 0o644)
}

// Upper uppercases the single input into the single output.
func Upper(params map[string]string, inputs, outputs []string) error {
	if len(inputs) != 1 || len(outputs) != 1 {
		return fmt.Errorf("upper: want 1 input and 1 output, got %d/%d", len(inputs), len(outputs))
	}
	data, err := os.ReadFile(inputs[0])
	if err != nil {
		return err
	}
	return os.WriteFile(outputs[0], []byte(strings.ToUpper(string(data))), 0o644)
}

// Concat joins all inputs (in declared order) with params["sep"] (default
// "\n") into the single output.
func Concat(params map[string]string, inputs, outputs []string) error {
	if len(outputs) != 1 {
		return fmt.Errorf("concat: want 1 output, got %d", len(outputs))
	}
	sep := params["sep"]
	if sep == "" {
		sep = "\n"
	}
	parts := make([]string, len(inputs))
	for i, in := range inputs {
		data, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		parts[i] = string(data)
	}
	return os.WriteFile(outputs[0], []byte(strings.Join(parts, sep)), 0o644)
}
