package core

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// DecodeJSON keeps integer platform IDs exact in persisted generic maps.
func DecodeJSON(data []byte, target any) error {
	if !json.Valid(data) {
		return fmt.Errorf("invalid JSON document")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(target)
}
