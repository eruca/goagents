package paddleocr

type OCRResponse struct {
	LogID     string    `json:"logId"`
	Result    OCRResult `json:"result"`
	ErrorCode int       `json:"errorCode"`
	ErrorMsg  string    `json:"errorMsg"`
}

type OCRResult struct {
	LayoutParsingResults []LayoutParsingResult `json:"layoutParsingResults"`
	DataInfo             DataInfo              `json:"dataInfo"`
	PreprocessedImages   []string              `json:"preprocessedImages"`
}

type LayoutParsingResult struct {
	PrunedResult PrunedResult      `json:"prunedResult"`
	Markdown     MarkdownResult    `json:"markdown"`
	OutputImages map[string]string `json:"outputImages"`
	InputImage   string            `json:"inputImage"`
}

type PrunedResult struct {
	PageCount          int                `json:"page_count"`
	Width              int                `json:"width"`
	Height             int                `json:"height"`
	ModelSettings      ModelSettings      `json:"model_settings"`
	ParsingResList     []ParsingBlock     `json:"parsing_res_list"`
	DocPreprocessorRes DocPreprocessorRes `json:"doc_preprocessor_res"`
	LayoutDetRes       LayoutDetectionRes `json:"layout_det_res"`
}

type ModelSettings struct {
	UseDocPreprocessor   bool     `json:"use_doc_preprocessor"`
	UseLayoutDetection   bool     `json:"use_layout_detection"`
	UseChartRecognition  bool     `json:"use_chart_recognition"`
	UseSealRecognition   bool     `json:"use_seal_recognition"`
	UseOCRForImageBlock  bool     `json:"use_ocr_for_image_block"`
	FormatBlockContent   bool     `json:"format_block_content"`
	MergeLayoutBlocks    bool     `json:"merge_layout_blocks"`
	MarkdownIgnoreLabels []string `json:"markdown_ignore_labels"`
}

type ParsingBlock struct {
	BlockLabel   string `json:"block_label"`
	BlockContent string `json:"block_content"`
	BlockBBox    []int  `json:"block_bbox"`
	BlockID      int    `json:"block_id"`
	BlockOrder   *int   `json:"block_order"`
	GroupID      int    `json:"group_id"`
}

type DocPreprocessorRes struct {
	ModelSettings DocPreprocessorSettings `json:"model_settings"`
	Angle         int                     `json:"angle"`
}

type DocPreprocessorSettings struct {
	UseDocOrientationClassify bool `json:"use_doc_orientation_classify"`
	UseDocUnwarping           bool `json:"use_doc_unwarping"`
}

type LayoutDetectionRes struct {
	Boxes []LayoutBox `json:"boxes"`
}

type LayoutBox struct {
	ClsID      int     `json:"cls_id"`
	Label      string  `json:"label"`
	Score      float64 `json:"score"`
	Coordinate []int   `json:"coordinate"`
	Order      *int    `json:"order"`
}

type MarkdownResult struct {
	Text   string            `json:"text"`
	Images map[string]string `json:"images"`
}

type DataInfo struct {
	NumPages int        `json:"numPages"`
	Pages    []PageInfo `json:"pages"`
	Type     string     `json:"type"`
}

type PageInfo struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}
