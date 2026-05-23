package appmanager

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/types"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/celestix/gotgproto"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
	"google.golang.org/api/option"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
)

var (
	FirebaseApp     *firebase.App
	FirestoreClient *firestore.Client
	AuthClient      *auth.Client
	tempEmailMap    sync.Map
	initOnce        sync.Once
)

type FileSource struct {
	ChannelID int64 `firestore:"channel_id"`
	MessageID int   `firestore:"message_id"`
}

type FileDocument struct {
	ID          string       `firestore:"id"`
	FileName    string       `firestore:"file_name"`
	FileSize    int64        `firestore:"file_size"`
	MimeType    string       `firestore:"mime_type"`
	UploaderUID string       `firestore:"uploader_uid"`
	FolderID    string       `firestore:"folder_id"`
	Sources     []FileSource `firestore:"sources"`
	CreatedAt   time.Time    `firestore:"created_at"`
}

type UserDocument struct {
	UID        string    `firestore:"uid"`
	Email      string    `firestore:"email"`
	IsVerified bool      `firestore:"is_verified"`
	CreatedAt  time.Time `firestore:"created_at"`
}

func InitFirebase(log *zap.Logger) {
	initOnce.Do(func() {
		ctx := context.Background()
		credsFile := config.ValueOf.FirebaseCredsFile

		opt := option.WithCredentialsFile(credsFile)
		app, err := firebase.NewApp(ctx, nil, opt)
		if err != nil {
			fmt.Println("FIREBASE INIT ERROR:", err) // সরাসরি কনসোলে এরর প্রিন্ট করবে
			log.Fatal("Failed to initialize Firebase App", zap.Error(err))
		}
		FirebaseApp = app

		fsClient, err := app.Firestore(ctx)
		if err != nil {
			fmt.Println("FIRESTORE INIT ERROR:", err) // সরাসরি কনসোলে এরর প্রিন্ট করবে
			log.Fatal("Failed to initialize Firestore Client", zap.Error(err))
		}
		FirestoreClient = fsClient

		auClient, err := app.Auth(ctx)
		if err != nil {
			fmt.Println("FIREBASE AUTH INIT ERROR:", err) // সরাসরি কনসোলে এরর প্রিন্ট করবে
			log.Fatal("Failed to initialize Firebase Auth Client", zap.Error(err))
		}
		AuthClient = auClient

		log.Info("Successfully connected to Firebase Firestore and Authentication")
		go updateTempEmailList(log)
	})
}

// updateTempEmailList fetches blocklist from GitHub and caches domain lookup
func updateTempEmailList(log *zap.Logger) {
	url := "https://raw.githubusercontent.com/disposable-email-domains/disposable-email-domains/master/disposable_email_blocklist.conf"
	resp, err := http.Get(url)
	var data []byte
	if err != nil {
		log.Warn("Failed to fetch temp-email blocklist from GitHub, reading local backup...", zap.Error(err))
		data, err = os.ReadFile("disposable_domains.json")
		if err != nil {
			log.Error("No local disposable_domains.json backup found, temp-email checker is disabled")
			return
		}
	} else {
		defer resp.Body.Close()
		data, _ = io.ReadAll(resp.Body)
		_ = os.WriteFile("disposable_domains.json", data, 0644)
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		domain := strings.TrimSpace(line)
		if domain != "" && !strings.HasPrefix(domain, "#") {
			tempEmailMap.Store(strings.ToLower(domain), true)
		}
	}
	log.Info("Temp-Email blocker database loaded/updated successfully")
}

func IsTempEmail(email string) bool {
	parts := strings.Split(email, "@")
	if len(parts) < 2 {
		return true
	}
	domain := strings.ToLower(strings.TrimSpace(parts[1]))
	_, isTemp := tempEmailMap.Load(domain)
	return isTemp
}

func StartCleanupWorker(log *zap.Logger) {
	ticker := time.NewTicker(24 * time.Hour)
	log.Info("7-Day Trial unverified account cleanup scheduler started")
	for range ticker.C {
		ctx := context.Background()
		now := time.Now()
		sevenDaysAgo := now.AddDate(0, 0, -7)

		// Fetch all unverified users created > 7 days ago
		iter := FirestoreClient.Collection("users").
			Where("is_verified", "==", false).
			Where("created_at", "<", sevenDaysAgo).
			Documents(ctx)

		for {
			doc, err := iter.Next()
			if err != nil {
				break
			}
			var user UserDocument
			if err := doc.DataTo(&user); err == nil {
				log.Info("Starting auto-wipe for expired trial user", zap.String("uid", user.UID))
				go WipeUserAccount(ctx, user.UID, log)
			}
		}
	}
}

func WipeUserAccount(ctx context.Context, uid string, log *zap.Logger) {
	// 1. Delete all user files from DB & Telegram Channel
	fileIter := FirestoreClient.Collection("files").Where("uploader_uid", "==", uid).Documents(ctx)
	for {
		doc, err := fileIter.Next()
		if err != nil {
			break
		}
		var fileDoc FileDocument
		if err := doc.DataTo(&fileDoc); err == nil {
			// Trigger permanent Telegram message deletions
			for _, src := range fileDoc.Sources {
				DeleteTelegramMessage(src.ChannelID, src.MessageID, log)
			}
			_, _ = doc.Ref.Delete(ctx)
		}
	}

	// 2. Delete User Folders
	folderIter := FirestoreClient.Collection("folders").Where("uploader_uid", "==", uid).Documents(ctx)
	for {
		doc, err := folderIter.Next()
		if err != nil {
			break
		}
		_, _ = doc.Ref.Delete(ctx)
	}

	// 3. Delete Firebase Auth & Firestore record
	_, _ = FirestoreClient.Collection("users").Doc(uid).Delete(ctx)
	_ = AuthClient.DeleteUser(ctx, uid)
	log.Info("Account wiped out successfully due to expired verification", zap.String("uid", uid))
}

func DeleteTelegramMessage(channelID int64, messageID int, log *zap.Logger) {
	// Use your main bot client or worker raw API to delete message
	log.Info("Deleting message from Telegram channel", zap.Int64("channelID", channelID), zap.Int("messageID", messageID))
	// Implementation will invoke gotgproto delete message request inside the worker loop
}

func GetFileSourcesAndMeta(ctx context.Context, fileID string) ([]FileSource, *FileDocument, error) {
	doc, err := FirestoreClient.Collection("files").Doc(fileID).Get(ctx)
	if err != nil {
		return nil, nil, err
	}
	var fileDoc FileDocument
	if err := doc.DataTo(&fileDoc); err != nil {
		return nil, nil, err
	}
	return fileDoc.Sources, &fileDoc, nil
}

func GetFileFromCustomChannel(ctx context.Context, client *gotgproto.Client, channelID int64, messageID int) (*types.File, error) {
	// standard high performance input channel structure
	peer := &tg.InputChannel{
		ChannelID: channelID,
	}
	
	res, err := client.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: peer,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}},
	})
	if err != nil {
		return nil, err
	}
	
	messages, ok := res.(*tg.MessagesChannelMessages)
	if !ok || len(messages.Messages) == 0 {
		return nil, fmt.Errorf("message not found on channel %d", channelID)
	}
	
	msg, ok := messages.Messages[0].(*tg.Message)
	if !ok || msg.Media == nil {
		return nil, fmt.Errorf("message has no media")
	}
	
	mediaDoc, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, fmt.Errorf("media is not a document")
	}
	
	doc, ok := mediaDoc.Document.(*tg.Document)
	if !ok {
		return nil, fmt.Errorf("invalid document structure")
	}
	
	// Create direct input file location mapping
	location := &tg.InputDocumentFileLocation{
		ID:            doc.ID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
	}
	
	// Extract original filename from Telegram document attributes
	var fileName string
	for _, attr := range doc.Attributes {
		if fileAttr, ok := attr.(*tg.DocumentAttributeFilename); ok {
			fileName = fileAttr.FileName
			break
		}
	}
	
	file := &types.File{
		ID:        doc.ID,
		FileName:  fileName,
		FileSize:  doc.Size,
		MimeType:  doc.MimeType,
		Location:  location,
	}
	
	return file, nil
}
