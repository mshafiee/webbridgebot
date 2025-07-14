package utils

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"webBridgeBot/internal/cache"
	"webBridgeBot/internal/types"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
)

// https://stackoverflow.com/a/70802740/15807350
func Contains[T comparable](s []T, e T) bool {
	for _, v := range s {
		if v == e {
			return true
		}
	}
	return false
}

// GetMessage fetches the message by the specified message ID
func GetMessage(ctx context.Context, client *gotgproto.Client, messageID int) (*tg.Message, error) {
	// Fetch messages using the client API
	messages, err := client.API().MessagesGetMessages(ctx, []tg.InputMessageClass{
		&tg.InputMessageID{ID: messageID},
	})
	if err != nil {
		return nil, err
	}

	// Attempt to cast the response to the expected type
	if msgs, ok := messages.(*tg.MessagesMessages); ok {
		// Iterate over the messages to find the one with the matching ID
		for _, msg := range msgs.Messages {
			if m, ok := msg.(*tg.Message); ok && m.GetID() == messageID {
				return m, nil
			}
		}
	}

	return nil, fmt.Errorf("message not found")
}

// FileFromMedia extracts file information from various tg.MessageMediaClass types.
func FileFromMedia(media tg.MessageMediaClass) (*types.DocumentFile, error) {
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

		return &types.DocumentFile{
			ID:       document.ID,
			Location: document.AsInputDocumentFileLocation(),
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
			largestSize     *tg.PhotoSize
			largestWidth    int
			largestHeight   int
			largestFileSize int64
		)
		for _, size := range photo.GetSizes() {
			if s, ok := size.(*tg.PhotoSize); ok {
				if s.W > largestWidth {
					largestWidth = s.W
					largestHeight = s.H
					largestSize = s
					largestFileSize = int64(s.Size)
				}
			}
		}
		if largestSize == nil {
			return nil, fmt.Errorf("no suitable full-size photo found for streaming")
		}
		photoFileLocation := &tg.InputPhotoFileLocation{
			ID:            photo.ID,
			AccessHash:    photo.AccessHash,
			FileReference: photo.FileReference,
			ThumbSize:     largestSize.GetType(),
		}
		fileName := fmt.Sprintf("photo_%d.jpg", photo.ID)
		mimeType := "image/jpeg"
		if largestSize != nil {
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
		}
		return &types.DocumentFile{
			ID:       photo.ID,
			Location: photoFileLocation,
			FileSize: largestFileSize,
			FileName: fileName,
			MimeType: mimeType,
			Width:    largestWidth,
			Height:   largestHeight,
			Duration: 0,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported media type: %T", media)
	}
}

func FileFromMessage(ctx context.Context, client *gotgproto.Client, messageID int) (*types.DocumentFile, error) {
	key := fmt.Sprintf("file:%d:%d", messageID, client.Self.ID)
	var cachedMedia types.DocumentFile
	err := cache.GetCache().Get(key, &cachedMedia)
	if err == nil {
		return &cachedMedia, nil
	}
	message, err := GetMessage(ctx, client, messageID)
	if err != nil {
		return nil, err
	}
	file, err := FileFromMedia(message.Media)
	if err != nil {
		return nil, err
	}
	err = cache.GetCache().Set(key, file, 3600)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func ForwardMessages(ctx *ext.Context, fromChatId int64, logChannelIdentifier string, messageID int) (*tg.Updates, error) {
	fromPeer := ctx.PeerStorage.GetInputPeerById(fromChatId)
	if fromPeer.Zero() {
		return nil, fmt.Errorf("fromChatId: %d is not a valid peer", fromChatId)
	}
	toPeer, err := GetLogChannelPeer(ctx, logChannelIdentifier)
	if err != nil {
		return nil, err
	}
	update, err := ctx.Raw.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		RandomID: []int64{rand.Int63()},
		FromPeer: fromPeer,
		ID:       []int{messageID},
		ToPeer:   toPeer,
	})
	if err != nil {
		return nil, err
	}
	return update.(*tg.Updates), nil
}

// ResolveChannelPeer resolves a peer identifier to a channel peer.
func ResolveChannelPeer(ctx *ext.Context, identifier string) (tg.InputPeerClass, error) {
	if id, err := strconv.ParseInt(identifier, 10, 64); err == nil {
		peer := ctx.PeerStorage.GetInputPeerById(id)
		if !peer.Zero() {
			switch peer.(type) {
			case *tg.InputPeerChannel, *tg.InputPeerChannelFromMessage:
				return peer, nil
			}
		}
	}
	username := identifier
	if strings.HasPrefix(username, "@") {
		username = strings.TrimPrefix(username, "@")
	}
	resolved, err := ctx.Raw.ContactsResolveUsername(context.Background(), &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, fmt.Errorf("failed to resolve username '%s': %w", username, err)
	}
	for _, chat := range resolved.Chats {
		if channel, ok := chat.(*tg.Channel); ok {
			inputPeer := &tg.InputPeerChannel{
				ChannelID:  channel.ID,
				AccessHash: channel.AccessHash,
			}
			return inputPeer, nil
		}
	}
	return nil, fmt.Errorf("no channel found for identifier '%s'", identifier)
}

func GetLogChannelPeer(ctx *ext.Context, logChannelIdentifier string) (tg.InputPeerClass, error) {
	peer, err := ResolveChannelPeer(ctx, logChannelIdentifier)
	if err != nil {
		return nil, fmt.Errorf("could not resolve log channel peer '%s': %w", logChannelIdentifier, err)
	}
	return peer, nil
}
