package server

import (
	"log/slog"
	"reflect"
)

// Stats holds statistics for server operation. Can be requested with Server.GetStatistics().
type Stats struct {
	ErrorsCount           int `slog:"errors"`
	UploadCount           int `slog:"uploads"`
	ExistsYesCount        int `slog:"exists_yes"`
	ExistsNoCount         int `slog:"exists_no"`
	DownloadCount         int `slog:"downloads"`
	DownloadNotFoundCount int `slog:"downloads_not_found"`

	UploadedBytes   int64 `slog:"ul_bytes"`
	DownloadedBytes int64 `slog:"dl_bytes"`
}

// SlogArgs converts stats to an array than can be passed to slog logging functions.
// For example, slog.Info("server stats", stats.SlogArgs()...)
func (st Stats) SlogArgs() (result []any) {
	types := reflect.TypeOf(st)
	values := reflect.ValueOf(st)

	for i := 0; i < types.NumField(); i++ {
		var (
			f = types.Field(i)
			v = values.Field(i).Int()

			slogTag = f.Tag.Get("slog")
		)
		if slogTag != "" && v > 0 {
			result = append(result, slog.Int64(slogTag, v))
		}
	}
	if len(result) == 0 {
		result = []any{slog.Int("cache_requests", 0)}
	}
	return
}
