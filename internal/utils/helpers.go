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
	"github.com/celestix/gotgproto/storage"
	"github.com/gotd/td/tg"
)

// Contains checks if a slice contains a specific element.
// Source: https://stackoverflow.com/a/70802740/15807350
func Contains[T comparable](s []T, e T) bool {
	for _, v := range s {
		if v == e {
			return true
		}
	}
	return false
}

// GetMessage fetches a message by its ID using the Telegram client.
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

// FileFromMessage retrieves file information from a message, using cache if available.
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

// ForwardMessages forwards a message from one chat to another.
func ForwardMessages(ctx *ext.Context, fromChatId int64, logChannelIdentifier string, messageID int) (*tg.Updates, error) {
	// Use ctx.PeerStorage.GetInputPeerById to retrieve the peer (corrected from storage.GetInputPeerById if it existed)
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

// ResolveChannelPeer resolves a peer identifier (ID or username) to a channel peer.
func ResolveChannelPeer(ctx *ext.Context, identifier string) (tg.InputPeerClass, error) {
	// Try parsing as a numeric ID first.
	if id, err := strconv.ParseInt(identifier, 10, 64); err == nil {
		// If it's a channel ID, resolve it via API to get the access hash and verify it.
		peerInfo := ctx.PeerStorage.GetPeerById(id)
		if peerInfo.Type == int(storage.TypeChannel) {
			// The peerInfo.ID is the negative ID, e.g., -100123456.
			// The actual channel ID for API calls is the positive part, e.g., 123456.
			// And `tg.Channel.GetID()` also returns the positive ID.
			strID := strconv.FormatInt(peerInfo.ID, 10)
			if !strings.HasPrefix(strID, "-100") {
				return nil, fmt.Errorf("peer %d is a channel but ID does not have '-100' prefix", peerInfo.ID)
			}
			bareIDStr := strings.TrimPrefix(strID, "-100")
			bareChannelID, err := strconv.ParseInt(bareIDStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("failed to parse bare channel ID from %s: %w", strID, err)
			}

			// Use the bare (positive) ID and the stored access hash for the API call.
			resolved, err := ctx.Raw.ChannelsGetChannels(ctx, []tg.InputChannelClass{
				&tg.InputChannel{ChannelID: bareChannelID, AccessHash: peerInfo.AccessHash},
			})
			if err != nil {
				return nil, fmt.Errorf("failed to resolve channel ID %d (bare: %d): %w", id, bareChannelID, err)
			}

			var chats []tg.ChatClass
			switch r := resolved.(type) {
			case *tg.MessagesChats:
				chats = r.GetChats()
			case *tg.MessagesChatsSlice:
				chats = r.GetChats()
			default:
				return nil, fmt.Errorf("unexpected type from ChannelsGetChannels: %T", resolved)
			}

			for _, chat := range chats {
				if ch, ok := chat.(*tg.Channel); ok && ch.GetID() == bareChannelID {
					return ch.AsInputPeer(), nil
				}
			}
			return nil, fmt.Errorf("channel ID %d resolved but could not find matching chat entity", id)
		}

		// For non-channel peers, use ctx.PeerStorage.GetInputPeerById (corrected from storage.GetInputPeerById if it existed)
		peer := ctx.PeerStorage.GetInputPeerById(id)
		if !peer.Zero() {
			return peer, nil
		}
	}

	// If not a numeric ID, treat it as a username.
	username := identifier
	if strings.HasPrefix(username, "@") {
		username = strings.TrimPrefix(username, "@")
	}
	resolved, err := ctx.Raw.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, fmt.Errorf("failed to resolve username '%s': %w", username, err)
	}
	// Look for a channel in the resolved chats.
	for _, chat := range resolved.Chats {
		if channel, ok := chat.(*tg.Channel); ok {
			return channel.AsInputPeer(), nil
		}
	}
	return nil, fmt.Errorf("no channel found for identifier '%s'", identifier)
}

// GetLogChannelPeer resolves the log channel peer using the identifier.
func GetLogChannelPeer(ctx *ext.Context, logChannelIdentifier string) (tg.InputPeerClass, error) {
	peer, err := ResolveChannelPeer(ctx, logChannelIdentifier)
	if err != nil {
		return nil, fmt.Errorf("could not resolve log channel peer '%s': %w", logChannelIdentifier, err)
	}
	return peer, nil
}
