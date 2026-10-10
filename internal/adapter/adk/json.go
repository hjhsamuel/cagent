package adk

import (
	"bytes"
	"encoding/json"
	"io"
)

// decodeJSON preserves arbitrary JSON numbers through application records and restart.
func decodeJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return invalid("json.trailing_data")
	}
	return nil
}
