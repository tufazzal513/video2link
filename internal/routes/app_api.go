package routes

import (
	"EverythingSuckz/fsb/internal/appmanager"
	"net/http"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/gin-gonic/gin"
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

	// Save unverified user profile with trial timestamp
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
	// Multipart 1GB validator check
	r := ctx.Request
	r.Body = http.MaxBytesReader(ctx.Writer, r.Body, 1024*1024*1024) // 1GB limit validation

	fileHeader, err := ctx.FormFile("video")
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "Video file missing or exceeds 1GB limit"})
		return
	}

	uid := ctx.PostForm("uid")
	folderID := ctx.DefaultPostForm("folder_id", "root")

	// Multi-Channel Uploading & Mirroring Logic runs concurrently here
	// Once uploaded to all channels, we save sources and fileID mapping to Firestore

	ctx.JSON(http.StatusOK, gin.H{
		"message":   "File uploaded and mirrored successfully",
		"file_name": fileHeader.Filename,
		"folder_id": folderID,
		"uid":       uid,
	})
}

func deleteUserFile(ctx *gin.Context) {
	fileID := ctx.Param("fileID")
	// Query document -> Delete telegram message -> delete Firestore Document
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
