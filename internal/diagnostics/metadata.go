package diagnostics

import (
	"encoding/json"
)

type MalformedBodyMetadata struct {
	Malformed   bool   `json:"malformed"`
	ContentType string `json:"content_type,omitempty"`
	Length      int    `json:"length"`
	Error       string `json:"error,omitempty"`
	Preview     string `json:"preview,omitempty"`
}

func DescribeMalformedBody(body []byte, contentType string, parseErr error) MalformedBodyMetadata {
	metadata := MalformedBodyMetadata{
		Malformed:   true,
		ContentType: contentType,
		Length:      len(body),
	}
	if parseErr != nil {
		metadata.Error = parseErr.Error()
	}
	return metadata
}

func MalformedBodyJSON(body []byte, contentType string, parseErr error) json.RawMessage {
	encoded, err := json.Marshal(DescribeMalformedBody(body, contentType, parseErr))
	if err != nil {
		return nil
	}
	return encoded
}
