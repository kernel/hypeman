package builds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// encoding/json otherwise silently accepts duplicate keys and null strings.
// Reject both so a recipe has one unambiguous interpretation.
func validateMachineRecipeJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 8 {
			return fmt.Errorf("machine recipe nesting exceeds limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("invalid machine recipe JSON")
		}
		if token == nil {
			return fmt.Errorf("machine recipe does not support null")
		}
		switch token {
		case json.Delim('{'):
			seen := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return fmt.Errorf("invalid machine recipe object")
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return fmt.Errorf("duplicate or invalid machine recipe key")
				}
				// Struct decoding matches keys case-insensitively; the recipe
				// contract accepts only the exact documented field names.
				if (depth == 0 && name != "version" && name != "steps") || (depth == 2 && name != "command" && name != "env") {
					return fmt.Errorf("unknown machine recipe field")
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case json.Delim('['):
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("recipe must contain one JSON object")
	}
	return nil
}
