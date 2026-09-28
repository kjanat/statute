package cloudflare

import (
	"net/http"
	"testing"
	"time"
)

func TestRefreshAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{"default", nil, 24 * time.Hour},
		{"default age", http.Header{"Age": {"3600"}}, 23 * time.Hour},
		{"default old date", http.Header{"Date": {now.Add(-2 * time.Hour).Format(http.TimeFormat)}}, 22 * time.Hour},
		{"invalid private valid shared", http.Header{"Cache-Control": {"max-age=invalid, s-maxage=7200"}}, 2 * time.Hour},
		{"invalid controls valid expires", http.Header{"Cache-Control": {"max-age=invalid, s-maxage=-1"}, "Date": {now.Format(http.TimeFormat)}, "Expires": {now.Add(time.Hour).Format(http.TimeFormat)}}, time.Hour},
		{"zero private precedes shared", http.Header{"Cache-Control": {"max-age=0, s-maxage=7200"}}, 5 * time.Minute},
		{"private precedence", http.Header{"Cache-Control": {"s-maxage=86400, max-age=3600"}}, time.Hour},
		{"shared hint age", http.Header{"Cache-Control": {"s-maxage=86400"}, "Age": {"70965"}}, 15435 * time.Second},
		{"date age max", http.Header{"Cache-Control": {"max-age=7200"}, "Age": {"3600"}, "Date": {now.Add(-30 * time.Minute).Format(http.TimeFormat)}}, time.Hour},
		{"date older", http.Header{"Cache-Control": {"max-age=7200"}, "Age": {"60"}, "Date": {now.Add(-time.Hour).Format(http.TimeFormat)}}, time.Hour},
		{"expires", http.Header{"Date": {now.Format(http.TimeFormat)}, "Expires": {now.Add(2 * time.Hour).Format(http.TimeFormat)}}, 2 * time.Hour},
		{"expired", http.Header{"Date": {now.Format(http.TimeFormat)}, "Expires": {now.Add(-time.Hour).Format(http.TimeFormat)}}, 5 * time.Minute},
		{"lower bound", http.Header{"Cache-Control": {"max-age=0"}}, 5 * time.Minute},
		{"upper bound", http.Header{"Cache-Control": {"max-age=999999"}}, 24 * time.Hour},
		{"no cache", http.Header{"Cache-Control": {"max-age=86400, no-cache"}}, 5 * time.Minute},
		{"no store", http.Header{"Cache-Control": {"no-store"}}, 5 * time.Minute},
		{"malformed", http.Header{"Cache-Control": {"max-age=no"}}, 24 * time.Hour},
		{"overflow", http.Header{"Cache-Control": {"max-age=9999999999999999999"}}, 24 * time.Hour},
		{"duplicate", http.Header{"Cache-Control": {"max-age=300", "max-age=600"}}, 24 * time.Hour},
		{"quoted", http.Header{"Cache-Control": {`max-age="3600"`}}, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := refreshAfter(tc.header, now); got != tc.want {
				t.Errorf("refresh delay: %s, want %s", got, tc.want)
			}
		})
	}
}
