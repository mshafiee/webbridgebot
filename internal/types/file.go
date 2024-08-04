package types

import (
	"crypto/md5"
	"encoding/hex"
	"reflect"
	"strconv"

	"github.com/gotd/td/tg"
)

type DocumentFile struct {
	ID        int64
	Location  *tg.InputDocumentFileLocation
	FileSize  int64
	FileName  string
	MimeType  string
	VideoAttr tg.DocumentAttributeVideo
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
