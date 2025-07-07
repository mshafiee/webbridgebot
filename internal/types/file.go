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
	ID       int64
	Location tg.InputFileLocationClass // Changed to be more general (can be Document or Photo location)
	FileSize int64
	FileName string
	MimeType string
	Width    int // For video/photo
	Height   int // For video/photo
	Duration int // For video/audio (in seconds)
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
		for _, attribute := range document.Attributes {
			if name, ok := attribute.(*tg.DocumentAttributeFilename); ok {
				fileName = name.FileName
			}
			if videoAttr, ok := attribute.(*tg.DocumentAttributeVideo); ok {
				videoWidth = videoAttr.W
				videoHeight = videoAttr.H
				videoDuration = int(videoAttr.Duration)
			}
			// tg.DocumentAttributeAudio could also be parsed if specific audio metadata is needed.
		}

		return &DocumentFile{
			ID:       document.ID,
			Location: document.AsInputDocumentFileLocation(), // tg.InputDocumentFileLocation implements tg.InputFileLocationClass
			FileSize: document.Size,
			FileName: fileName,
			MimeType: document.MimeType,
			Width:    videoWidth,
			Height:   videoHeight,
			Duration: videoDuration,
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
		if largestSize != nil {
			switch largestSize.GetType() { // Attempt to infer mime type from photo size type
			case "j":
				mimeType = "image/jpeg"
			case "p":
				mimeType = "image/png"
			case "w":
				mimeType = "image/webp"
			case "g":
				mimeType = "image/gif"
			}
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
