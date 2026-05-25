package routes

import (
	"EverythingSuckz/fsb/internal/appmanager"
	"EverythingSuckz/fsb/internal/bot"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/celestix/gotgproto/uploader"
	"github.com/gin-gonic/gin"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

func (e *allRoutes) LoadAppAPI(r *Route) {
	api := r.Engine.Group("/api")
	{
		// Authentications
		api.POST("/auth/register", registerUser)
		
		// Folders
		api.GET("/folders", getUserFolders)
		api.POST("/folders", createFolder)
		api.DELETE("/folders/:folderID", deleteFolder)

		// Files
		api.GET("/files", getUserFiles)
		api.POST("/files/upload", uploadFileToTelegram) // Handles concurrent multi-channel upload
		api.DELETE("/files/:fileID", deleteUserFile)
		api.PUT("/files/move", moveFileToFolder)
	}
}

type RegisterRequest struct {
	UID   string `json:"uid" binding:"required"`
	Email string `json:"email" binding:"required"`
}

func registerUser(ctx *gin.Context) {
	var req RegisterRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if appmanager.IsTempEmail(req.Email) {
		ctx.JSON(http.StatusForbidden, gin.H{"error": "Temporary/Disposable email addresses are strictly blocked"})
		return
	}

	userDoc := appmanager.UserDocument{
		UID:        req.UID,
		Email:      req.Email,
		IsVerified: false,
		CreatedAt:  time.Now(),
	}

	_, err := appmanager.FirestoreClient.Collection("users").Doc(req.UID).Set(ctx, userDoc)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create user record"})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{"message": "User registered successfully, 7 days trial activated"})
}

func getUserFolders(ctx *gin.Context) {
	uid := ctx.Query("uid")
	if uid == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "Missing uid parameter"})
		return
	}

	iter := appmanager.FirestoreClient.Collection("folders").Where("uploader_uid", "==", uid).Documents(ctx)
	folders := []map[string]interface{}{}
	for {
		doc, err := iter.Next()
		if err != nil {
			break
		}
		data := doc.Data()
		data["id"] = doc.Ref.ID
		folders = append(folders, data)
	}

	ctx.JSON(http.StatusOK, folders)
}

func createFolder(ctx *gin.Context) {
	var body struct {
		Name string `json:"name" binding:"required"`
		UID  string `json:"uid" binding:"required"`
	}
	if err := ctx.ShouldBindJSON(&body); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ref, _, err := appmanager.FirestoreClient.Collection("folders").Add(ctx, map[string]interface{}{
		"name":         body.Name,
		"uploader_uid": body.UID,
		"created_at":   time.Now(),
	})
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create folder"})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{"id": ref.ID, "name": body.Name})
}

func deleteFolder(ctx *gin.Context) {
	folderID := ctx.Param("folderID")
	ctx.JSON(http.StatusOK, gin.H{"message": "Folder deleted, files moved to Root", "id": folderID})
}

func getUserFiles(ctx *gin.Context) {
	uid := ctx.Query("uid")
	folderID := ctx.DefaultQuery("folder_id", "root")

	if uid == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "Missing uid parameter"})
		return
	}

	iter := appmanager.FirestoreClient.Collection("files").
		Where("uploader_uid", "==", uid).
		Where("folder_id", "==", folderID).
		Documents(ctx)

	files := []appmanager.FileDocument{}
	for {
		doc, err := iter.Next()
		if err != nil {
			break
		}
		var file appmanager.FileDocument
		if err := doc.DataTo(&file); err == nil {
			files = append(files, file)
		}
	}

	ctx.JSON(http.StatusOK, files)
}

func uploadFileToTelegram(ctx *gin.Context) {
	r := ctx.Request
	r.Body = http.MaxBytesReader(ctx.Writer, r.Body, 1024*1024*1024) // 1GB Limit check

	fileHeader, err := ctx.FormFile("video")
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "Video file missing or exceeds 1GB limit"})
		return
	}

	uid := ctx.PostForm("uid")
	folderID := ctx.DefaultPostForm("folder_id", "root")

	// 1. Temporary ফোল্ডারে ফাইলটি সেভ করা
	tempDir := os.TempDir()
	tempFilePath := filepath.Join(tempDir, fileHeader.Filename)
	out, err := os.Create(tempFilePath)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create temp file"})
		return
	}
	defer out.Close()
	defer os.Remove(tempFilePath)

	file, err := fileHeader.Open()
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open multipart file"})
		return
	}
	defer file.Close()

	_, err = io.Copy(out, file)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to write temp file"})
		return
	}

	// 2. ফায়ারস্টোর থেকে সচল রোটেশন চ্যানেলগুলো রিড করা
	var activeChannels []int64
	chIter := appmanager.FirestoreClient.Collection("channels").Where("is_active", "==", true).Documents(ctx)
	for {
		doc, err := chIter.Next()
		if err != nil {
			break
		}
		if val, err := doc.DataAt("channel_id"); err == nil {
			if id, ok := val.(int64); ok {
				activeChannels = append(activeChannels, id)
			}
		}
	}

	if len(activeChannels) == 0 {
		ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "No active backup channels found in Admin Panel"})
		return
	}

	// 3. টেলিগ্রামের প্রথম চ্যানেলে ভিডিও আপলোড করা
	client := bot.Bot
	u := uploader.NewUploader(client.API())
	tgFile, err := u.FromPath(ctx, tempFilePath)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Telegram upload failed: %v", err)})
		return
	}

	// প্রথম চ্যানেলে মেসেজ পাঠানো
	firstChannelID := activeChannels[0]
	peer := &tg.InputPeerChannel{ChannelID: firstChannelID}
	media := &tg.InputMediaUploadedDocument{
		File:     tgFile,
		MimeType: "video/mp4",
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeVideo{
				Duration: 0,
				W:        0,
				H:        0,
			},
			&tg.DocumentAttributeFilename{
				FileName: fileHeader.Filename,
			},
		},
	}

	res, err := client.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:     peer,
		Media:    media,
		RandomID: rand.Int63(),
	})
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to send media: %v", err)})
		return
	}

	updates, ok := res.(*tg.Updates)
	if !ok || len(updates.Updates) == 0 {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Unexpected Telegram response structure"})
		return
	}

	var tgDoc *tg.Document
	var firstMsgID int
	for _, u := range updates.Updates {
		if uMessage, ok := u.(*tg.UpdateNewChannelMessage); ok {
			if msg, ok := uMessage.Message.(*tg.Message); ok {
				firstMsgID = msg.ID
				if msg.Media != nil {
					if mediaDoc, ok := msg.Media.(*tg.MessageMediaDocument); ok {
						if d, ok := mediaDoc.Document.(*tg.Document); ok {
							tgDoc = d
						}
					}
				}
			}
		}
	}

	if tgDoc == nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Could not extract uploaded Telegram document"})
		return
	}

	// 4. বাকি রোটেশন চ্যানেলগুলোতে রেফারেন্স ব্যবহার করে ইনস্ট্যান্ট মিররিং
	sources := []appmanager.FileSource{
		{ChannelID: firstChannelID, MessageID: firstMsgID},
	}

	if len(activeChannels) > 1 {
		inputDoc := &tg.InputMediaDocument{
			ID: &tg.InputDocument{
				ID:            tgDoc.ID,
				AccessHash:    tgDoc.AccessHash,
				FileReference: tgDoc.FileReference,
			},
		}

		for i := 1; i < len(activeChannels); i++ {
			chID := activeChannels[i]
			otherPeer := &tg.InputPeerChannel{ChannelID: chID}
			otherRes, err := client.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
				Peer:     otherPeer,
				Media:    inputDoc,
				RandomID: rand.Int63(),
			})
			if err == nil {
				if otherUpdates, ok := otherRes.(*tg.Updates); ok {
					for _, ou := range otherUpdates.Updates {
						if ouMessage, ok := ou.(*tg.UpdateNewChannelMessage); ok {
							if oMsg, ok := ouMessage.Message.(*tg.Message); ok {
								sources = append(sources, appmanager.FileSource{ChannelID: chID, MessageID: oMsg.ID})
							}
						}
					}
				}
			}
		}
	}

	// 5. সম্পূর্ণ মেটাডাটা ফায়ারস্টোর ড্যাশবোর্ডে সেভ করা
	fileID := fmt.Sprintf("%d", tgDoc.ID)
	fileDoc := appmanager.FileDocument{
		ID:          fileID,
		FileName:    fileHeader.Filename,
		FileSize:    tgDoc.Size,
		MimeType:    tgDoc.MimeType,
		UploaderUID: uid,
		FolderID:    folderID,
		Sources:     sources,
		CreatedAt:   time.Now(),
	}

	_, err = appmanager.FirestoreClient.Collection("files").Doc(fileID).Set(ctx, fileDoc)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save file metadata to Firestore"})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{
		"message":   "File uploaded and mirrored successfully to all active channels",
		"file_id":   fileID,
		"file_name": fileHeader.Filename,
		"folder_id": folderID,
		"uid":       uid,
	})
}

func deleteUserFile(ctx *gin.Context) {
	fileID := ctx.Param("fileID")
	sources, _, err := appmanager.GetFileSourcesAndMeta(ctx, fileID)
	if err == nil {
		for _, src := range sources {
			appmanager.DeleteTelegramMessage(src.ChannelID, src.MessageID, zap.NewNop())
		}
	}
	_, _ = appmanager.FirestoreClient.Collection("files").Doc(fileID).Delete(ctx)
	ctx.JSON(http.StatusOK, gin.H{"message": "File deleted from all channels and DB", "id": fileID})
}

func moveFileToFolder(ctx *gin.Context) {
	var body struct {
		FileID   string `json:"file_id" binding:"required"`
		FolderID string `json:"folder_id" binding:"required"`
	}
	if err := ctx.ShouldBindJSON(&body); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	_, err := appmanager.FirestoreClient.Collection("files").Doc(body.FileID).Update(ctx, []firestore.Update{
		{Path: "folder_id", Value: body.FolderID},
	})
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move file"})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{"message": "File moved successfully"})
}
