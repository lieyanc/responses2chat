package gateway

import (
	"encoding/json"
	"errors"
	"io"
)

func decodeJSONObject(input io.Reader) (map[string]any, error) {
	var value map[string]any
	decoder := json.NewDecoder(input)
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("JSON value must be an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("input contains multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}
