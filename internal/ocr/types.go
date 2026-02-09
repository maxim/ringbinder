package ocr

import (
	"encoding/json"
	"strings"
)

type mistralRequest struct {
	Model                string               `json:"model"`
	Document             mistralDocument      `json:"document"`
	BBoxAnnotationFormat bboxAnnotationFormat `json:"bbox_annotation_format"`
}

type mistralDocument struct {
	Type        string `json:"type"`
	DocumentURL string `json:"document_url,omitempty"`
	ImageURL    string `json:"image_url,omitempty"`
}

type mistralResponse struct {
	Pages []mistralPage `json:"pages"`
	Model string        `json:"model"`
}

type mistralPage struct {
	Index      int               `json:"index"`
	Markdown   string            `json:"markdown"`
	Dimensions *mistralDimension `json:"dimensions,omitempty"`
	Images     []mistralImage    `json:"images,omitempty"`
}

type mistralDimension struct {
	DPI    int `json:"dpi,omitempty"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type bboxAnnotationFormat struct {
	Type       string            `json:"type"`
	JSONSchema bboxJSONSchemaDef `json:"json_schema"`
}

type bboxJSONSchemaDef struct {
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	Strict      bool                 `json:"strict"`
	Schema      bboxSchemaDefinition `json:"schema"`
}

type bboxSchemaDefinition struct {
	Type                 string                     `json:"type"`
	Properties           map[string]bboxPropertyDef `json:"properties"`
	Required             []string                   `json:"required,omitempty"`
	AdditionalProperties bool                       `json:"additionalProperties"`
}

type bboxPropertyDef struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

type mistralImage struct {
	ID              string          `json:"id"`
	TopLeftX        int             `json:"top_left_x"`
	TopLeftY        int             `json:"top_left_y"`
	BottomRightX    int             `json:"bottom_right_x"`
	BottomRightY    int             `json:"bottom_right_y"`
	ImageAnnotation imageAnnotation `json:"image_annotation"`
}

type imageAnnotation struct {
	ImageType   string `json:"image_type"`
	Description string `json:"description"`
}

func (a *imageAnnotation) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*a = imageAnnotation{}
		return nil
	}

	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}

		s = strings.TrimSpace(s)
		if s == "" {
			*a = imageAnnotation{}
			return nil
		}

		// Some OCR responses serialize the annotation object as a JSON string.
		if strings.HasPrefix(s, "{") {
			var parsed imageAnnotation
			if err := json.Unmarshal([]byte(s), &parsed); err == nil {
				*a = parsed
				return nil
			}
		}

		*a = imageAnnotation{Description: s}
		return nil
	}

	type alias imageAnnotation
	var parsed alias
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*a = imageAnnotation(parsed)
	return nil
}
