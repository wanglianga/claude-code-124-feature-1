package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func decodeBody(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

func pathID(r *http.Request, name string) (int, bool) {
	n, err := strconv.Atoi(r.PathValue(name))
	return n, err == nil
}

// parseTime 兼容 RFC3339、"2006-01-02T15:04"、"2006-01-02 15:04"
func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, strconv.ErrSyntax
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var categories = []string{"rose", "lily", "tulip", "eustoma", "eucalyptus", "holiday_mix"}
var purposes = []string{"wedding", "retail", "livestream", "enterprise"}
var returnReasons = []string{"petal_drop", "quality", "temp_damage", "short_stem", "other"}
