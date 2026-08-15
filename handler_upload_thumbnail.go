package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

const maxMemory = 10 << 20

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	//implement the upload here

	err = r.ParseMultipartForm(maxMemory)

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to set Multipart size", err)
		return
	}

	// "thumbnail" should match the HTML form input name
	file, header, err := r.FormFile("thumbnail")
	defer file.Close()
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}

	//extact file extension
	contentType := header.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type header", err)
		return
	}

	var ext string
	switch mediaType {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	default:
		respondWithError(w, http.StatusBadRequest, "Unsupported media type: only JPEG and PNG are allowed", nil)
		return
	}

	videoDb, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Couldn't find video", err)
		return
	}

	if videoDb.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", nil)
		return
	}

	//create destination
	assetDir := cfg.assetsRoot
	if assetDir == "" {
		assetDir = "assets"
	}

	//random name to avoid caching

	key := make([]byte, 32)
	_, err = rand.Read(key)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Not create random byte", err)
		return
	}

	// Only encode the key once we know rand.Read succeeded
	encodedString := base64.RawURLEncoding.EncodeToString(key)

	fileName := fmt.Sprintf("%s%s", encodedString, ext)
	dstPath := filepath.Join(assetDir, fileName)

	// Ensure ./assets exists
	if err := os.MkdirAll(assetDir, 0755); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create directory", err)
		return
	}

	// 3. Create destination file
	dstFile, err := os.Create(dstPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create file", err)
		return
	}
	defer dstFile.Close()

	// Stream bytes directly to disk but we copy from http not byte
	if _, err := io.Copy(dstFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to write file to disk", err)
		return
	}

	thumbnailURL := fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, fileName)

	videoDb.ThumbnailURL = &thumbnailURL
	err = cfg.db.UpdateVideo(videoDb)

	if err != nil {
		respondWithError(w, http.StatusNotFound, "Couldn't update video from database", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoDb)
}
