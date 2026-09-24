// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import "encoding/json"

// The media types a manifest travels in. The server decodes both syntaxes to
// one object, so a caller sends the bytes it wrote in the syntax it wrote
// them in.
const (
	MediaJSON = "application/json"
	MediaYAML = "application/yaml"
)

// Manifest is a document to create or apply: its bytes and the media type
// they are written in.
type Manifest struct {
	Body []byte
	// ContentType is the body's media type, MediaJSON when empty.
	ContentType string
}

// JSON is a manifest written in JSON.
func JSON(body []byte) Manifest { return Manifest{Body: body, ContentType: MediaJSON} }

// YAML is a manifest written in YAML.
func YAML(body []byte) Manifest { return Manifest{Body: body, ContentType: MediaYAML} }

// Encode is the manifest of a typed object, such as a v1.Sandbox, encoded as
// JSON.
func Encode(obj any) (Manifest, error) {
	body, err := json.Marshal(obj)
	if err != nil {
		return Manifest{}, err
	}
	return JSON(body), nil
}

// mediaType is the Content-Type the manifest is sent under.
func (m Manifest) mediaType() string {
	if m.ContentType == "" {
		return MediaJSON
	}
	return m.ContentType
}
