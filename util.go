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

// 花材敏感度等级（冷库爆满时优先入库的第一依据）：高敏感花材离冷库升温后开放/衰败最快
var sensitivityLevels = []string{"high", "medium", "low"}

// 品类默认敏感度：玫瑰/洋桔梗/郁金香最娇贵，百合/节日混搭次之，尤加利最耐放
var categorySensitivity = map[string]string{
	"rose": "high", "eustoma": "high", "tulip": "high",
	"lily": "medium", "holiday_mix": "medium",
	"eucalyptus": "low",
}

// 临时保温方式
var holdingMethods = []string{"ice_box", "refrigerated_truck", "insulated_cover", "cool_room", "other"}

// 损耗责任承担方
var responsibilityParties = []string{"coldstore", "wholesaler", "farmer", "broker"}

// 凌晨到货时段（入场顺序调整粒度）
var arrivalTimeSlots = []string{"00:00-02:00", "02:00-04:00", "04:00-06:00", "06:00-08:00"}
