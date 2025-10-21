package types

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"reflect"
	"strconv"

	"github.com/gotd/td/tg"
)

type DocumentFile struct {
	ID         int64
	Location   tg.InputFileLocationClass // Changed to be more general (can be Document or Photo location)
	FileSize   int64
	FileName   string
	MimeType   string
	Width      int    // For video/photo
	Height     int    // For video/photo
	Duration   int    // For video/audio (in seconds)
	Title      string // For audio files
	Performer  string // For audio files (artist/performer)
	IsVoice    bool   // True if it's a voice message
	IsAnimation bool  // True if it's an animation/GIF
}

type FileMetadata struct {
	FileName string
	FileSize int64
	MimeType string
	FileID   int64
}

func (h *FileMetadata) GenerateHash() string {
	hasher := md5.New()
	val := reflect.ValueOf(*h)
	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)

		var fieldValue []byte
		switch field.Kind() {
		case reflect.String:
			fieldValue = []byte(field.String())
		case reflect.Int64:
			fieldValue = []byte(strconv.FormatInt(field.Int(), 10))
		}

		hasher.Write(fieldValue)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// FileFromMedia extracts file information from various tg.MessageMediaClass types.
func FileFromMedia(media tg.MessageMediaClass) (*DocumentFile, error) {
	switch media := media.(type) {
	case *tg.MessageMediaDocument:
		document, ok := media.Document.AsNotEmpty()
		if !ok {
			return nil, fmt.Errorf("document is empty or not a valid type")
		}

		var fileName string
		var videoWidth, videoHeight, videoDuration int
		var audioTitle, audioPerformer string
		var audioDuration int
		var isVoice, isAnimation bool

		// Extract metadata from document attributes
		for _, attribute := range document.Attributes {
			switch attr := attribute.(type) {
			case *tg.DocumentAttributeFilename:
				fileName = attr.FileName
			case *tg.DocumentAttributeVideo:
				videoWidth = attr.W
				videoHeight = attr.H
				videoDuration = int(attr.Duration)
			case *tg.DocumentAttributeAudio:
				audioDuration = int(attr.Duration)
				audioTitle = attr.Title
				audioPerformer = attr.Performer
				isVoice = attr.Voice // Voice messages have this flag set to true
			case *tg.DocumentAttributeAnimated:
				isAnimation = true
			}
		}

		// Determine the final duration (prefer video duration, then audio duration)
		finalDuration := videoDuration
		if finalDuration == 0 {
			finalDuration = audioDuration
		}

		return &DocumentFile{
			ID:          document.ID,
			Location:    document.AsInputDocumentFileLocation(), // tg.InputDocumentFileLocation implements tg.InputFileLocationClass
			FileSize:    document.Size,
			FileName:    fileName,
			MimeType:    document.MimeType,
			Width:       videoWidth,
			Height:      videoHeight,
			Duration:    finalDuration,
			Title:       audioTitle,
			Performer:   audioPerformer,
			IsVoice:     isVoice,
			IsAnimation: isAnimation,
		}, nil

	case *tg.MessageMediaPhoto:
		photo, ok := media.Photo.AsNotEmpty()
		if !ok {
			return nil, fmt.Errorf("photo is empty or not a valid type")
		}

		var (
			largestSize     *tg.PhotoSize // Will store the actual PhotoSize object
			largestWidth    int
			largestHeight   int
			largestFileSize int64 // Store the size of this specific largest PhotoSize
		)

		// Iterate to find the largest *actual* PhotoSize that has a size
		for _, size := range photo.GetSizes() {
			if s, ok := size.(*tg.PhotoSize); ok {
				if s.W > largestWidth { // Prioritize larger dimensions
					largestWidth = s.W
					largestHeight = s.H
					largestSize = s                 // Store the PhotoSize object
					largestFileSize = int64(s.Size) // Store its size
				}
			}
			// Ignoring PhotoStrippedSize and PhotoCachedSize as they are typically small previews.
		}

		if largestSize == nil {
			return nil, fmt.Errorf("no suitable full-size photo found for streaming")
		}

		// Construct InputPhotoFileLocation using the selected PhotoSize's type for ThumbSize
		photoFileLocation := &tg.InputPhotoFileLocation{
			ID:            photo.ID,
			AccessHash:    photo.AccessHash,
			FileReference: photo.FileReference,
			ThumbSize:     largestSize.GetType(), // Use the type of the largest size as ThumbSize
		}

		// Determine a filename and mimetype for photos.
		fileName := fmt.Sprintf("photo_%d.jpg", photo.ID) // Default filename
		mimeType := "image/jpeg"                          // Common mime type for photos from Telegram
		// Attempt to infer mime type from photo size type
		switch largestSize.GetType() {
		case "j":
			mimeType = "image/jpeg"
		case "p":
			mimeType = "image/png"
		case "w":
			mimeType = "image/webp"
		case "g":
			mimeType = "image/gif"
		}

		return &DocumentFile{
			ID:       photo.ID,
			Location: photoFileLocation, // tg.InputPhotoFileLocation implements tg.InputFileLocationClass
			FileSize: largestFileSize,   // Use the size of the chosen largest PhotoSize
			FileName: fileName,
			MimeType: mimeType,
			Width:    largestWidth,
			Height:   largestHeight,
			Duration: 0, // Photos have no duration
		}, nil

	default:
		return nil, fmt.Errorf("unsupported media type: %T", media)
	}
}
