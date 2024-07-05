package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/be9/tbc/client"
	"github.com/gorilla/mux"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Options for creating a server.
type Options struct {
	Token string
}

// Stats holds statistics for server operation. Can be requested with Server.GetStatistics().
type Stats struct {
	ErrorsCount           int `json:"errors,omitempty"`
	UploadCount           int `json:"uploads,omitempty"`
	ExistsYesCount        int `json:"exists_yes,omitempty"`
	ExistsNoCount         int `json:"exists_no,omitempty"`
	DownloadCount         int `json:"downloads,omitempty"`
	DownloadNotFoundCount int `json:"download_not_found,omitempty"`
}

type Server struct {
	opts   Options
	cl     client.Interface
	logger *slog.Logger

	stats Stats
}

func NewServer(client client.Interface, opts Options) *Server {
	return &Server{
		opts:   opts,
		cl:     client,
		logger: slog.With(),
	}
}

func (s *Server) CreateHandler() (http.Handler, error) {
	r := mux.NewRouter()
	api := r.PathPrefix("/v8/artifacts").Subrouter()

	if s.opts.Token != "" {
		api.Use(func(next http.Handler) http.Handler {
			expectedHeader := fmt.Sprintf("Bearer %s", s.opts.Token)

			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != expectedHeader {
					s.logger.Error("[tbc] authorization error")

					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
			})
		})
	}

	api.HandleFunc("/events", s.eventsHandler).Methods("POST")
	api.HandleFunc("/status", s.statusHandler).Methods("GET")
	api.HandleFunc("/{hash}", s.uploadArtifactHandler).Methods("PUT")
	api.HandleFunc("/{hash}", s.artifactExistsHandler).Methods("HEAD")
	api.HandleFunc("/{hash}", s.downloadArtifactHandler).Methods("GET")

	return r, nil
}

func (*Server) eventsHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (*Server) statusHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	jsonBody(w, struct {
		Status string `json:"status"`
	}{
		Status: "enabled",
	})
}

func (s *Server) uploadArtifactHandler(w http.ResponseWriter, r *http.Request) {
	key := getKey(w, r)
	if key == "" {
		return
	}

	reportError := func(msg string, err error) {
		http.Error(w, "unable to upload", http.StatusInternalServerError)
		s.logger.Error("[tbc] "+msg, slog.String("err", err.Error()))
		s.stats.ErrorsCount++
	}

	uploadedFile, err := os.CreateTemp("", "tbc-upload-*.tmp")
	if err != nil {
		reportError("error creating a temp file", err)
		return
	}

	defer func() {
		_ = uploadedFile.Close()
		_ = os.Remove(uploadedFile.Name())
	}()

	_, err = io.Copy(uploadedFile, r.Body)
	if err != nil {
		reportError("error saving uploaded file", err)
		return
	}

	err = uploadedFile.Close()
	if err != nil {
		reportError("error closing uploaded file", err)
		return
	}

	err = s.cl.UploadFile(r.Context(), key, uploadedFile.Name(), collectMetadata(r.Header))
	if err != nil {
		reportError("error uploading file", err)
		return
	}

	s.stats.UploadCount++
	w.WriteHeader(http.StatusAccepted)
	jsonBody(w, struct {
		Urls []string `json:"urls"`
	}{})
}

func (s *Server) artifactExistsHandler(w http.ResponseWriter, r *http.Request) {
	key := getKey(w, r)
	if key == "" {
		return
	}
	ok, err := s.cl.FindFile(r.Context(), key)

	if err != nil {
		http.Error(w, "Error looking up file", http.StatusInternalServerError)
		s.logger.Error("[tbc] error looking up file", slog.String("err", err.Error()))
		s.stats.ErrorsCount++
		return
	}

	if ok {
		s.stats.ExistsYesCount++
		w.WriteHeader(http.StatusOK)
	} else {
		s.stats.ExistsNoCount++
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *Server) downloadArtifactHandler(w http.ResponseWriter, r *http.Request) {
	key := getKey(w, r)
	if key == "" {
		return
	}

	reportError := func(msg string, err error) {
		http.Error(w, "unable to download", http.StatusInternalServerError)
		s.logger.Error("[tbc] "+msg, slog.String("err", err.Error()))
		s.stats.ErrorsCount++
	}

	downloadedFile, err := os.CreateTemp("", "tbc-download-*.tmp")
	if err != nil {
		reportError("error creating a temp file", err)
		return
	}

	defer func() {
		_ = downloadedFile.Close()
		_ = os.Remove(downloadedFile.Name())
	}()

	md, err := s.cl.DownloadFile(r.Context(), key, downloadedFile)
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
			http.Error(w, "key not found", http.StatusNotFound)
			s.stats.DownloadNotFoundCount++
			return
		}

		reportError("error downloading file from the remote cache", err)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	for k, v := range md {
		if v, ok := v.(string); ok {
			w.Header().Set(k, v)
		}
	}

	s.stats.DownloadCount++
	http.ServeContent(w, r, "", time.UnixMilli(0), downloadedFile)
}

func (s *Server) GetStatistics() Stats {
	return s.stats
}

func (s *Server) ResetStatistics() {
	s.stats = Stats{}
}

func jsonBody(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func getKey(w http.ResponseWriter, r *http.Request) string {
	hash := mux.Vars(r)["hash"]

	// Sanity check
	if hash == "" {
		http.Error(w, "bad hash", http.StatusInternalServerError)
		return ""
	}

	query := r.URL.Query()
	keyParts := []string{hash}
	if query.Has("teamId") {
		keyParts = append([]string{query.Get("teamId")}, keyParts...)
	}
	if query.Has("slug") {
		keyParts = append([]string{query.Get("slug")}, keyParts...)
	}
	return strings.Join(keyParts, "/")
}

var headersForMetadata = []string{
	"x-artifact-duration",
	"x-artifact-tag",
}

func collectMetadata(h http.Header) (md client.Metadata) {
	for _, hdr := range headersForMetadata {
		if v := h.Get(hdr); v != "" {
			if md == nil {
				md = make(client.Metadata)
			}
			md[hdr] = v
		}
	}
	return
}
