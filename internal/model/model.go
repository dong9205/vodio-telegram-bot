package model

type VideoMetadata struct {
	Caption             string `json:"caption,omitempty"`
	FileName            string `json:"file_name,omitempty"`
	MIMEType            string `json:"mime_type,omitempty"`
	ForwardFromName     string `json:"forward_from_name,omitempty"`
	ForwardFromUsername string `json:"forward_from_username,omitempty"`
	FileSize            int64  `json:"file_size,omitempty"`
	FileID              string `json:"-"`
	SourceType          string `json:"source_type,omitempty"`
	MediaCount          int    `json:"media_count,omitempty"`
}

type Classification struct {
	Directory string `json:"directory"`
	Title     string `json:"title"`
	Reason    string `json:"reason"`
}

func DefaultClassification() Classification {
	return Classification{
		Directory: "Unsorted",
		Title:     "telegram-video",
		Reason:    "AI classification unavailable or inconclusive",
	}
}
