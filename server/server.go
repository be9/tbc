package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Southclaws/fault/fctx"
	"github.com/be9/tbc/client"
	"github.com/gorilla/mux"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Options for creating a server.
type Options struct {
	Token string
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

func (s *Server) CreateHandler() http.Handler {
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

	return r
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
		s.logError(err)
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

	size, err := io.Copy(uploadedFile, r.Body)
	if err != nil {
		reportError("error saving uploaded file", err)
		return
	}

	err = uploadedFile.Close()
	if err != nil {
		reportError("error closing uploaded file", err)
		return
	}

	err = s.cl.UploadFile(s.context(r), key, uploadedFile.Name(), collectMetadata(r.Header))
	if err != nil {
		reportError("error uploading file", err)
		return
	}

	s.stats.UploadCount++
	s.stats.UploadedBytes += size
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
	ok, err := s.cl.FindFile(s.context(r), key)

	if err != nil {
		http.Error(w, "Error looking up file", http.StatusInternalServerError)
		s.logError(err)
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
		s.logError(err)
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

	md, err := s.cl.DownloadFile(s.context(r), key, downloadedFile)
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
	s.stats.DownloadedBytes += getFileSize(downloadedFile)
	http.ServeContent(w, r, "", time.UnixMilli(0), downloadedFile)
}

func (s *Server) context(r *http.Request) context.Context {
	ctx := fctx.WithMeta(r.Context(),
		"method", r.Method,
		"url", r.URL.String(),
	)
	return ctx
}

func (s *Server) logError(err error) {
	s.stats.ErrorsCount++

	var attrs []slog.Attr
	for k, v := range fctx.Unwrap(err) {
		attrs = append(attrs, slog.String(k, v))
	}

	s.logger.LogAttrs(context.Background(), slog.LevelError, fmt.Sprintf("[tbc] %+v", err), attrs...)
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

func getFileSize(f *os.File) int64 {
	ret, _ := f.Seek(0, io.SeekEnd)
	return ret
}
