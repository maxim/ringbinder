package ocr

type mistralRequest struct {
	Model    string          `json:"model"`
	Document mistralDocument `json:"document"`
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
}

type mistralDimension struct {
	DPI    int `json:"dpi,omitempty"`
	Width  int `json:"width"`
	Height int `json:"height"`
}
