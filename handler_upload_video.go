package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	defer r.Body.Close()

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

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error fetching video info", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", err)
		return
	}

	const maxMemory = 10 << 20
	r.ParseMultipartForm(maxMemory)

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType := header.Header.Get("Content-Type")
	mType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to map file type", err)
		return
	}

	if mType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "image can only be jpeg or png", err)
		return
	}

	f, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create temp file", err)
		return
	}
	defer os.Remove(f.Name())

	_, err = io.Copy(f, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error writing to temp file", err)
		return
	}

	f.Seek(0, io.SeekStart)

	processedVideoPath, err := processVideoForFastStart(f.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to convert video", err)
		return
	}
	defer os.Remove(processedVideoPath)

	processedVideo, err := os.Open(processedVideoPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed video", err)
		return
	}
	defer processedVideo.Close()

	aspectRatio, err := getVideoAspectRatio(f.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting aspect ratio", err)
		return
	}

	var prefix string
	if aspectRatio == "9:16" {
		prefix = "portrait"
	} else if aspectRatio == "16:9" {
		prefix = "landscape"
	} else {
		prefix = "other"
	}

	key := make([]byte, 32)
	rand.Read(key)
	fileNameStr := base64.RawURLEncoding.EncodeToString(key)
	fileName := fmt.Sprintf("%s/%s%s", prefix, fileNameStr, ".mp4")

	objectInput := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileName,
		Body:        processedVideo,
		ContentType: &mType,
	}

	_, err = cfg.s3Client.PutObject(r.Context(), &objectInput)

	videoURL := fmt.Sprintf("https://%s/%s", cfg.s3CfDistribution, fileName)
	video.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error updating video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var b bytes.Buffer
	cmd.Stdout = &b

	err := cmd.Run()
	if err != nil {
		return "", err
	}

	var parsed struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	if err := json.NewDecoder(&b).Decode(&parsed); err != nil {
		return "", err
	}
	if len(parsed.Streams) == 0 {
		return "", fmt.Errorf("no streams found in %s", filePath)
	}
	w, h := parsed.Streams[0].Width, parsed.Streams[0].Height
	if h == 0 {
		return "", fmt.Errorf("stream has zero height in %s", filePath)
	}
	ratio := float64(w) / float64(h)

	const (
		ratio16x9 = 16.0 / 9.0 // ~1.7778
		ratio9x16 = 9.0 / 16.0 // ~0.5625
		tolerance = 0.30       // Allows for minor padding or rounding errors
	)

	if math.Abs(ratio-ratio16x9) < tolerance {
		return "16:9", nil
	} else if math.Abs(ratio-ratio9x16) < tolerance {
		return "9:16", nil
	}

	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	newFilePath := filePath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", newFilePath)
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return newFilePath, nil
}
