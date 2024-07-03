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

type ServerOptions struct {
	Token string
}

type server struct {
	opts   ServerOptions
	cl     client.Interface
	logger *slog.Logger
}

func CreateHandler(
	client client.Interface,
	opts ServerOptions,
) (http.Handler, error) {
	r := mux.NewRouter()
	api := r.PathPrefix("/v8/artifacts").Subrouter()

	srv := &server{
		opts:   opts,
		cl:     client,
		logger: slog.With(),
	}

	if opts.Token != "" {
		api.Use(func(next http.Handler) http.Handler {
			expectedHeader := fmt.Sprintf("Bearer %s", opts.Token)

			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != expectedHeader {
					srv.logger.Error("[tbc] authorization error")

					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
			})
		})
	}

	api.HandleFunc("/events", srv.eventsHandler).Methods("POST")
	api.HandleFunc("/status", srv.statusHandler).Methods("GET")
	api.HandleFunc("/{hash}", srv.fileUploadHandler).Methods("PUT")
	api.HandleFunc("/{hash}", srv.fileCheckHandler).Methods("HEAD")
	api.HandleFunc("/{hash}", srv.fileDownloadHandler).Methods("GET")

	// TODO
	//api.HandleFunc("/", srv.multiQueryHandler).Methods("POST")

	return r, nil
}

func (*server) eventsHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (*server) statusHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	jsonBody(w, struct {
		Status string `json:"status"`
	}{
		Status: "enabled",
	})
}

func (s *server) fileUploadHandler(w http.ResponseWriter, r *http.Request) {
	key := getKey(w, r)
	if key == "" {
		return
	}

	uploadedFile, err := os.CreateTemp("", "tbc-upload-*.tmp")
	if err != nil {
		http.Error(w, "unable to upload", http.StatusInternalServerError)
		s.logger.Error("[tbc] error creating a temp file", slog.String("err", err.Error()))
		return
	}

	defer func() {
		_ = uploadedFile.Close()
		_ = os.Remove(uploadedFile.Name())
	}()

	_, err = io.Copy(uploadedFile, r.Body)
	if err != nil {
		http.Error(w, "Error saving file", http.StatusInternalServerError)
		s.logger.Error("[tbc] error saving uploaded file", slog.String("err", err.Error()))
		return
	}

	err = uploadedFile.Close()
	if err != nil {
		http.Error(w, "Error saving file", http.StatusInternalServerError)
		s.logger.Error("[tbc] error closing uploaded file", slog.String("err", err.Error()))
		return
	}

	err = s.cl.UploadFile(r.Context(), key, uploadedFile.Name(), collectMetadata(r.Header))
	if err != nil {
		http.Error(w, "Error uploading file", http.StatusInternalServerError)
		s.logger.Error("[tbc] error uploading file", slog.String("err", err.Error()))
		return
	}

	w.WriteHeader(http.StatusAccepted)
	jsonBody(w, struct {
		Urls []string `json:"urls"`
	}{})
}

func (s *server) fileCheckHandler(w http.ResponseWriter, r *http.Request) {
	key := getKey(w, r)
	if key == "" {
		return
	}
	ok, err := s.cl.FindFile(r.Context(), key)

	if err != nil {
		http.Error(w, "Error looking up file", http.StatusInternalServerError)
		s.logger.Error("[tbc] error looking up file", slog.String("err", err.Error()))
		return
	}

	if ok {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *server) fileDownloadHandler(w http.ResponseWriter, r *http.Request) {
	key := getKey(w, r)
	if key == "" {
		return
	}

	reportError := func(msg string, err error) {
		http.Error(w, "unable to download", http.StatusInternalServerError)
		s.logger.Error("[tbc] "+msg, slog.String("err", err.Error()))
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

	http.ServeContent(w, r, "", time.UnixMilli(0), downloadedFile)
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
