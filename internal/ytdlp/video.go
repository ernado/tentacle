package ytdlp

type Fragment struct {
	URL      string  `json:"url"`
	Duration float64 `json:"duration"`
}

type Format struct {
	FormatID       string            `json:"format_id"`
	FormatNote     string            `json:"format_note"`
	Ext            string            `json:"ext"`
	Protocol       string            `json:"protocol"`
	ACodec         string            `json:"acodec"`
	VCodec         string            `json:"vcodec"`
	URL            string            `json:"url"`
	Width          int               `json:"width"`
	Height         int               `json:"height"`
	FPS            float64           `json:"fps"`
	Rows           int               `json:"rows"`
	Columns        int               `json:"columns"`
	Fragments      []Fragment        `json:"fragments"`
	AudioExt       string            `json:"audio_ext"`
	VideoExt       string            `json:"video_ext"`
	VBR            float64           `json:"vbr"`
	ABR            float64           `json:"abr"`
	TBR            float64           `json:"tbr"`
	Resolution     string            `json:"resolution"`
	AspectRatio    float64           `json:"aspect_ratio"`
	FilesizeApprox int64             `json:"filesize_approx"`
	HTTPHeaders    map[string]string `json:"http_headers"`
	Format         string            `json:"format"`
}

type Video struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Formats []Format `json:"formats"`
}
