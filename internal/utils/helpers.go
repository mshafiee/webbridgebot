package utils

import (
	"context"
	"errors"
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
			Location: document.AsInputDocumentFileLocation(), // tg.InputDocumentFileLocation implements tg.InputFileLocationClass
			FileSize: document.Size,
			FileName: fileName,
			MimeType: document.MimeType,
			Width:    videoWidth,    // Corrected field name
			Height:   videoHeight,   // Corrected field name
			Duration: videoDuration, // Corrected field name
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

		return &types.DocumentFile{
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
	err = cache.GetCache().Set(
		key,
		file,
		3600,
	)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func ForwardMessages(ctx *ext.Context, fromChatId, logChannelID int64, messageID int) (*tg.Updates, error) {
	fromPeer := ctx.PeerStorage.GetInputPeerById(fromChatId)
	if fromPeer.Zero() {
		return nil, fmt.Errorf("fromChatId: %d is not a valid peer", fromChatId)
	}
	toPeer, err := GetLogChannelPeer(ctx, ctx.Raw, ctx.PeerStorage, logChannelID)
	if err != nil {
		return nil, err
	}
	update, err := ctx.Raw.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		RandomID: []int64{rand.Int63()},
		FromPeer: fromPeer,
		ID:       []int{messageID},
		ToPeer:   &tg.InputPeerChannel{ChannelID: toPeer.ChannelID, AccessHash: toPeer.AccessHash},
	})
	if err != nil {
		return nil, err
	}
	return update.(*tg.Updates), nil
}

func GetLogChannelPeer(ctx context.Context, api *tg.Client, peerStorage *storage.PeerStorage, logChannelID int64) (*tg.InputChannel, error) {
	cachedInputPeer := peerStorage.GetInputPeerById(logChannelID)

	switch peer := cachedInputPeer.(type) {
	case *tg.InputPeerEmpty:
		break // Cache miss, proceed to fetch from API.
	case *tg.InputPeerChannel:
		return &tg.InputChannel{
			ChannelID:  peer.ChannelID,
			AccessHash: peer.AccessHash,
		}, nil
	default:
		// A peer was found but it's not a channel, which is an error for a log channel.
		return nil, fmt.Errorf("log channel ID %d resolved to an unexpected peer type: %T", logChannelID, peer)
	}

	// If here, it's a cache miss. Fetch from API.
	// The logChannelID from config is likely a "marked" ID, e.g., -100xxxxxxxxxx.
	// The API's `channels.getChannels` call requires the bare channel ID.
	var bareChannelID int64
	if logChannelID < 0 {
		s := fmt.Sprintf("%d", logChannelID)
		if strings.HasPrefix(s, "-100") {
			bareIDStr := s[4:] // Strip the "-100" prefix
			parsedID, err := strconv.ParseInt(bareIDStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("could not parse bare channel ID from %d: %w", logChannelID, err)
			}
			bareChannelID = parsedID
		} else {
			// This handles other negative IDs, like for legacy group chats, which are not channels.
			// This should be treated as an error for a log *channel*.
			return nil, fmt.Errorf("unsupported negative ID format for a channel: %d", logChannelID)
		}
	} else {
		// Positive IDs are assumed to be bare channel IDs already.
		bareChannelID = logChannelID
	}

	inputChannel := &tg.InputChannel{
		ChannelID: bareChannelID,
	}

	channels, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{inputChannel})
	if err != nil {
		return nil, fmt.Errorf("api.ChannelsGetChannels failed for channel %d: %w", bareChannelID, err)
	}

	if len(channels.GetChats()) == 0 {
		return nil, errors.New("no channels found")
	}

	channel, ok := channels.GetChats()[0].(*tg.Channel)
	if !ok {
		return nil, fmt.Errorf("expected *tg.Channel, but got %T", channels.GetChats()[0])
	}

	// Get username to store in peer storage
	username, _ := channel.GetUsername()

	// When adding the peer to storage, we must use the original `logChannelID` as the key,
	// so that subsequent calls to `peerStorage.GetInputPeerById(logChannelID)` will succeed.
	peerStorage.AddPeer(logChannelID, channel.AccessHash, storage.TypeChannel, username)

	return channel.AsInput(), nil
}
