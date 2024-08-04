package utils

import (
	"webBridgeBot/internal/types"
)

// PackFile creates a packed string from the given file details.
func PackFile(fileName string, fileSize int64, mimeType string, fileID int64) string {
	hashableFileStruct := types.FileMetadata{
		FileName: fileName,
		FileSize: fileSize,
		MimeType: mimeType,
		FileID:   fileID,
	}
	return hashableFileStruct.GenerateHash()
}

// GetShortHash returns a shortened version of the provided hash.
func GetShortHash(fullHash string, hashLength int) string {
	if len(fullHash) < hashLength {
		return fullHash
	}
	return fullHash[:hashLength]
}

func CheckHash(inputHash string, expectedHash string, hashLength int) bool {
	return inputHash == GetShortHash(expectedHash, hashLength)
}
