package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

const maxMemoryVideo = 1 << 30

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
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

	err = r.ParseMultipartForm(maxMemoryVideo)

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to set Multipart size", err)
		return
	}

	file, header, err := r.FormFile("video")

	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}

	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type header", err)
		return
	}

	var ext string
	switch mediaType {
	case "video/mp4":
		ext = ".mp4"
	default:
		respondWithError(w, http.StatusBadRequest, "Unsupported media type: only MP4 is allowed", nil)
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

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create temp file", err)
		return
	}
	// Defers are LIFO: tempFile.Close() will run first, followed by os.Remove(tempFile.Name())
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	if _, err := io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to write file to disk", err)
		return
	}

	//move file cursor to start of 1st byte, hint "io.SeekStart"
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to reset file pointer", err)
		return
	}

	//Determine the aspect ratio from the saved temporary file
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to determine video aspect ratio", err)
		return
	}

	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to process video for fast start", err)
		return
	}

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to open processed video file", err)
		return
	}

	defer os.Remove(processedFilePath)
	defer processedFile.Close()

	var prefix string
	switch aspectRatio {
	case "16:9":
		prefix = "landscape"
	case "9:16":
		prefix = "portrait"
	default:
		prefix = "other"
	}

	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to generate random key", err)
		return
	}

	key := fmt.Sprintf("%s/%s%s", prefix, hex.EncodeToString(randomBytes), ext)

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key), //there is no such directorie system , so s3 using key
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	})

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to upload file to S3", err)
		return
	}

	//update this key and bucket back to database
	video_url := fmt.Sprintf("%s,%s", cfg.s3Bucket, key)
	videoDb.VideoURL = &video_url
	err = cfg.db.UpdateVideo(videoDb)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video record", err)
		return
	}

	// Sign the video URL before returning
	signedVideo, err := cfg.dbVideoToSignedVideo(videoDb)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to sign video URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, signedVideo)
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) < 2 {
		return video, nil
	}

	bucket := parts[0]
	key := parts[1]

	presignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, 15*time.Minute)
	if err != nil {
		return video, err
	}

	video.VideoURL = &presignedURL
	return video, nil
}

type probeWidthHeight struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)

	var buf bytes.Buffer
	cmd.Stdout = &buf

	if err := cmd.Run(); err != nil {
		return "", err
	}

	var data probeWidthHeight
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		return "", err
	}

	var width, height int
	for _, s := range data.Streams {
		if s.Width > 0 && s.Height > 0 {
			width = s.Width
			height = s.Height
			break
		}
	}

	if width == 0 || height == 0 {
		return "", fmt.Errorf("no video stream found with valid dimensions")
	}

	ratio := float64(width) / float64(height)

	const target16x9 = 16.0 / 9.0 // ~1.7778
	const target9x16 = 9.0 / 16.0 // 0.5625
	const tolerance = 0.02

	if math.Abs(ratio-target16x9) <= tolerance {
		return "16:9", nil
	}
	if math.Abs(ratio-target9x16) <= tolerance {
		return "9:16", nil
	}

	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outputPath := filePath + ".processing"

	cmd := exec.Command(
		"ffmpeg",
		"-i", filePath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4",
		outputPath,
	)

	if err := cmd.Run(); err != nil {
		return "", err
	}

	return outputPath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	//Generate key not relate with our BackEnd IAM , we create permission slip from user , signed by us
	//fuctional option pattern 'https://golang.cafe/blog/golang-functional-options-pattern'
	req, err := presignClient.PresignGetObject(
		context.TODO(),
		&s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		},
		s3.WithPresignExpires(expireTime),
	)

	if err != nil {
		return "", err
	}

	return req.URL, nil
}
